package xhttp

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ccxp/xgo/xlog"
)

func toHTTPError(err error) (msg string, httpStatus int) {
	if os.IsNotExist(err) {
		return "404 page not found", http.StatusNotFound
	}
	if os.IsPermission(err) {
		return "403 Forbidden", http.StatusForbidden
	}
	// Default:
	return "500 Internal Server Error", http.StatusInternalServerError
}

// file data
type contentData struct {
	Data    []byte
	Offset  int64
	Name    string
	ModTime time.Time
	Etag    string
}

// contentData初始化
func newContentData(b []byte, name string, modtime time.Time) *contentData {

	data := &contentData{
		Data:    b,
		Offset:  0,
		Name:    name,
		ModTime: modtime,
		Etag:    getEtag(b, modtime),
	}

	return data
}

func getEtag(b []byte, modtime time.Time) string {
	if len(b) > 0 {
		hash := sha1.Sum(b)
		return fmt.Sprintf("\"%v\"", hex.EncodeToString(hash[:]))
	}
	return fmt.Sprintf("\"%v\"", modtime.UnixNano())
}

// clone contentData and set offset to 0.
func copyContentData(other *contentData) *contentData {
	data := &contentData{
		Data:    other.Data,
		Offset:  0,
		Name:    other.Name,
		ModTime: other.ModTime,
		Etag:    other.Etag,
	}

	return data
}

func (d *contentData) Read(p []byte) (n int, err error) {
	if d.Offset >= int64(len(d.Data)) {
		return 0, io.EOF
	}
	n = copy(p, d.Data[d.Offset:])
	d.Offset += int64(n)
	return
}

func (d *contentData) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = d.Offset + offset
	case io.SeekEnd:
		abs = int64(len(d.Data)) + offset
	default:
		return 0, errors.New("strings.Reader.Seek: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("strings.Reader.Seek: negative position")
	}
	d.Offset = abs
	return abs, nil
}

// file handler.
//
// output 404 if path is directory.
type FileHandler struct {
	BaseDir   string // base file directory, endswith /
	UrlPrefix string // endswith /, for example: url="/a/b/c.png", UrlPrefix="/a/b/", then file path=BaseDir + "c.png"
	Index     string // 当请求路径是目录时，如果Index不为空，则直接添加Index的内容在路径后面

	// for cache header
	CacheControlHeader string // like max-age=60

	// EnableCache is for read whole file into memory, not for cache header.
	EnableCache      bool
	CacheFileMaxSize int64

	mapFile map[string]*contentData
	muFile  sync.RWMutex
}

func (h *FileHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	if strings.Contains(r.URL.Path, "/../") {
		http.NotFound(w, r)
		return
	}

	prefix := h.UrlPrefix
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}

	path := h.BaseDir
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	path += r.URL.Path[len(prefix):]
	if len(h.Index) > 0 && strings.HasSuffix(path, "/") {
		path += h.Index
	}

	if !h.EnableCache {
		f, err := os.Open(path)
		if err != nil {
			msg, code := toHTTPError(err)
			http.Error(w, msg, code)
			return
		}
		defer f.Close()

		d, err := os.Stat(path)
		if err != nil {
			xlog.Errorf("read static file %s fail: %v", path, err)
			msg, code := toHTTPError(err)
			http.Error(w, msg, code)
			return
		}

		if d.IsDir() {
			http.NotFound(w, r)
			return
		}

		if h.CacheControlHeader != "" {
			w.Header().Set("Etag", getEtag(nil, d.ModTime()))
			w.Header().Set("Cache-Control", h.CacheControlHeader)
		}
		http.ServeContent(w, r, d.Name(), d.ModTime(), f)
		return
	}

	var cache *contentData
	ok := false
	h.muFile.RLock()
	if h.mapFile != nil {
		cache, ok = h.mapFile[path]
	}
	h.muFile.RUnlock()

	if ok {
		data := copyContentData(cache) //Offset need  set to 0, make new object

		if h.CacheControlHeader != "" {
			w.Header().Set("Etag", data.Etag)
			w.Header().Set("Cache-Control", h.CacheControlHeader)
		}
		http.ServeContent(w, r, data.Name, data.ModTime, data)
		return
	}

	d, err := os.Stat(path)
	if err != nil {
		xlog.Errorf("read static file %s fail: %v", path, err)
		msg, code := toHTTPError(err)
		http.Error(w, msg, code)
		return
	}

	if d.IsDir() {
		http.NotFound(w, r)
		return
	}

	if h.CacheFileMaxSize > 0 && d.Size() > h.CacheFileMaxSize {
		f, err := os.Open(path)
		if err != nil {
			msg, code := toHTTPError(err)
			http.Error(w, msg, code)
			return
		}
		defer f.Close()

		if h.CacheControlHeader != "" {
			w.Header().Set("Etag", getEtag(nil, d.ModTime()))
			w.Header().Set("Cache-Control", h.CacheControlHeader)
		}
		http.ServeContent(w, r, d.Name(), d.ModTime(), f)
		return
	}

	data, err := ioutil.ReadFile(path)
	//logger.Error("ReadFile %s: %v", path, err)
	if err != nil {
		msg, code := toHTTPError(err)
		http.Error(w, msg, code)
		return
	}

	cache = newContentData(data, d.Name(), d.ModTime())
	h.muFile.Lock()
	if h.mapFile == nil {
		h.mapFile = make(map[string]*contentData)
	}
	h.mapFile[path] = cache
	h.muFile.Unlock()

	if h.CacheControlHeader != "" {
		w.Header().Set("Etag", cache.Etag)
		w.Header().Set("Cache-Control", h.CacheControlHeader)
	}
	http.ServeContent(w, r, cache.Name, cache.ModTime, cache)
}
