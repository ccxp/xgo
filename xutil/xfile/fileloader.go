package xfile

import (
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type FileLoader struct {
	FilePath              string
	CheckModifiedDuration time.Duration

	Read func(path string) error

	filePath  string
	fileMtime int64

	expireTime int64
	lock       sync.Mutex
}

func (l *FileLoader) LoadIfModified() (relaoded bool, err error) {

	if l.FilePath == "" {
		return false, nil
	}

	curr := time.Now().UnixNano()
	var expireTime int64

	if l.CheckModifiedDuration > 0 {
		expireTime = atomic.LoadInt64(&l.expireTime)
		if expireTime > curr {
			return false, nil
		}
	}

	d, err := os.Stat(l.FilePath)
	if err != nil {
		if l.CheckModifiedDuration > 0 {
			atomic.CompareAndSwapInt64(&l.expireTime, expireTime, curr+int64(l.CheckModifiedDuration))
		}
		return false, err
	}
	newTime := d.ModTime().UnixNano()

	fileMtime := atomic.LoadInt64(&l.fileMtime)
	if l.filePath == l.FilePath && fileMtime == newTime {
		if l.CheckModifiedDuration > 0 {
			atomic.CompareAndSwapInt64(&l.expireTime, expireTime, curr+int64(l.CheckModifiedDuration))
		}
		return false, nil
	}

	l.lock.Lock()
	defer l.lock.Unlock()

	if l.CheckModifiedDuration > 0 {
		// 可能已经其它协程读取过了
		expireTime = atomic.LoadInt64(&l.expireTime)
		if expireTime > curr {
			return false, nil
		}
	}

	if l.Read != nil {
		err = l.Read(l.FilePath)
	}

	l.filePath = l.FilePath
	atomic.StoreInt64(&l.fileMtime, newTime)

	if l.CheckModifiedDuration > 0 {
		atomic.CompareAndSwapInt64(&l.expireTime, expireTime, curr+int64(l.CheckModifiedDuration))
	}

	return true, err
}

func (l *FileLoader) SetFile(fpath string) {
	l.FilePath = fpath
}
