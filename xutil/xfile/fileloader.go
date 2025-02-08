package xfile

import (
	"github.com/ccxp/xgo/xlog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type FileLoader struct {
	FilePath              string
	CheckModifiedDuration time.Duration // 废弃，最小20s

	Read func(path string) error

	inited bool

	mtime    int64  // 当前使用数据的mtime
	mtimePtr *int64 // checker更新的时间
	lock     sync.Mutex
}

func (l *FileLoader) LoadIfModified() (relaoded bool, err error) {

	if l.FilePath == "" {
		return
	}

	if l.inited && atomic.LoadInt64(l.mtimePtr) == atomic.LoadInt64(&l.mtime) {
		return
	}

	l.lock.Lock()
	defer l.lock.Unlock()

	var ts int64

	if !l.inited {
		l.mtimePtr = innerchecker.Register(l.FilePath)
		ts = atomic.LoadInt64(l.mtimePtr)
		atomic.StoreInt64(&l.mtime, ts)
		l.inited = true
	} else {
		ts = atomic.LoadInt64(l.mtimePtr)
		if ts == l.mtime {
			return
		}
		atomic.StoreInt64(&l.mtime, ts)
	}

	if ts <= 0 {
		return
	}

	if l.Read != nil {
		err = l.Read(l.FilePath)
	}

	return true, err
}

func (l *FileLoader) SetFile(fpath string) {
	l.FilePath = fpath
}

/*********************/

// 单协程在后台做处理
type fileChangeChecker struct {
	files map[string]*int64 // 文件的mtime，内部使用
	mu    sync.RWMutex

	once sync.Once
}

// 注册一个新
func (cl *fileChangeChecker) Register(path string) (mtime *int64) {

	cl.once.Do(cl.init)

	xlog.Debugf("fileChangeChecker.register %v", path)

	var ok bool
	cl.mu.RLock()
	if mtime, ok = cl.files[path]; ok {
		cl.mu.RUnlock()
		return mtime
	}
	cl.mu.RUnlock()

	// 读一次
	ts := cl.stat(path)

	cl.mu.Lock()
	defer cl.mu.Unlock()

	if mtime, ok = cl.files[path]; ok {
		return mtime
	}

	mtime = &ts

	cl.files[path] = mtime
	return mtime
}

func (cl *fileChangeChecker) stat(path string) int64 {

	st, e := os.Stat(path)
	if e == nil {
		return st.ModTime().UnixNano()
	}

	return 0
}

func (cl *fileChangeChecker) loop() {
	ticker := time.NewTicker(time.Second * 20)
	defer ticker.Stop()

	m := make(map[string]*int64)
	var path string
	var ptr *int64
	var newMtime int64

	for {
		<-ticker.C

		cl.mu.RLock()
		for path, ptr = range cl.files {
			m[path] = ptr
		}
		cl.mu.RUnlock()

		for path, ptr = range m {
			newMtime = cl.stat(path)
			if newMtime > 0 && atomic.LoadInt64(ptr) != newMtime {
				atomic.StoreInt64(ptr, newMtime)
				xlog.Debugf("file %v changed", path)
			}
		}

	}
}

func (cl *fileChangeChecker) init() {
	go cl.loop()
}

var innerchecker = &fileChangeChecker{
	files: make(map[string]*int64),
}
