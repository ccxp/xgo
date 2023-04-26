package xbinlog

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ccxp/xgo/xlog"
	"github.com/ccxp/xgo/xutil/xfile"
)

const (
	ASYNC_FLUSH = iota // 异步写（使用缓存）
	SYNC_FLUSH         // 同步写
)

type BinLog struct {
	DataPath            string        // 数据保存位置，如果不设置则不使用文件保存
	FilePrefix          string        // 文件名前缀
	FileSplitDuration   time.Duration // 多久切割一个文件
	FileReserveDuration time.Duration // 文件保留多久
	FlushMode           int

	PoolSize int // 异步队列长度，0是同步写入

	BufioWriterPool xfile.BufioWriterPool

	Step func(b []byte) error

	fw   *xfile.Writer
	fwTs int64 // nano，当前使用文件的timestamp

	closed  bool
	closeCh chan struct{}
	wg      sync.WaitGroup

	itemCh chan []byte
}

// 注意并发锁的问题，比如调用的时候所有worker要不在工作状态
func (q *BinLog) Close() {

	if !q.closed {
		q.closed = true
		close(q.closeCh)
		q.wg.Wait()
	}
}

func (q *BinLog) Init() (e error) {

	q.closeCh = make(chan struct{})
	q.reload()

	e = q.checkFile()
	if q.PoolSize > 0 {
		q.itemCh = make(chan []byte, q.PoolSize)
		go q.loopSave()
	}
	go q.loopClean()

	return e
}

func (q *BinLog) reload() (e error) {

	t := time.Now().UnixNano()/int64(q.FileSplitDuration)*int64(q.FileSplitDuration) - int64(q.FileReserveDuration) - int64(q.FileSplitDuration)

	brp := xfile.NewBufioReaderPool(1 << 20)
	bp := xfile.NewBufferPool(1 << 20)

	for {
		t += int64(q.FileSplitDuration)
		if t > time.Now().UnixNano() {
			break
		}

		fn := fmt.Sprintf("%s/%s.%v", q.DataPath, q.FilePrefix, t)

		f, e := os.Open(fn)
		if e != nil {
			if os.IsExist(e) {
				xlog.Errorf("open fail: %v", e)
			}
			continue
		}

		xlog.Warnf("load %v", fn)

		r, e := xfile.NewReader(f, brp, bp)
		if e != nil {
			xlog.Errorf("NewReader fail: %v", e)
			f.Close()
			continue
		}

		for {
			b, e := r.Next()
			if e != nil {
				if e != io.EOF {
					xlog.Errorf("Next fail: %v", e)
				}
				break
			}
			e = q.Step(b)
			if e != nil {
				xlog.Errorf("Step fail: %v", e)
			}
		}
		r.Close()
	}

	return nil
}

func (q *BinLog) closeFile() {
	if q.fw != nil {
		// xlog.Warnf("closeFile")
		q.fw.Close()
		q.fw = nil
	}
	q.fwTs = 0
}

// 检查当前用的文件状态要不要切换，要切换则切换文件
func (q *BinLog) checkFile() (e error) {
	t := time.Now().UnixNano() / int64(q.FileSplitDuration) * int64(q.FileSplitDuration)
	if t <= q.fwTs {
		return nil
	}

	q.closeFile()

	q.fwTs = t
	defer func() {
		if e != nil {
			q.closeFile()
		}
	}()

	// 生成队列文件writer
	fn := fmt.Sprintf("%s/%s.%v", q.DataPath, q.FilePrefix, t)
	f, e := os.OpenFile(fn, os.O_WRONLY|os.O_CREATE, 0740)
	if e != nil {
		xlog.Errorf("os.OpenFile fail: %v", e)
		return
	}
	w, e := xfile.NewWriter(f, q.BufioWriterPool)
	if e != nil {
		f.Close()
		xlog.Errorf("xfile.NewWriter fail: %v", e)
		return
	}
	q.fw = w

	xlog.Debugf("open %v", fn)
	return nil
}

func (q *BinLog) Save(b []byte) {
	if q.PoolSize > 0 {
		q.itemCh <- b
	} else {
		q.save(b)
	}
}

func (q *BinLog) save(b []byte) error {

	e := q.checkFile()
	if e != nil || q.fw == nil {
		xlog.Errorf("Write binlog fail: %v", e)
		return e
	}

	_, _, e = q.fw.Write(b)
	if e != nil {
		xlog.Errorf("Write binlog fail: %v", e)
		return e
	}

	if q.FlushMode == SYNC_FLUSH {
		e = q.fw.Flush()
		if e != nil {
			xlog.Errorf("Write binlog fail: %v", e)
			return e
		}
	}

	return nil
}

func (q *BinLog) loopSave() {

	q.wg.Add(1)
	defer func() {
		q.closeFile()
		q.wg.Done()
	}()

	var b []byte
	for {
		select {
		case b = <-q.itemCh:
			q.save(b)
		case <-q.closeCh:
			return
		}
	}
}

func (q *BinLog) loopClean() {

	q.wg.Add(1)
	defer q.wg.Done()

	q.doClean()

	ticker := time.NewTicker(q.FileSplitDuration)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			q.doClean()
		case <-q.closeCh:
			return
		}
	}
}

func (q *BinLog) doClean() {
	files, _ := ioutil.ReadDir(q.DataPath)

	var name string
	prefix := q.FilePrefix + "."
	p := len(prefix)

	var t int64
	deleteTime := (time.Now().UnixNano()-int64(q.FileReserveDuration))/int64(q.FileSplitDuration)*int64(q.FileSplitDuration) - int64(q.FileSplitDuration)

	for _, f := range files {
		if q.closed {
			return
		}
		name = f.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}

		t, _ = strconv.ParseInt(name[p:], 10, 64)
		if t == 0 || t >= deleteTime {
			continue
		}

		xlog.Debugf("delete data file %v", f.Name())
		os.Remove(fmt.Sprintf("%v/%v", q.DataPath, f.Name()))
	}
}
