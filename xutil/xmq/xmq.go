package xmq

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ccxp/xgo/xlog"
	"github.com/ccxp/xgo/xutil/xbytes"
	"github.com/ccxp/xgo/xutil/xfile"
	"github.com/ccxp/xgo/xutil/xmq/internal/xmqproto"

	proto "github.com/gogo/protobuf/proto"
)

type HANDLE_RESULT int

const (
	RESULT_OK HANDLE_RESULT = iota
	RESULT_ERROR
	RESULT_RETRYABLE
)

const result_unknown HANDLE_RESULT = -1

type BINLOG_MODE int

const (
	ASYNC_BINLOG BINLOG_MODE = iota // 异步写（使用缓存）
	SYNC_BINLOG                     // 同步写
)

type ErrDelay struct {
	Duration time.Duration
}

func (e *ErrDelay) Error() string {
	return fmt.Sprintf("Err: delay %v", e.Duration)
}

var (
	ErrEmptyQueue      = errors.New("queue is empty")
	ErrQueueClosed     = errors.New("queue is closed")
	ErrContextCanceled = errors.New("context is canceled")
)

type mqQueue struct {
	QueueSize           int    // 队列占用内存大小
	BlockSize           int    // 内存块大小，防止一次申请一整块大的内存
	DataPath            string // 数据保存位置，如果不设置则不使用文件保存
	FilePrefix          string // 文件名前缀
	FileSplitDuration   int64  // 秒，多久切割一个文件，默认是60s
	FileReserveDuration int64  // 秒，文件保留多久，默认是600s，超出时间的文件则重启时不reload
	BinLogMode          BINLOG_MODE
	SkipStorageError    bool
	Delay               time.Duration // 延迟多久
	bw                  xfile.BufioWriterPool

	fileSuffix string

	queue *xbytes.Container
	fw    *xfile.Writer
	fwTs  int64 // 秒，当前使用文件的timestamp
	mu    sync.Mutex

	fwResult   *xfile.Writer
	fwResultTs int64 // 秒，当前使用文件的timestamp
	resultMu   sync.Mutex

	// retry队列用，如果读出来时还不到时间，则先放在这
	delayItem *QueueItem
	delayOn   time.Time

	closed  bool
	closeCh chan struct{}

	WorkerCount int
	itemReadyCh chan int
	itemDelayCh chan int

	// 用本机ip、pid、时间戳生成不严谨的uuid，
	// 如果需要严谨的uuid请自行实现.
	GetUUID func() (uint64, uint64)
}

// 注意并发锁的问题，比如调用的时候所有worker要不在工作状态
func (q *mqQueue) Close() {

	q.closed = true

	q.mu.Lock()
	q.closeFile()
	q.mu.Unlock()

	q.resultMu.Lock()
	q.closeResultFile()
	q.resultMu.Unlock()
}

func (q *mqQueue) init() (e error) {
	q.queue = xbytes.NewContainer(q.QueueSize, q.BlockSize)
	if q.DataPath != "" {
		q.reload()

		e = q.checkFile()
		if e == nil {
			e = q.checkResultFile()
		}
		if e != nil && q.SkipStorageError {
			e = nil
		}
	}

	if q.WorkerCount == 0 {
		q.WorkerCount = 16
	}

	q.itemReadyCh = make(chan int, q.WorkerCount*2)
	if q.Delay > 0 {
		q.itemDelayCh = make(chan int, q.WorkerCount*2)
		for i := 0; i < q.WorkerCount; i++ {
			go q.monitorDelay()
		}
	}
	return e
}

type uuid128 struct {
	X, Y uint64
}

func (q *mqQueue) reload() (e error) {

	// 读结果文件
	t := time.Now().Unix()/q.FileSplitDuration*q.FileSplitDuration - q.FileReserveDuration - q.FileSplitDuration
	done := make(map[uuid128]bool)

	brp := xfile.NewBufioReaderPool(1 << 20)
	bp := xfile.NewBufferPool(1 << 20)

	for {
		t += q.FileSplitDuration
		if t > time.Now().Unix() {
			break
		}

		fn := fmt.Sprintf("%s/%s_%v.r%s", q.DataPath, q.FilePrefix, t, q.fileSuffix)
		f, e := os.Open(fn)
		if e != nil {
			if os.IsExist(e) {
				xlog.Errorf("open fail: %v", e)
			}
			continue
		}

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

			tmp := &xmqproto.QueueItemLog{}
			e = proto.Unmarshal(b, tmp)
			if e != nil {
				xlog.Errorf("proto.Unmarshal fail: %v", e)
				continue
			}

			// xlog.Debugf("%v", tmp)
			done[uuid128{tmp.Uuid1, tmp.Uuid2}] = true
		}
		r.Close()
	}

	// 读历史数据
	t = time.Now().Unix()/q.FileSplitDuration*q.FileSplitDuration - q.FileReserveDuration - q.FileSplitDuration
	for {
		t += q.FileSplitDuration
		if t > time.Now().Unix() {
			break
		}

		fn := fmt.Sprintf("%s/%s_%v.q%s", q.DataPath, q.FilePrefix, t, q.fileSuffix)
		f, e := os.Open(fn)
		if e != nil {
			if os.IsExist(e) {
				xlog.Errorf("open fail: %v", e)
			}
			continue
		}

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

			item := &QueueItem{}
			e = proto.Unmarshal(b, item)
			if e != nil {
				xlog.Errorf("proto.Unmarshal fail: %v", e)
				continue
			}

			if done[uuid128{item.Uuid1, item.Uuid2}] {
				continue
			}

			// xlog.Debugf("reload %v", item)
			// 写进队列
			for {
				e = q.queue.Push(b)
				if e == xbytes.ErrBufferCollapsed {
					q.queue.Reset()
					continue
				} else if e == xbytes.ErrBufferFull {
					q.queue.Pop() // 内存不够，抛弃旧的，保留新的
					continue
				}
				break
			}
		}
		r.Close()
	}

	return nil
}

func (q *mqQueue) closeFile() {
	if q.fw != nil {
		// xlog.Warnf("closeFile")
		q.fw.Close()
		q.fw = nil
	}
	q.fwTs = 0
}

func (q *mqQueue) closeResultFile() {
	if q.fwResult != nil {
		// xlog.Warnf("closeResultFile")
		q.fwResult.Close()
		q.fwResult = nil
	}
	q.fwResultTs = 0
}

// 检查当前用的文件状态要不要切换，要切换则切换文件
func (q *mqQueue) checkFile() (e error) {
	t := time.Now().Unix() / q.FileSplitDuration * q.FileSplitDuration
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
	fn := fmt.Sprintf("%s/%s_%v.q%s", q.DataPath, q.FilePrefix, t, q.fileSuffix)
	f, e := os.OpenFile(fn, os.O_WRONLY|os.O_CREATE, 0740)
	if e != nil {
		xlog.Errorf("os.OpenFile fail: %v", e)
		return
	}
	w, e := xfile.NewWriter(f, q.bw)
	if e != nil {
		f.Close()
		xlog.Errorf("xfile.NewWriter fail: %v", e)
		return
	}
	q.fw = w

	return nil
}

// 检查当前用的文件状态要不要切换，要切换则切换文件
func (q *mqQueue) checkResultFile() (e error) {
	t := time.Now().Unix() / q.FileSplitDuration * q.FileSplitDuration
	if t <= q.fwResultTs {
		return nil
	}

	q.closeResultFile()

	q.fwResultTs = t
	defer func() {
		if e != nil {
			q.closeResultFile()
		}
	}()

	// 生成队列文件writer
	fn := fmt.Sprintf("%s/%s_%v.r%s", q.DataPath, q.FilePrefix, t, q.fileSuffix)
	f, e := os.OpenFile(fn, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0740)
	if e != nil {
		xlog.Errorf("os.OpenFile fail: %v", e)
		return
	}
	w, e := xfile.NewWriter(f, q.bw)
	if e != nil {
		f.Close()
		xlog.Errorf("xfile.NewWriter fail: %v", e)
		return
	}
	q.fwResult = w
	return nil
}

func (q *mqQueue) push(item *QueueItem) error {
	item.Uuid1, item.Uuid2 = q.GetUUID()
	item.UpdateTime = time.Now().UnixNano()

	b, e := proto.Marshal(item)
	if e != nil {
		xlog.Errorf("proto.Marshal fail: %v", e)
		return e
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	// 判断一下内存够不够
	if q.queue.Left() < len(b) {
		return xbytes.ErrBufferFull
	}

	if q.DataPath != "" {
		e = q.checkFile()
		if e != nil {
			xlog.Errorf("Write binlog fail: %v", e)
			if !q.SkipStorageError {
				return e
			}
		}

		if e == nil && q.fw != nil {
			_, _, e = q.fw.Write(b)
			// xlog.Debugf("Write: %v", e)
			if e != nil {
				xlog.Errorf("Write binlog fail: %v", e)
				if !q.SkipStorageError {
					return e
				}
			}
		}

		if q.BinLogMode == SYNC_BINLOG && e == nil && q.fw != nil {
			e = q.fw.Flush()
			if e != nil {
				xlog.Errorf("Write binlog fail: %v", e)
				if !q.SkipStorageError {
					return e
				}
			}
		}

		e = nil
	}

	e = q.queue.Push(b)
	if e == xbytes.ErrBufferCollapsed {
		q.queue.Reset()
		e = q.queue.Push(b)
	}

	if e != nil {
		if e == xbytes.ErrBufferCollapsed {
			q.queue.Reset()
		}
		xlog.Errorf("Push to queue fail: %v", e)
		return e
	}

	if q.Delay > 0 {
		select {
		case q.itemDelayCh <- 1:
		default:
		}
	} else {
		select {
		case q.itemReadyCh <- 1:
		default:
		}
	}

	return nil
}

func (q *mqQueue) popNoDelay() (*QueueItem, error) {

	q.mu.Lock()
	defer q.mu.Unlock()

	b, e := q.queue.Pop()
	if e == xbytes.ErrBufferEmpty {
		return nil, nil
	} else if e == xbytes.ErrBufferCollapsed {
		q.queue.Reset()
		xlog.Errorf("queue is collapsed, reset")
		return nil, nil
	} else if e != nil {
		xlog.Errorf("pop item fail: %v", e)
		return nil, e
	}

	item := &QueueItem{}
	e = proto.Unmarshal(b, item)
	if e != nil {
		xlog.Errorf("proto.Unmarshal item fail: %v", e)
		return nil, e
	}

	return item, nil
}

func (q *mqQueue) pop() (*QueueItem, error) {

	if q.closed {
		return nil, ErrQueueClosed
	}

	if q.Delay <= 0 {
		return q.popNoDelay()
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	curr := time.Now()
	if q.delayItem != nil {
		du := q.delayOn.Sub(curr)
		if du > 0 {
			return nil, &ErrDelay{du}
		}
		item := q.delayItem
		q.delayItem = nil
		return item, nil
	}

	b, e := q.queue.Pop()
	if e == xbytes.ErrBufferEmpty {
		return nil, nil
	} else if e == xbytes.ErrBufferCollapsed {
		q.queue.Reset()
		xlog.Errorf("queue is collapsed, reset")
		return nil, nil
	} else if e != nil {
		xlog.Errorf("pop item fail: %v", e)
		return nil, e
	}

	item := &QueueItem{}
	e = proto.Unmarshal(b, item)
	if e != nil {
		xlog.Errorf("proto.Unmarshal item fail: %v", e)
		return nil, e
	}

	delayOn := time.Unix(0, item.UpdateTime).Add(q.Delay)
	du := delayOn.Sub(curr)
	if du > 0 {
		q.delayOn = delayOn
		q.delayItem = item
		return nil, &ErrDelay{du}
	}

	return item, nil
}

func (q *mqQueue) peekDelay() (time.Duration, error) {

	if q.closed {
		return 0, ErrQueueClosed
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	curr := time.Now()
	if q.delayItem != nil {
		du := q.delayOn.Sub(curr)
		return du, nil
	}

	b, e := q.queue.Pop()
	if e == xbytes.ErrBufferEmpty {
		return 0, e
	} else if e == xbytes.ErrBufferCollapsed {
		q.queue.Reset()
		xlog.Errorf("queue is collapsed, reset")
		return 0, e
	} else if e != nil {
		xlog.Errorf("pop item fail: %v", e)
		return 0, e
	}

	item := &QueueItem{}
	e = proto.Unmarshal(b, item)
	if e != nil {
		xlog.Errorf("proto.Unmarshal item fail: %v", e)
		return 0, e
	}

	q.delayOn = time.Unix(0, item.UpdateTime).Add(q.Delay)
	q.delayItem = item
	du := q.delayOn.Sub(curr)

	return du, nil
}

func (q *mqQueue) monitorDelay() {
	var du time.Duration
	var e error
	for !q.closed {
		du, e = q.peekDelay()
		if e == nil {
			if du > time.Millisecond {
				timer := time.NewTimer(du)
				select {
				case <-timer.C:
				case <-q.closeCh:
				}
				timer.Stop()
			}

			select {
			case q.itemReadyCh <- 1:
			default:
			}
		}

		<-q.itemDelayCh
	}
}

// 记录已经处理过
func (q *mqQueue) logItem(item *QueueItem) error {

	if q.DataPath == "" {
		return nil
	}

	if q.closed {
		return ErrQueueClosed
	}

	tmp := &xmqproto.QueueItemLog{
		Uuid1:      item.Uuid1,
		Uuid2:      item.Uuid2,
		UpdateTime: time.Now().UnixNano(),
	}

	b, e := proto.Marshal(tmp)
	if e != nil {
		xlog.Errorf("proto.Marshal fail: %v", e)
		return e
	}

	q.resultMu.Lock()
	defer q.resultMu.Unlock()

	e = q.checkResultFile()
	if e != nil {
		if q.SkipStorageError {
			return nil
		}
		return e
	}

	if q.fwResult == nil { // SkipStorageError引起没有成功打开文件
		return nil
	}

	_, _, e = q.fwResult.Write(b)
	// xlog.Debugf("Write result: %v", e)
	if e != nil {
		xlog.Errorf("Write binlog fail: %v", e)
		if q.SkipStorageError {
			return nil
		}
		return e
	}

	if q.BinLogMode == SYNC_BINLOG {
		e = q.fwResult.Flush()
		if e != nil {
			xlog.Errorf("Write binlog fail: %v", e)
			if q.SkipStorageError {
				return nil
			}
			return e
		}
	}
	return nil
}

func (q *mqQueue) size() int {
	q.mu.Lock()
	cnt := q.queue.Size()
	if q.delayItem != nil {
		cnt++
	}
	q.mu.Unlock()
	return cnt
}

// 取状态，返回item个数，内存大小
func (q *mqQueue) status() (cnt, memSize, memLeft int) {

	q.mu.Lock()
	cnt, memSize, memLeft = q.queue.Status()
	if q.delayItem != nil {
		cnt++
		memSize += len(q.delayItem.Data1) + len(q.delayItem.Data2) + len(q.delayItem.Data3) + len(q.delayItem.Data4) + len(q.delayItem.Data5)
	}
	q.mu.Unlock()

	return
}

type MQ struct {
	QueueSize      int // 队列占用内存大小
	RetryQueueSize int // 重试队列占用内存大小，为0则不重试
	BlockSize      int // 内存块大小，防止一次申请一整块大的内存

	DataPath            string // 数据保存位置，如果不设置则不使用文件保存
	FilePrefix          string // 文件名前缀
	WriterBufferSize    int    // 写缓存大小，默认1MB
	FileSplitDuration   int64  // 秒，多久切割一个文件，默认是60s
	FileReserveDuration int64  // 秒，文件保留多久，默认是600s，超出时间的文件则重启时不reload

	SkipStorageError bool // 当使用文件保存时，如果 SkipStorageError = true，则会忽略任何读写文件异常

	// 设置成true后，自动根据 FileReserveDuration删除数据文件
	AutoClean bool

	// binlog写入方式:
	//
	// 	ASYNC_BINLOG: 只写入缓存，不保证写binlog成功. 默认方式.
	// 	SYNC_BINLOG: 同步写binlog
	//
	BinLogMode BINLOG_MODE

	WorkerCount int
	Delay       time.Duration // 延迟多久才处理

	MaxRetry         int // 最大重试次数
	RetryWorkerCount int
	RetryDelay       time.Duration // 重试延迟多久

	// 数据处理函数，当Handler=nil时使用无handler模式
	Handler   func(item *QueueItem) HANDLE_RESULT
	HandlerV2 func(workerIdx int, isRetry bool, item *QueueItem) HANDLE_RESULT

	// 批量处理模式，优先于Handler.
	//
	// 	res []HANDLE_RESULT: 已经按items大小设置好，用户需要设置 res[i] = RESULT_OK 就可以了
	//
	BatchHandler   func(items []*QueueItem, res []HANDLE_RESULT)
	BatchHandlerV2 func(workerIdx int, isRetry bool, items []*QueueItem, res []HANDLE_RESULT)
	// batch最大item数量，默认是10
	BatchMaxItem int
	// batch最大一次处理的包大小，默认是0
	BatchMaxItemSize int
	// 批量取item时最大等待多久，默认是100ms
	BatchWaitDuration time.Duration

	// 默认用本机ip、pid、时间戳生成不严谨的uuid，
	// 如果需要严谨的uuid请自行实现.
	GetUUID func() (uint64, uint64)

	bw         xfile.BufioWriterPool
	queue      *mqQueue
	retryQueue *mqQueue

	wg      sync.WaitGroup
	closed  bool
	closeCh chan struct{}
}

func (q *MQ) Init() (e error) {
	if q.QueueSize <= 0 {
		return fmt.Errorf("QueueSize %v error", q.QueueSize)
	}
	if q.BlockSize <= 0 {
		return fmt.Errorf("BlockSize %v error", q.BlockSize)
	}
	if (q.Handler != nil || q.BatchHandler != nil || q.HandlerV2 != nil || q.BatchHandlerV2 != nil) && q.WorkerCount <= 0 {
		return fmt.Errorf("WorkerCount %v error", q.WorkerCount)
	}

	if q.RetryQueueSize > 0 {
		if (q.Handler != nil || q.BatchHandler != nil || q.HandlerV2 != nil || q.BatchHandlerV2 != nil) && q.RetryWorkerCount <= 0 {
			return fmt.Errorf("RetryWorkerCount %v error", q.RetryWorkerCount)
		}
	} else {
		q.RetryWorkerCount = 0
	}

	if q.BatchMaxItem <= 0 {
		q.BatchMaxItem = 10
	}
	if q.BatchWaitDuration <= 0 {
		q.BatchWaitDuration = time.Millisecond * 100
	}

	q.closeCh = make(chan struct{})

	if q.DataPath != "" {

		if q.WriterBufferSize <= 0 {
			q.WriterBufferSize = 1 << 20
		}
		if q.FileSplitDuration <= 0 {
			q.FileSplitDuration = 60
		}
		if q.FileReserveDuration <= 0 {
			q.FileReserveDuration = 600
			if q.FileReserveDuration < q.FileSplitDuration*2 {
				q.FileReserveDuration = q.FileSplitDuration * 2
			}
		}

		// 打开当前要写的文件
		q.bw = xfile.NewBufioWriterPool(q.WriterBufferSize)
	}

	defer func() {
		if e == nil {
			return
		}

		if q.queue != nil {
			q.queue.Close()
		}
		if q.retryQueue != nil {
			q.retryQueue.Close()
		}
	}()

	if q.GetUUID == nil {
		q.GetUUID = getUUID
	}
	q.queue = &mqQueue{
		GetUUID:             q.GetUUID,
		QueueSize:           q.QueueSize,
		BlockSize:           q.BlockSize,
		DataPath:            q.DataPath,
		FilePrefix:          q.FilePrefix,
		fileSuffix:          "",
		FileSplitDuration:   q.FileSplitDuration,
		FileReserveDuration: q.FileReserveDuration,
		BinLogMode:          q.BinLogMode,
		SkipStorageError:    q.SkipStorageError,
		bw:                  q.bw,

		Delay: q.Delay,

		closeCh:     q.closeCh,
		WorkerCount: q.WorkerCount,
	}
	e = q.queue.init()
	if e != nil {
		return
	}

	if q.RetryQueueSize > 0 {
		q.retryQueue = &mqQueue{
			GetUUID:             q.GetUUID,
			QueueSize:           q.RetryQueueSize,
			BlockSize:           q.BlockSize,
			DataPath:            q.DataPath,
			FilePrefix:          q.FilePrefix,
			fileSuffix:          ".retry",
			FileSplitDuration:   q.FileSplitDuration,
			FileReserveDuration: q.FileReserveDuration,
			BinLogMode:          q.BinLogMode,
			SkipStorageError:    q.SkipStorageError,
			bw:                  q.bw,

			Delay: q.RetryDelay,

			closeCh:     q.closeCh,
			WorkerCount: q.RetryWorkerCount,
		}
		e = q.retryQueue.init()
		if e != nil {
			return
		}
	}

	if q.BatchHandler != nil || q.BatchHandlerV2 != nil {
		// 启动 worker
		for i := 0; i < q.WorkerCount; i++ {
			q.wg.Add(1)
			go q.batchWork(i, false)
		}
		if q.RetryQueueSize > 0 {
			for i := 0; i < q.RetryWorkerCount; i++ {
				q.wg.Add(1)
				go q.batchWork(i, true)
			}
		}
	} else if q.Handler != nil || q.HandlerV2 != nil {
		// 启动 worker
		for i := 0; i < q.WorkerCount; i++ {
			q.wg.Add(1)
			go q.work(i, false)
		}
		if q.RetryQueueSize > 0 {
			for i := 0; i < q.RetryWorkerCount; i++ {
				q.wg.Add(1)
				go q.work(i, true)
			}
		}
	}

	if q.AutoClean && q.DataPath != "" {
		go q.autoClean()
	}

	return nil
}

func (q *MQ) autoClean() {

	q.doCleanAll()

	ticker := time.NewTicker(time.Duration(q.FileSplitDuration) * time.Second)
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

func (q *MQ) doCleanAll() {
	files, _ := ioutil.ReadDir(q.DataPath)

	var name string
	prefix := q.FilePrefix + "_"

	var p1, p2 int
	p1 = len(prefix)

	var t int64
	deleteTime := (time.Now().Unix()-q.FileReserveDuration)/q.FileSplitDuration*q.FileSplitDuration - q.FileSplitDuration

	for _, f := range files {
		if q.closed {
			return
		}
		name = f.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}

		name = name[p1:]
		p2 = strings.Index(name, ".")
		if p2 <= 0 {
			continue
		}
		t, _ = strconv.ParseInt(name[0:p2], 10, 64)
		if t == 0 || t >= deleteTime {
			continue
		}

		xlog.Debugf("delete data file %v", f.Name())
		os.Remove(fmt.Sprintf("%v/%v", q.DataPath, f.Name()))
	}
}

var suffixs = []string{".q", ".r", ".q.retry", ".r.retry"}

func (q *MQ) doClean() {

	t := (time.Now().Unix()-q.FileReserveDuration)/q.FileSplitDuration*q.FileSplitDuration - q.FileSplitDuration
	var s1, s2 string

	for i := int64(0); i < 2; i++ {
		s1 = fmt.Sprintf("%s/%s_%v", q.DataPath, q.FilePrefix, t)
		for _, s := range suffixs {
			s2 = s1 + s
			if _, e := os.Stat(s2); e == nil {
				xlog.Debugf("delete data file %v", s2)
				os.Remove(s2)
			}
		}

		t -= q.FileSplitDuration
	}
}

func (q *MQ) Push(item *QueueItem) error {

	if q.closed {
		return ErrQueueClosed
	}

	item.Timestamp = time.Now().UnixNano()

	e := q.queue.push(item)
	if e != nil {
		return e
	}

	return nil
}

func (q *MQ) pop(isRetry, isWorker bool, ctx context.Context) (item *QueueItem, e error) {

	if !isRetry {
		item, e = q.queue.pop()
		if item != nil {
			return
		}

		if e != nil {
			if isWorker {
				if e1, ok := e.(*ErrDelay); ok {
					time.Sleep(e1.Duration)
					return nil, nil
				} else {
					return
				}
			} else {
				return
			}
		}

		select {
		case <-q.queue.itemReadyCh:
		case <-q.closeCh:
			return nil, ErrQueueClosed
		case <-ctx.Done():
			return nil, ErrContextCanceled
		}

		return q.queue.pop()

	} else {
		item, e = q.retryQueue.pop()
		if item != nil {
			return
		}

		if e != nil {
			if isWorker {
				if e1, ok := e.(*ErrDelay); ok {
					time.Sleep(e1.Duration)
					return nil, nil
				} else {
					return
				}
			} else {
				return
			}
		}

		select {
		case <-q.retryQueue.itemReadyCh:
		case <-q.closeCh:
			return nil, ErrQueueClosed
		case <-ctx.Done():
			return nil, ErrContextCanceled
		}

		return q.retryQueue.pop()
	}
}

func (q *MQ) handleItemResult(isRetry bool, item *QueueItem, ret HANDLE_RESULT, ctx context.Context) {
	if ret != RESULT_OK {
		item.HandlerCount++
		xlog.Errorf("queue [%v] handle item fail, HandlerCount:%v", q.FilePrefix, item.HandlerCount)
		if ret == RESULT_RETRYABLE && q.RetryQueueSize > 0 && item.HandlerCount <= uint32(q.MaxRetry) {

			var tmp QueueItem = *item
			e := q.retryQueue.push(&tmp)
			if e != nil {
				ticker := time.NewTicker(time.Millisecond * 5)
				defer ticker.Stop()

				for i := 0; i < q.MaxRetry && i < 10 && !q.closed; i++ {
					// 写重试队列，写到成功为止
					e = q.retryQueue.push(&tmp)
					if e == nil {
						break
					}

					if q.closed {
						return
					}

					select {
					case <-ticker.C:
					case <-q.closeCh:
						return
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}

	if !isRetry {
		q.queue.logItem(item)
	} else {
		q.retryQueue.logItem(item)
	}
}

func (q *MQ) work(id int, isRetry bool) {

	defer q.wg.Done()

	var item *QueueItem
	var ret HANDLE_RESULT
	ctx := context.Background()
	for !q.closed {
		c, cf := context.WithTimeout(context.Background(), time.Millisecond*5)
		item, _ = q.pop(isRetry, true, c)
		cf()
		// e != nil , 数据有问题，继续取新的
		if item == nil {
			continue
		}
		if q.closed {
			return
		}

		ret = q.handle(id, isRetry, item)
		q.handleItemResult(isRetry, item, ret, ctx)
	}
}

func (q *MQ) handle(id int, isRetry bool, item *QueueItem) (r HANDLE_RESULT) {
	defer func() {
		if err := recover(); err != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			xlog.Errorf("panic: %v\n%s", err, buf)
			r = RESULT_ERROR
		}
	}()

	if q.HandlerV2 != nil {
		r = q.HandlerV2(id, isRetry, item)
	} else {
		r = q.Handler(item)
	}
	return
}

func (q *MQ) batchWork(id int, isRetry bool) {

	defer q.wg.Done()

	var item *QueueItem
	var items = make([]*QueueItem, 0, q.BatchMaxItem)
	var rets = make([]HANDLE_RESULT, 0, q.BatchMaxItem)
	ctx := context.Background()
	var totalSize int
	for !q.closed {
		items = items[0:0]
		rets = rets[0:0]
		totalSize = 0
		c, cf := context.WithTimeout(context.Background(), q.BatchWaitDuration)
		for len(items) < q.BatchMaxItem && !q.closed && c.Err() == nil {
			item, _ = q.pop(isRetry, true, c)
			if item != nil {
				items = append(items, item)
				rets = append(rets, result_unknown)

				if q.BatchMaxItemSize > 0 {
					totalSize += len(item.Data1) + len(item.Data2) + len(item.Data3) + len(item.Data4) + len(item.Data5)
					if totalSize >= q.BatchMaxItemSize {
						break
					}
				}
			} else if len(items) > 0 {
				break
			}
		}
		cf()

		if len(items) == 0 || q.closed {
			continue
		}

		q.batchHandle(id, isRetry, items, rets)
		for i, _ := range items {
			q.handleItemResult(isRetry, items[i], rets[i], ctx)
		}
	}
}

func (q *MQ) batchHandle(id int, isRetry bool, items []*QueueItem, rets []HANDLE_RESULT) {
	defer func() {
		if err := recover(); err != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			xlog.Errorf("panic: %v\n%s", err, buf)
			for i, r := range rets {
				if r == result_unknown {
					rets[i] = RESULT_ERROR
				}
			}
		}
	}()

	if q.BatchHandlerV2 != nil {
		q.BatchHandlerV2(id, isRetry, items, rets)
	} else {
		q.BatchHandler(items, rets)
	}
	for i, r := range rets {
		if r == result_unknown {
			rets[i] = RESULT_OK
		}
	}
}

func (q *MQ) Close() {

	if q.closed {
		return
	}
	q.closed = true
	close(q.closeCh)

	if q.Handler != nil || q.BatchHandler != nil || q.HandlerV2 != nil || q.BatchHandlerV2 != nil {
		time.Sleep(time.Millisecond * 50)

		xlog.Debugf("closing")

		q.wg.Wait()
	}

	q.queue.Close()
	if q.retryQueue != nil {
		q.retryQueue.Close()
	}

	xlog.Debugf("closed")
}

// 取item，没有数据不会等待，没有设置Handler时用.
//
// Err有可能是：
//
//	1. *ErrDelay：当取retry队列，而且retry有延迟时会返回*ErrDelay, 这时worker应该延迟一会再取.
//	2. ErrEmptyQueue，队列是空
// 	3. ErrQueueClosed
//	4. 其它
//
func (q *MQ) Pop(isRetryQueue bool) (item *QueueItem, e error) {

	if q.closed {
		return nil, ErrQueueClosed
	}

	if isRetryQueue {
		if q.retryQueue == nil {
			return nil, ErrEmptyQueue
		}
		item, e = q.retryQueue.pop()
	} else {
		item, e = q.queue.pop()
	}

	if item == nil && e == nil {
		e = ErrEmptyQueue
	}
	return
}

// 列队长度（item个数）
func (q *MQ) Size(isRetryQueue bool) int {

	if q.closed {
		return 0
	}

	if isRetryQueue {
		if q.retryQueue == nil {
			return 0
		}
		return q.retryQueue.size()
	}
	if q.queue == nil {
		return 0
	}
	return q.queue.size()
}

// 取状态，返回item个数，内存大小
func (q *MQ) Status(isRetryQueue bool) (cnt, memSize, memLeft int) {

	if q.closed {
		return
	}

	if isRetryQueue {
		if q.retryQueue == nil {
			return
		}
		return q.retryQueue.status()
	}
	if q.queue == nil {
		return
	}
	return q.queue.status()
}

// 设置处理结果，没有设置Handler时用.
func (q *MQ) SetResult(isRetryQueue bool, item *QueueItem, ret HANDLE_RESULT, ctx context.Context) error {
	if q.closed {
		return ErrQueueClosed
	}
	q.handleItemResult(isRetryQueue, item, ret, ctx)
	return nil
}

// 取item，如没有数据会等待，没有设置Handler时用.
//
// Err有可能是：
//
//	1. ErrQueueClosed
//	2. ErrContextCanceled
//
func (q *MQ) PopWait(isRetryQueue bool, ctx context.Context) (item *QueueItem, e error) {

	for !q.closed {
		item, e = q.pop(isRetryQueue, false, ctx)
		if item != nil {
			return item, nil
		}

		if e == nil {
			continue
		}
		return nil, e
	}
	return nil, ErrQueueClosed
}
