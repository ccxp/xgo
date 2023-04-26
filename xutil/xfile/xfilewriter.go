package xfile

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"
)

var (
	dataMagicBegin = []byte{0xf1, 0xe1, 0xf2, 0xe2}
	dataMagicEnd   = []byte{0xf3, 0xe3, 0xf4, 0xe4}
)

// 给writer写文件用的buffer
type BufioWriterPool interface {
	Get() *bufio.Writer
	Put(*bufio.Writer)
}

type bufioWriterPool struct {
	p sync.Pool
}

func (p *bufioWriterPool) Get() *bufio.Writer {
	return p.p.Get().(*bufio.Writer)
}

func (p *bufioWriterPool) Put(b *bufio.Writer) {
	p.p.Put(b)
}

func NewBufioWriterPool(cap int) BufioWriterPool {
	return &bufioWriterPool{
		p: sync.Pool{
			New: func() interface{} {
				return bufio.NewWriterSize(nil, cap)
			},
		},
	}
}

const (
	HeaderLen = 24
	TailLen   = 8
)

// 带有缓存、校验的数据写入器，跟Reader配合使用.
type Writer struct {
	w          io.Writer
	ws         io.WriteSeeker
	bw         *bufio.Writer
	pool       BufioWriterPool
	offset     int64 // 当前offset
	maxDataLen int

	preAllocSize int64
	fileSize     int64
}

// 默认单次写入最大64MB
const MaxDataLen = 64 << 20

// 初始化.
//
// 数据追加到结尾.
func NewWriter(w io.Writer, pool BufioWriterPool) (*Writer, error) {

	var offset int64
	var e error
	ws, ok := w.(io.WriteSeeker)
	if ok {
		offset, e = ws.Seek(0, io.SeekEnd)
		if e != nil {
			return nil, e
		}
	}

	bw := pool.Get()
	bw.Reset(w)

	return &Writer{
		w:          w,
		ws:         ws,
		bw:         bw,
		pool:       pool,
		offset:     offset,
		maxDataLen: -1,
		fileSize:   offset,
	}, nil
}

// 初始化.
//
// 数据offset开始，如果offset<当前大小，则从最后开始
func NewWriterAt(w io.WriteSeeker, offset int64, pool BufioWriterPool) (*Writer, error) {

	off, e := w.Seek(0, io.SeekEnd)
	if e != nil {
		return nil, e
	}
	fileSize := off
	if off > offset {
		off = offset
		_, e = w.Seek(off, io.SeekStart)
		if e != nil {
			return nil, e
		}
	}

	bw := pool.Get()
	bw.Reset(w)

	return &Writer{
		w:          w,
		ws:         w,
		bw:         bw,
		pool:       pool,
		offset:     off,
		maxDataLen: -1,
		fileSize:   fileSize,
	}, nil
}

// 更新单次写入最大长度，0是无限制
func (w *Writer) SetMaxDataLen(size int) {
	w.maxDataLen = size
}

func (w *Writer) getMaxDataLen() int {
	if w.maxDataLen >= 0 {
		return w.maxDataLen
	}
	return MaxDataLen
}

var space = make([]byte, 16)

// 写入数据，返回写入位置、长度.
// 长度包含校验信息.
func (w *Writer) Write(b []byte) (pos, n int64, e error) {

	maxDataLen := w.getMaxDataLen()
	if maxDataLen > 0 && len(b) > maxDataLen {
		e = fmt.Errorf("data length %d exceed MaxDataLength %d", len(b), maxDataLen)
		return
	}
	pos = w.offset

	l := len(b)
	bufOfLen := make([]byte, 4)
	binary.BigEndian.PutUint32(bufOfLen, uint32(l))

	// 要写的数据分别有
	// dataMagicBegin:    len=4, offset = 0
	// 数据长度:           len=4, offset = 4
	// reserve:           len=16, offset = 8
	// data:              offset = 24
	// 数据长度:           len=4, offset = 24 + len(data)
	// dataMagicEnd:      len=4, offset = 28 + len(data)

	n = int64(l) + 32

	if w.preAllocSize > 0 && w.offset+n > w.fileSize {
		// 分配新空间
		newSize := w.fileSize + w.preAllocSize
		for newSize < w.offset+n {
			newSize += w.preAllocSize
		}
		e = w.preAlloc(newSize)
		if e != nil {
			return
		}
	}

	_, e = w.bw.Write(dataMagicBegin)
	if e != nil {
		return
	}

	_, e = w.bw.Write(bufOfLen)
	if e != nil {
		return
	}

	_, e = w.bw.Write(space)
	if e != nil {
		return
	}

	if l > 0 {
		_, e = w.bw.Write(b)
		if e != nil {
			return
		}
	}

	_, e = w.bw.Write(bufOfLen)
	if e != nil {
		return
	}

	_, e = w.bw.Write(dataMagicEnd)
	if e != nil {
		return
	}

	w.offset += n
	if w.fileSize < w.offset {
		w.fileSize = w.offset
	}
	return
}

func (w *Writer) Flush() error {
	return w.bw.Flush()
}

func (w *Writer) Close() error {
	e := w.bw.Flush()
	if c, ok := w.w.(io.Closer); ok {
		if e1 := c.Close(); e1 != nil && e == nil {
			e = e1
		}
	}
	w.pool.Put(w.bw)
	return e
}

func (w *Writer) SyncClose() error {
	e := w.bw.Flush()
	if e == nil {
		if fp, ok := w.w.(*os.File); ok {
			e = Fdatasync(fp)
		}
	}
	if c, ok := w.w.(io.Closer); ok {
		if e1 := c.Close(); e1 != nil && e == nil {
			e = e1
		}
	}
	w.pool.Put(w.bw)
	return e
}

func (w *Writer) Sync() error {
	e := w.bw.Flush()
	if e == nil {
		if fp, ok := w.w.(*os.File); ok {
			e = Fdatasync(fp)
		}
	}
	return e
}

// 返回当前的位置，注意因为有缓存，所以不是实际当前打开文件的offset
func (w *Writer) Offset() int64 {
	return w.offset
}

// 开启预分配文件大小，注意文件不能用APPEND方式打开，而且要支持Seek.
//
// 下次打开时，如果直接打开到结尾有可能会浪费空间，需要先用Reader读一次才知道上次写到哪个位置（待优化）.
func (w *Writer) SetPreAlloc(allocSize int64) {
	if w.ws == nil {
		return
	}
	w.preAllocSize = allocSize
}

var emptyByte = []byte{0}

func (w *Writer) preAlloc(newSize int64) error {

	if w.ws == nil {
		return nil
	}

	// 当前
	curr, e := w.ws.Seek(0, io.SeekCurrent)
	if e != nil {
		return e
	}

	_, e = w.ws.Seek(newSize-1, io.SeekStart)
	if e != nil {
		return e
	}

	_, e = w.w.Write(emptyByte)
	if e != nil {
		return e
	}

	_, e = w.ws.Seek(curr, io.SeekStart)
	if e != nil {
		return e
	}
	w.fileSize = newSize
	return nil
}

/****************************/

func WriteFileSync(name string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = Fdatasync(f)
	}
	if err1 := f.Close(); err1 != nil && err == nil {
		err = err1
	}

	return err
}

// 写出跟Writer一样的格式，自行提供Writer
func WriteItem(w io.Writer, b []byte) (n int, e error) {

	l := len(b)
	bufOfLen := make([]byte, 4)
	binary.BigEndian.PutUint32(bufOfLen, uint32(l))

	// 要写的数据分别有
	// dataMagicBegin:    len=4, offset = 0
	// 数据长度:           len=4, offset = 4
	// reserve:           len=16, offset = 8
	// data:              offset = 24
	// 数据长度:           len=4, offset = 24 + len(data)
	// dataMagicEnd:      len=4, offset = 28 + len(data)

	n1, e := w.Write(dataMagicBegin)
	n += n1
	if e != nil {
		return
	}

	n1, e = w.Write(bufOfLen)
	n += n1
	if e != nil {
		return
	}

	n1, e = w.Write(space)
	n += n1
	if e != nil {
		return
	}

	if l > 0 {
		n1, e = w.Write(b)
		n += n1
		if e != nil {
			return
		}
	}

	n1, e = w.Write(bufOfLen)
	n += n1
	if e != nil {
		return
	}

	n1, e = w.Write(dataMagicEnd)
	n += n1
	if e != nil {
		return
	}

	return
}
