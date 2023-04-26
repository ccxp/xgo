package xfile

import (
	"bufio"
	"io"

	"github.com/ccxp/xgo/xlog"
)

// 带有缓存的ReadSeeker
type BufferedReadSeeker struct {
	r io.ReadSeeker

	brpool BufioReaderPool
	br     *bufio.Reader

	offset int64 // 当前offset

}

func NewBufferedReadSeeker(r io.ReadSeeker, brPool BufioReaderPool) (*BufferedReadSeeker, error) {
	offset, e := r.Seek(0, io.SeekCurrent)
	if e != nil {
		return nil, e
	}

	br := brPool.Get()
	br.Reset(r)

	return &BufferedReadSeeker{
		r: r,

		brpool: brPool,
		br:     br,

		offset: offset,
	}, nil
}

func (r *BufferedReadSeeker) Seek(offset int64, whence int) (int64, error) {

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

	off, e := r.r.Seek(offset, whence)
	if e != nil {
		xlog.Errorf("seek %v %v fail: %v", offset, whence, e)
		r.resetBuf()
		r.markBad()
		return off, e
	}
	r.offset = off
	r.resetBuf()
	return off, nil
}

func (r *BufferedReadSeeker) resetBuf() {
	if n := r.br.Buffered(); n > 0 {
		r.br.Discard(n)
	}
}

func (r *BufferedReadSeeker) markBad() {
	r.offset = -1
}

func (r *BufferedReadSeeker) Read(p []byte) (n int, err error) {
	n, err = r.br.Read(p)
	if err != nil {
		r.markBad()
	} else {
		r.offset += int64(n)
	}
	return
}

// 返回当前的位置，在next成功返回时，可记录下位置，下一次从这里扫next.
func (r *BufferedReadSeeker) Offset() int64 {
	return r.offset
}

func (r *BufferedReadSeeker) Close() error {
	r.brpool.Put(r.br)

	if c, ok := r.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// 关闭buffer，但不关闭里面的reader，这时这个reader不能再用
func (r *BufferedReadSeeker) CloseBuffer() {
	r.brpool.Put(r.br)

	r.r = nil
	r.brpool = nil
	r.br = nil
}
