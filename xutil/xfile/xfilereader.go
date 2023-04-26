package xfile

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/ccxp/xgo/xlog"
)

// 给reader读取文件用的buffer
type BufioReaderPool interface {
	Get() *bufio.Reader
	Put(*bufio.Reader)
}

// 给reader结果用的buffer，注意由于会自动扩展，所以put回去时已经不是原来的大小
type BufferPool interface {
	Get() []byte
	Put([]byte)
}

type bufioReaderPool struct {
	p              sync.Pool
	cap            int64
	maxReserveSize int64
	reserveSize    int64
}

func (p *bufioReaderPool) Get() *bufio.Reader {

	b := p.p.Get()
	if b != nil {
		atomic.AddInt64(&p.reserveSize, -p.cap)
		return b.(*bufio.Reader)
	}

	return bufio.NewReaderSize(nil, int(p.cap))
}

func (p *bufioReaderPool) Put(b *bufio.Reader) {
	if p.maxReserveSize != 0 && atomic.LoadInt64(&p.reserveSize)+p.cap > p.maxReserveSize {
		return
	}

	p.p.Put(b)
	atomic.AddInt64(&p.reserveSize, p.cap)
}

func NewBufioReaderPool(cap int) BufioReaderPool {
	return NewBufioReaderPoolLimit(cap, 1<<30)
}

func NewBufioReaderPoolLimit(cap, maxReserveSize int) BufioReaderPool {
	return &bufioReaderPool{
		p:              sync.Pool{},
		cap:            int64(cap),
		maxReserveSize: int64(maxReserveSize),
	}
}

type bufferPool struct {
	p              sync.Pool
	initSize       int
	maxReserveSize int64
	reserveSize    int64
}

func (p *bufferPool) Get() []byte {
	b := p.p.Get()
	if b != nil {
		atomic.AddInt64(&p.reserveSize, -int64(p.initSize))
		return b.([]byte)
	}
	return make([]byte, p.initSize)
}

func (p *bufferPool) Put(b []byte) {
	l := len(b)
	if l != p.initSize ||
		(p.maxReserveSize != 0 && atomic.LoadInt64(&p.reserveSize)+int64(l) > p.maxReserveSize) {
		return
	}

	p.p.Put(b)
	atomic.AddInt64(&p.reserveSize, int64(l))
}

func NewBufferPool(initSize int) BufferPool {
	return NewBufferPoolLimit(initSize, 1<<30)
}

func NewBufferPoolLimit(initSize, maxReserveSize int) BufferPool {
	return &bufferPool{
		p:              sync.Pool{},
		initSize:       initSize,
		maxReserveSize: int64(maxReserveSize),
	}
}

// 带有缓存、校验的数据读取器，跟Writer配合使用.
type Reader struct {
	r  io.Reader
	rs io.ReadSeeker

	brpool BufioReaderPool
	br     *bufio.Reader

	bufpool BufferPool
	buf     []byte

	offset int64 // 当前offset

	maxDataLen int
}

func NewReader(r io.Reader, brPool BufioReaderPool, bufPool BufferPool) (*Reader, error) {

	res := &Reader{
		r: r,

		brpool: brPool,

		bufpool: bufPool,
		buf:     bufPool.Get(),

		maxDataLen: -1,
	}

	if rs, ok := r.(io.ReadSeeker); ok {
		offset, e := rs.Seek(0, io.SeekCurrent)
		if e != nil {
			return nil, e
		}
		res.rs = rs
		res.offset = offset
	}

	br := brPool.Get()
	br.Reset(r)
	res.br = br

	return res, nil
}

// 更新单次写入最大长度，0是无限制
func (r *Reader) SetMaxDataLen(size int) {
	r.maxDataLen = size
}

func (r *Reader) getMaxDataLen() int {
	if r.maxDataLen >= 0 {
		return r.maxDataLen
	}
	return MaxDataLen
}

var pagesize = 256

func (r *Reader) expandBuf(l int) {
	if len(r.buf) < l {
		n := len(r.buf)
		if n < pagesize {
			n = pagesize / 2
		}
		for {
			n = ((n + n/2) + pagesize - 1) / pagesize * pagesize
			if n >= l {
				break
			}
		}
		r.buf = make([]byte, n)
	}
}

var errNotSeeker = errors.New("reader is not seeker")

func (r *Reader) Seek(offset int64, whence int) (int64, error) {

	if r.rs == nil {
		switch whence {
		case io.SeekCurrent:
			if offset > 0 {
				_, e := r.br.Discard(int(offset))
				if e != nil {
					return 0, e
				}

				r.offset += offset
				return r.offset, nil
			}
		default:
		}

		return r.offset, nil
	}

	switch whence {
	case io.SeekStart:
		if r.offset >= 0 && r.offset <= offset && r.br.Buffered() >= int(offset-r.offset) {
			r.br.Discard(int(offset - r.offset))
			r.offset = offset
			return offset, nil
		}
	case io.SeekCurrent:
		if r.offset >= 0 && offset >= 0 && r.br.Buffered() >= int(offset) {
			r.br.Discard(int(offset))
			r.offset += offset
			return r.offset, nil
		}
	}

	off, e := r.rs.Seek(offset, whence)
	if e != nil {
		xlog.Errorf("seek %v %v fail: %v", offset, whence, e)
		r.resetBuf()
		r.markBad()
		return off, e
	}
	r.offset = off
	r.resetBuf()
	return off, e
}

func (r *Reader) resetBuf() {
	if n := r.br.Buffered(); n > 0 {
		r.br.Discard(n)
	}
}

func (r *Reader) markBad() {
	r.offset = -1
}

var (
	ErrBadRecord   = errors.New("bad record")
	ErrDataToLarge = errors.New("data to large") // 受MaxDataLen限制，读不出数据
)

var (
	errBadBeginCheckSum = errors.New("bad begin checksum")
	errBadEndCheckSum   = errors.New("bad end checksum")
)

// 在当前位置的取有效记录.
func (r *Reader) read(forNext bool) ([]byte, error) {

	// 要写的数据分别有
	// dataMagicBegin:    len=4, offset = 0
	// 数据长度:           len=4, offset = 4
	// reserve:           len=16, offset = 8
	// data:              offset = 24
	// 数据长度:           len=4, offset = 24 + len(data)
	// dataMagicEnd:      len=4, offset = 28 + len(data)

	oldOffset := r.offset
	tmp, e := r.br.Peek(24)
	if e != nil {
		r.markBad()
		return nil, e
	}

	if bytes.Compare(tmp[0:4], dataMagicBegin) != 0 {
		if forNext {
			// 这里br、offset是还没变.
			// 判断下1个字节
			_, e = r.Seek(1, io.SeekCurrent)
			if e != nil {
				return nil, e
			}
		}
		return nil, errBadBeginCheckSum
	}

	l := binary.BigEndian.Uint32(tmp[4:8]) // 数据长度
	if int(l) > r.getMaxDataLen() {
		if forNext {
			// 判断下checksum是否正确
			r.Seek(int64(24+l), io.SeekCurrent)
			tmp, e = r.br.Peek(8)
			if e != nil {
				r.markBad()
				return nil, e
			}

			l1 := binary.BigEndian.Uint32(tmp[0:4]) // 数据长度
			if bytes.Compare(tmp[4:8], dataMagicEnd) == 0 && l == l1 {
				// 正常数据，只是太大直接略过
				_, e = r.Seek(8, io.SeekCurrent)
			} else {
				// 非正常数据
				// 判断下4个字节
				_, e = r.Seek(oldOffset+4, io.SeekStart)
			}
			if e != nil {
				return nil, e
			}
		}
		return nil, ErrDataToLarge
	}

	r.expandBuf(int(l + 8))

	r.br.Discard(24)
	_, e = io.ReadFull(r.br, r.buf[0:l+8])
	if e != nil {
		r.markBad()
		return nil, e
	}
	r.offset += 32 + int64(l)

	l1 := binary.BigEndian.Uint32(r.buf[l : l+4]) // 数据长度
	if bytes.Compare(r.buf[l+4:l+8], dataMagicEnd) != 0 || l != l1 {
		if forNext {
			// 非正常数据
			// 判断下4个字节
			_, e = r.Seek(oldOffset+4, io.SeekStart)
			if e != nil {
				return nil, e
			}
		}
		return nil, errBadEndCheckSum
	}

	return r.buf[0:l], nil
}

// 返回当前位置的下一条有效记录.
//
//
// 注意:
//
//	1. 返回的buf会被覆盖，使用上可能有需要进行copy处理.
//	2. 如果数据长度大于MaxDataLen限制，则直接略过此记录
//
func (r *Reader) Next() (b []byte, e error) {

	// 由于有些数据是写乱了，所以要从offset开始找有效数据
	for {

		b, e = r.read(true)
		if e == nil {
			return b, nil
		}

		switch e {
		case errBadBeginCheckSum, errBadEndCheckSum, ErrDataToLarge:
			// continue
		default:
			return nil, e
		}
	}
}

// 在当前位置读一条记录.
//
// 注意返回的buf会被覆盖，使用上可能有需要进行copy处理.
//
func (r *Reader) Read() (b []byte, e error) {

	b, e = r.read(false)
	if e == nil {
		return b, nil
	}

	switch e {
	case errBadBeginCheckSum, errBadEndCheckSum:
		return nil, ErrBadRecord
	default:
		return nil, e
	}
}

// 返回当前的位置，在next成功返回时，可记录下位置，下一次从这里扫next.
func (r *Reader) Offset() int64 {
	return r.offset
}

func (r *Reader) Close() error {
	r.brpool.Put(r.br)
	r.bufpool.Put(r.buf)

	if c, ok := r.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}
