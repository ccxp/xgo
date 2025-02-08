package xfilemq

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ccxp/xgo/xlog"
	"github.com/ccxp/xgo/xutil/xfile"
)

type FileMQ struct {
	DataPath            string        // 数据保存位置，如果不设置则不使用文件保存
	FilePrefix          string        // 文件名前缀
	FileSplitDuration   time.Duration // 多久切割一个文件
	FileReserveDuration time.Duration // 文件保留多久

	// 多久记录一次当前的读位置, <= 0是实时记录
	LogStatDuration time.Duration

	// 带缓存的写读io用
	BufioReaderPool xfile.BufioReaderPool
	BufferPool      xfile.BufferPool
	BufioWriterPool xfile.BufioWriterPool

	fw   *xfile.Writer
	fwTs int64
	wmu  sync.Mutex

	fr       *xfile.Reader
	frTs     int64
	frOffset int64

	lastData       []byte
	lastReadOffset int64 // 当前读取的位置，但没确认retry成功

	lastLogStatTs int64

	files []int64
	mu    sync.Mutex

	closed  bool
	closeCh chan struct{}
	wg      sync.WaitGroup
}

// 写当前的读取位置
func (q *FileMQ) saveState(ts, offset int64) {
	b := make([]byte, 16)
	binary.BigEndian.PutUint64(b, uint64(ts))
	binary.BigEndian.PutUint64(b[8:], uint64(offset))

	fn := fmt.Sprintf("%s/%s_stat", q.DataPath, q.FilePrefix)
	tmp := fn + ".tmp"
	if e := xfile.WriteFileSync(tmp, b, 0644); e == nil {
		os.Rename(tmp, fn)
	}

	// xlog.Debugf("save offset for %v is %v %v", q.FilePrefix, ts, offset)
}

// 读当前的读取位置
func (q *FileMQ) loadState() (ts, offset int64) {
	fn := fmt.Sprintf("%s/%s_stat", q.DataPath, q.FilePrefix)
	b, e := ioutil.ReadFile(fn)
	if e != nil || len(b) != 16 {
		return
	}

	ts = int64(binary.BigEndian.Uint64(b))
	offset = int64(binary.BigEndian.Uint64(b[8:]))

	xlog.Debugf("last offset for %v is %v %v", q.FilePrefix, ts, offset)

	return
}

func (q *FileMQ) loadStatus() {

	q.frTs, q.frOffset = q.loadState()

	files, _ := ioutil.ReadDir(q.DataPath)

	var name string
	prefix := q.FilePrefix + "."
	p := len(prefix)

	var t int64
	deleteTime := (time.Now().UnixNano() - int64(q.FileReserveDuration)) / int64(q.FileSplitDuration) * int64(q.FileSplitDuration)

	for _, f := range files {

		name = f.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}

		t, _ = strconv.ParseInt(name[p:], 10, 64)
		if t < deleteTime || t < q.frTs {
			continue
		}
		q.files = append(q.files, t)
	}

	sort.Slice(q.files, func(i, j int) bool {
		return q.files[i] < q.files[j]
	})
}

func (q *FileMQ) Init() {

	q.loadStatus()

	q.closeCh = make(chan struct{})
	go q.loopClean()

}

func (q *FileMQ) Close() {

	if !q.closed {
		q.closed = true
		close(q.closeCh)

		if q.fr != nil {
			// xlog.Warnf("closeFile")
			q.fr.Close()
			q.fr = nil
		}
		q.frTs = 0

		q.closeWriter()

		q.saveState(q.frTs, q.frOffset)

		q.wg.Wait()
	}
}

func (q *FileMQ) closeWriter() {
	if q.fw != nil {
		// xlog.Warnf("closeFile")
		q.fw.Close()
		q.fw = nil
	}
	q.fwTs = 0
}

// 检查当前用的文件状态要不要切换，要切换则切换文件
func (q *FileMQ) checkWriter() (e error) {
	t := time.Now().UnixNano() / int64(q.FileSplitDuration) * int64(q.FileSplitDuration)
	if t <= q.fwTs {
		return nil
	}

	q.closeWriter()

	q.fwTs = t
	defer func() {
		if e != nil {
			q.closeWriter()
		}
	}()

	// 生成队列文件writer
	fn := fmt.Sprintf("%s/%s.%v", q.DataPath, q.FilePrefix, t)
	f, e := os.OpenFile(fn, os.O_WRONLY|os.O_CREATE, 0640)
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

	q.mu.Lock()
	q.files = append(q.files, t)
	q.mu.Unlock()

	xlog.Debugf("open %v", fn)
	return nil
}

func (q *FileMQ) Save(b []byte) error {

	if q.closed {
		return nil
	}
	if len(b) == 0 {
		return nil
	}

	q.wmu.Lock()
	defer q.wmu.Unlock()

	e := q.checkWriter()
	if e != nil || q.fw == nil {
		xlog.Errorf("Write binlog fail: %v", e)
		return e
	}

	_, _, e = q.fw.Write(b)
	if e != nil {
		xlog.Errorf("Write binlog fail: %v", e)
		return e
	}

	e = q.fw.Flush()
	if e != nil {
		xlog.Errorf("Write binlog fail: %v", e)
		return e
	}

	return nil
}

func (q *FileMQ) loopClean() {

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

func (q *FileMQ) doClean() {
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

// 读下一个数据.
//
// 返回的buf会被覆盖，使用上可能有需要进行copy处理.
func (q *FileMQ) Next() []byte {

	deleteTime := (time.Now().UnixNano() - int64(q.FileReserveDuration)) / int64(q.FileSplitDuration) * int64(q.FileSplitDuration)

	if q.lastData != nil {
		// 上一次没有成功，检查下时间，看能不能直接返回上一次的数据
		if q.frTs >= deleteTime {
			return q.lastData
		}
		q.lastData = nil
	}

	// 看要不要打开新的读文件
	if q.fr != nil && q.frTs < deleteTime {
		q.fr.Close()
		q.fr = nil
	}

	if q.fr == nil {
		// 找下一个有效的文件
		var t int64
		var i int
		q.mu.Lock()
		for i = range q.files {
			if q.files[i] < deleteTime {
				continue
			}
			t = q.files[i]
			break
		}
		copy(q.files, q.files[i:])
		q.files = q.files[0 : len(q.files)-i]
		q.mu.Unlock()

		if t == 0 {
			return nil
		}

		fn := fmt.Sprintf("%s/%s.%v", q.DataPath, q.FilePrefix, t)
		f, e := os.Open(fn)
		if e != nil {
			if os.IsExist(e) {
				xlog.Errorf("open fail: %v", e)
			} else {
				q.mu.Lock()
				copy(q.files, q.files[1:])
				q.files = q.files[0 : len(q.files)-1]
				q.mu.Unlock()
			}
			return nil
		}

		if t == q.frTs && q.frOffset > 0 {
			_, e = f.Seek(q.frOffset, io.SeekStart)
			if e != nil {
				xlog.Errorf("Seek fail: %v", e)
				f.Close()
				return nil
			}
		}

		r, e := xfile.NewReader(f, q.BufioReaderPool, q.BufferPool)
		if e != nil {
			xlog.Errorf("NewReader fail: %v", e)
			f.Close()
			return nil
		}

		q.fr = r
		q.frTs = t
		q.frOffset = r.Offset()

		xlog.Debugf("load %v at %v", fn, q.frOffset)
	}

	for {
		b, e := q.fr.Next()
		if e != nil {
			if e != io.EOF {
				xlog.Errorf("read fail: %v", e)
				q.fr.Close()
				q.fr = nil
			} else {
				// eof而且有新文件，切换文件
				q.mu.Lock()
				if len(q.files) > 1 {
					copy(q.files, q.files[1:])
					q.files = q.files[0 : len(q.files)-1]
					q.fr.Close()
					q.fr = nil
				}
				q.mu.Unlock()
			}

			return nil
		}

		if len(b) == 0 {
			continue
		}

		q.lastData = b
		q.lastReadOffset = q.fr.Offset()
		return b
	}

	return nil
}

// 记录上一次返回的数据位置，代表上个数据已经处理成功
func (q *FileMQ) MarkSucc() {

	q.frOffset = q.lastReadOffset
	q.lastData = nil

	curr := time.Now().UnixNano()
	if q.LogStatDuration > 0 && curr < q.lastLogStatTs+int64(q.LogStatDuration) {
		return
	}

	q.lastLogStatTs = curr
	q.saveState(q.frTs, q.frOffset)
}

type fileStat struct {
	hasStat bool
	hasData bool
}

// 读目录下有哪些还有效的队列文件，返回队列prefix
func ListDir(dir string, fileReserveDuration, fileSplitDuration time.Duration) ([]string, error) {
	files, e := ioutil.ReadDir(dir)
	if e != nil {
		return nil, e
	}

	var name string
	var t, deleteTime int64
	if fileReserveDuration > 0 && fileSplitDuration > 0 {
		deleteTime = (time.Now().UnixNano() - int64(fileReserveDuration)) / int64(fileSplitDuration) * int64(fileSplitDuration)
	}

	m := make(map[string]*fileStat)
	var stat *fileStat
	var p int
	for _, f := range files {

		name = f.Name()
		if strings.HasSuffix(name, "_stat") {
			name = name[0 : len(name)-5]
			stat = m[name]
			if stat == nil {
				stat = &fileStat{hasStat: true}
				m[name] = stat
			} else {
				stat.hasStat = true
			}
			continue
		}

		p = strings.LastIndex(name, ".")
		if p <= 0 {
			continue
		}

		t, _ = strconv.ParseInt(name[p+1:], 10, 64)
		if t == 0 || (deleteTime > 0 && t < deleteTime) {
			continue
		}

		name = name[0:p]
		stat = m[name]
		if stat == nil {
			stat = &fileStat{hasData: true}
			m[name] = stat
		} else {
			stat.hasData = true
		}
	}

	res := make([]string, 0, len(m))
	for k, v := range m {
		if v.hasStat && v.hasData {
			res = append(res, k)
		}
	}
	return res, nil
}
