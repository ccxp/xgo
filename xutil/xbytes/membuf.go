package xbytes

import (
	"encoding/binary"
	"errors"
)

var (
	ErrBufferFull      = errors.New("buffer is full")
	ErrBufferEmpty     = errors.New("buffer is empty")
	ErrBufferCollapsed = errors.New("buffer is collapsed")
)

// 大小固定，而且内部可以小块分配内存的buffer
type Buffer struct {
	cap        int
	blockSize  int
	blockCount int

	size  int
	begin int
	end   int

	buf [][]byte
}

// 大小固定，而且内部可以小块分配内存的buffer
func NewBuffer(cap, blockSize int) *Buffer {
	b := &Buffer{
		blockSize:  blockSize,
		blockCount: (cap + blockSize - 1) / blockSize,
	}
	b.cap = b.blockCount * b.blockSize
	b.buf = make([][]byte, b.blockCount)
	for i := 0; i < b.blockCount; i++ {
		b.buf[i] = make([]byte, b.blockSize)
	}

	return b
}

func (b *Buffer) Size() int {
	return b.size
}

func (b *Buffer) Cap() int {
	return b.cap
}

func (b *Buffer) Left() int {
	return b.cap - b.size
}

func (b *Buffer) Rest() {
	b.begin = 0
	b.end = 0
	b.size = 0
}

func (b *Buffer) Write(buf []byte) error {
	n := len(buf)
	if n == 0 {
		return nil
	}

	if b.size+n > b.cap {
		return ErrBufferFull
	}

	idx := b.end / b.blockSize
	off := b.end - idx*b.blockSize
	var copyed, copyedTotal int
	for copyedTotal < n {
		copyed = copy(b.buf[idx][off:], buf[copyedTotal:])
		copyedTotal += copyed

		// 当前end的位置
		b.end += copyed
		if b.end >= b.cap {
			b.end -= b.cap
		}

		idx = b.end / b.blockSize
		off = b.end - idx*b.blockSize
	}

	b.size += n

	return nil
}

// 返回0代表没有数据
func (b *Buffer) Read(buf []byte) (n int) {
	n = len(buf)

	if n == 0 || b.size == 0 {
		return 0
	}

	if n > b.size {
		n = b.size
		buf = buf[0:n]
	}

	idx := b.begin / b.blockSize
	off := b.begin - idx*b.blockSize
	var copyed, copyedTotal int
	for copyedTotal < n {
		copyed = copy(buf[copyedTotal:], b.buf[idx][off:])
		copyedTotal += copyed

		// 当前begin的位置
		b.begin += copyed
		if b.begin >= b.cap {
			b.begin -= b.cap
		}
		idx = b.begin / b.blockSize
		off = b.begin - idx*b.blockSize
	}

	b.size -= n
	return n
}

func (b *Buffer) Peek(buf []byte) (n int) {
	n = len(buf)

	if n > b.size {
		n = b.size
		buf = buf[0:n]
	}

	begin := b.begin
	idx := begin / b.blockSize
	off := begin - idx*b.blockSize
	var copyed, copyedTotal int
	for copyedTotal < n {
		copyed = copy(buf[copyedTotal:], b.buf[idx][off:])
		copyedTotal += copyed

		// 当前begin的位置
		begin += copyed
		if begin >= b.cap {
			begin -= b.cap
		}
		idx = begin / b.blockSize
		off = begin - idx*b.blockSize
	}

	return n
}

// 同样是固定大小的内存容器，跟Buffer不一样的是用把单次写入的[]byte作为整体来读取.
//
// 会添加header，所以内存占用会稍大
type Container struct {
	buf   *Buffer
	count int
}

func NewContainer(cap, blockSize int) *Container {
	return &Container{
		buf: NewBuffer(cap, blockSize),
	}
}

func (c *Container) Reset() {
	c.buf.Rest()
	c.count = 0
}

func (c *Container) Push(b []byte) error {
	cnt := len(b)
	if c.buf.Left() < 4+cnt {
		return ErrBufferFull
	}

	tmp := make([]byte, 4)
	binary.BigEndian.PutUint32(tmp, uint32(cnt))
	c.buf.Write(tmp)
	c.buf.Write(b)

	c.count++

	return nil
}

// 返回nil, ErrBufferEmpty代表没有数据
func (c *Container) Pop() (b []byte, e error) {
	tmp := make([]byte, 4)
	n := c.buf.Read(tmp)
	if n == 0 {
		return nil, ErrBufferEmpty
	}
	if n != 4 {
		return nil, ErrBufferCollapsed
	}

	n = int(binary.BigEndian.Uint32(tmp))
	b = make([]byte, n)

	if c.buf.Read(b) != n {
		return nil, ErrBufferCollapsed
	}

	c.count--

	return b, nil
}

// 取多少个item，不是数据大小.
func (c *Container) Size() int {
	return c.count
}

// 取状态，返回item个数，内存大小
func (c *Container) Status() (cnt, memSize, memLeft int) {
	return c.count, c.buf.Size(), c.buf.Left()
}

// 判断当前一次写入能写多大的数据.
//
// 注意不含自动添加的包头，所以是一次能写入的数据.
func (c *Container) Left() int {
	i := c.buf.Left() - 4
	if i < 0 {
		return 0
	}
	return i
}

// 可以重复使用的buffer
type Bytes struct {
	buf []byte
}

const pageSize = 4096

func (b *Bytes) Get(l int) []byte {
	if len(b.buf) < l {
		n := (l + pageSize - 1) / pageSize * pageSize
		b.buf = make([]byte, n)
	}
	return b.buf[0:l]
}

func (b *Bytes) Cap() int {
	return len(b.buf)
}
