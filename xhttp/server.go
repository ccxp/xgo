// net.http enhancement.
package xhttp

import (
	"context"
	"crypto/tls"
	"errors"
	"strconv"

	"github.com/ccxp/xgo/xlog"
	"github.com/ccxp/xgo/xnet"
	"github.com/ccxp/xgo/xutil/xctx"
	"github.com/pierrec/lz4"

	//"net"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"
)

// --------------------------------RequestHelper

const (
	defaultMaxMemory = 32 << 20 // 32 MB
)

type RequestHelper struct {
	Req *http.Request
}

// Read data from Request.Body and decode by unmarshaler.
// Set Request.Body.Close() after read body.
func (h *RequestHelper) Unmarshal(unmarshaler func([]byte, interface{}) error, v interface{}) error {

	b, err := ioutil.ReadAll(h.Req.Body)
	defer h.Req.Body.Close()
	if err != nil {
		return err
	}

	return unmarshaler(b, v)
}

// Read data from Request.Body and decode by json.Unmarshal.
// Set Request.Body.Close() after read body.
func (h *RequestHelper) UnmarshalJSON(v interface{}) error {
	return h.Unmarshal(json.Unmarshal, v)
}

// Get URL query string value using Request.FormValue
func (h *RequestHelper) Form(key string, def string) string {
	if h.Req.Form == nil {
		h.Req.ParseMultipartForm(defaultMaxMemory)
	}
	if vs := h.Req.Form[key]; len(vs) > 0 {
		return vs[0]
	}
	return def
}

// Decode URL query string value in c.Request.Form into struct, *map[string]string or *map[string][]string.
//
// Struct field can be customized by the format string stored under the "query" key in the struct field's tag.
//
// Example:
//
//	type MyData struct {
//	    Aa int      `query:"aa"`
//	    Bb []string `query:"bb"`
//	    Cc *int     `query:"cc"`
//	    Dd []*int   `query:"dd"`
//	}
//
// If dat is *map[string]string, only store the first value in the same key.
//
// Request.ParseMultipartForm() will be call in FormUnmarshal.
func (h *RequestHelper) FormUnmarshal(dat interface{}) error {
	h.Req.ParseMultipartForm(defaultMaxMemory)
	return (*Values)(&h.Req.Form).Unmarshal(dat)
}

// --------------------------------ResponseHelper

type ResponseHelper struct {
	Resp *http.Response

	// Err return by RoundTrip
	Err error

	// if CheckStatusOK is true, and Resp.StatusCode != 200, helper will return fail.
	CheckStatusOK bool

	// if UseBodyClose is true, use defer Resp.Body.Close(),
	// the connection is closed where status code is not 200.
	// if UseBodyClose is false, use defer io.Copy(ioutil.Discard, h.Resp.Body).
	UseBodyClose bool
}

// Read data from Response.Body .
func (h *ResponseHelper) Body() ([]byte, error) {

	if h.Err != nil {
		return nil, h.Err
	}

	if h.UseBodyClose {
		defer h.Resp.Body.Close()
	} else {
		defer io.Copy(io.Discard, h.Resp.Body)
	}

	if h.CheckStatusOK && h.Resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Http Response status code is %v", h.Resp.StatusCode)
	}

	return io.ReadAll(h.Resp.Body)
}

// Read data from Response.Body and decode by unmarshaler.
func (h *ResponseHelper) Unmarshal(unmarshaler func([]byte, interface{}) error, v interface{}) error {

	b, err := h.Body()
	if err != nil {
		return err
	}

	return unmarshaler(b, v)
}

// Read data from Response.Body and decode by json.Unmarshal.
func (h *ResponseHelper) UnmarshalJSON(v interface{}) error {
	return h.Unmarshal(json.Unmarshal, v)
}

func (h *ResponseHelper) Error() error {
	if h.Err != nil {
		return h.Err
	}

	if h.CheckStatusOK && h.Resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Http Response status code is %v", h.Resp.StatusCode)
	}

	return nil
}

func (h *ResponseHelper) Close() {
	if h.UseBodyClose {
		h.Resp.Body.Close()
	} else {
		io.Copy(io.Discard, h.Resp.Body)
	}
}

type writeCloseFlusher interface {
	io.WriteCloser
	Flush() error
}

// ----------------------------------------------------------------ResponseWriter

// ResponseWriter implemets http.ResponseWriter, add output support.
//
// All ResponseWriter make by xhttp.Server is xhttp.ResponseWriter.
//
// Store values using http.Request.WithContext(http.Request.Context().WithValue()) will use reflect and deep copy,
// so we use ValueCtx to store your own values, and include RequestHelper in ResponseWriter.
type ResponseWriter struct {
	http.ResponseWriter

	TimeStamp time.Time // the timestamp that make this ResponseWriter

	// StatusCode set by WriteHeader
	StatusCode int

	// Store the data length using Write.
	BodySize int

	req *http.Request

	gZipLevel     int // Set response body gzip level. 0 is disable gzip.
	gZipMinLength int

	enableLz4           bool
	lz4CompressionLevel int
	lz4MinLength        int

	// set writerMode and zipWriteCloser on first write
	writerMode     int
	zipWriteCloser writeCloseFlusher
	zipWriteClosed bool

	ctx *xctx.ValueCtx
}

const (
	writerModeUnknown = iota
	writerModePlain
	writerModeGzip
	writerModeLz4
)

// Use for other http.Server implement.
func NewResponseWriter(w http.ResponseWriter, r *http.Request) *ResponseWriter {
	return newResponseWriter(w, r, true)
}

func newResponseWriter(w http.ResponseWriter, r *http.Request, check bool) *ResponseWriter {
	if check {
		if ww, ok := w.(*ResponseWriter); ok {
			return ww
		}
	}
	return &ResponseWriter{
		ResponseWriter: w,
		StatusCode:     0,
		req:            r,
		TimeStamp:      time.Now(),
	}
}

// Header returns the header map that will be sent by
// WriteHeader. The Header map also is the mechanism with which
// Handlers can set HTTP trailers.
func (w *ResponseWriter) Header() http.Header {
	return w.ResponseWriter.Header()
}

// Write writes the data to the connection as part of an HTTP reply.
//
// If WriteHeader has not yet been called, Write calls
// WriteHeader(http.StatusOK) before writing the data. If the Header
// does not contain a Content-Type line, Write adds a Content-Type set
// to the result of passing the initial 512 bytes of written data to
// DetectContentType. Additionally, if the total size of all written
// data is under a few KB and there are no Flush calls, the
// Content-Length header is added automatically.
//
// Depending on the HTTP protocol version and the client, calling
// Write or WriteHeader may prevent future reads on the
// Request.Body. For HTTP/1.x requests, handlers should read any
// needed request body data before writing the response. Once the
// headers have been flushed (due to either an explicit Flusher.Flush
// call or writing enough data to trigger a flush), the request body
// may be unavailable. For HTTP/2 requests, the Go HTTP server permits
// handlers to continue to read the request body while concurrently
// writing the response. However, such behavior may not be supported
// by all HTTP/2 clients. Handlers should read before writing if
// possible to maximize compatibility.
func (w *ResponseWriter) Write(b []byte) (int, error) {
	return w.writeGzipBody(b)
}

// WriteHeader sends an HTTP response header with the provided
// status code.
//
// If WriteHeader is not called explicitly, the first call to Write
// will trigger an implicit WriteHeader(http.StatusOK).
// Thus explicit calls to WriteHeader are mainly used to
// send error codes.
//
// The provided code must be a valid HTTP 1xx-5xx status code.
// Only one header may be written. Go does not currently
// support sending user-defined 1xx informational headers,
// with the exception of 100-continue response header that the
// Server sends automatically when the Request.Body is read.
func (w *ResponseWriter) WriteHeader(statusCode int) {
	w.StatusCode = statusCode

	// 如果直接WriteHeader，比如在ServContent中，就无法判断数据长度了
	if w.writerMode == writerModeUnknown {
		if w.gZipLevel != gzip.NoCompression && (w.StatusCode == 0 || w.StatusCode == 200) &&
			strings.Contains(w.req.Header.Get("Accept-Encoding"), "gzip") && w.Header().Get("Content-Encoding") == "" {
			var err error
			w.zipWriteCloser, err = gzip.NewWriterLevel(w.ResponseWriter, w.gZipLevel)
			if err != nil {
				w.writerMode = writerModePlain
			} else {
				w.writerMode = writerModeGzip
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Vary", "Accept-Encoding")

				w.Header().Del("Content-Length")
			}
		} else if w.enableLz4 && w.lz4CompressionLevel >= 0 && (w.StatusCode == 0 || w.StatusCode == 200) &&
			strings.Contains(w.req.Header.Get("Accept-Encoding"), "lz4") && w.Header().Get("Content-Encoding") == "" {
			zw := lz4.NewWriter(w.ResponseWriter)
			zw.Header.CompressionLevel = w.lz4CompressionLevel
			w.zipWriteCloser = zw
			w.writerMode = writerModeLz4
			w.Header().Set("Content-Encoding", "lz4")
			w.Header().Set("Vary", "Accept-Encoding")

			w.Header().Del("Content-Length")

		} else {
			w.writerMode = writerModePlain
		}
	}

	w.ResponseWriter.WriteHeader(statusCode)
}

// Get ValueCtx to fetch values.
func (w *ResponseWriter) ValCtx() *xctx.ValueCtx {
	if w.ctx == nil {
		w.ctx = &xctx.ValueCtx{}
	}
	return w.ctx
}

// According to context.Context, key should by static stuct instance,
// dont use string or int etc.
func (w *ResponseWriter) ValCtxSet(key interface{}, val interface{}) {
	w.ValCtx().Add(key, val)
}

func (w *ResponseWriter) ValCtxGet(key interface{}) interface{} {
	if w.ctx == nil {
		return nil
	}
	return w.ctx.Value(key)
}

// Set gzip level.
//
// ResponseWriter will check size on first write, if it is not using gzip on first write,
// there will be no gzip.
//
// CloseGzip must be used at the end of handler if not using xhttp.Server, xhttp.Handler,
// xhttp.ServePrefixMux and xhttp.ServeRESTfulMux.
//
// see https://golang.org/pkg/compress/gzip. 0 is disable gzip.
func (w *ResponseWriter) SetGzip(gziplevel, gzipMinLength int) {
	w.gZipLevel = gziplevel
	w.gZipMinLength = gzipMinLength
}

func (w *ResponseWriter) SetLz4(level, minLength int) {
	w.enableLz4 = true
	w.lz4CompressionLevel = level
	w.lz4MinLength = minLength
}

func (w *ResponseWriter) writeGzipBody(data []byte) (n int, err error) {

	// 这里是直接Write，没有调用WriteHeader时用，可以判断长度
	if w.writerMode == writerModeUnknown {
		if w.gZipLevel != gzip.NoCompression && len(data) >= w.gZipMinLength && (w.StatusCode == 0 || w.StatusCode == 200) &&
			strings.Contains(w.req.Header.Get("Accept-Encoding"), "gzip") && w.Header().Get("Content-Encoding") == "" {
			var err error
			w.zipWriteCloser, err = gzip.NewWriterLevel(w.ResponseWriter, w.gZipLevel)
			if err != nil {
				w.writerMode = writerModePlain
			} else {
				w.writerMode = writerModeGzip
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Vary", "Accept-Encoding")

				w.Header().Del("Content-Length")
			}
		} else if w.enableLz4 && w.lz4CompressionLevel >= 0 && len(data) >= w.lz4MinLength && (w.StatusCode == 0 || w.StatusCode == 200) &&
			strings.Contains(w.req.Header.Get("Accept-Encoding"), "lz4") && w.Header().Get("Content-Encoding") == "" {
			zw := lz4.NewWriter(w.ResponseWriter)
			zw.Header.CompressionLevel = w.lz4CompressionLevel
			w.zipWriteCloser = zw
			w.writerMode = writerModeLz4
			w.Header().Set("Content-Encoding", "lz4")
			w.Header().Set("Vary", "Accept-Encoding")

			w.Header().Del("Content-Length")
		} else {
			w.writerMode = writerModePlain
		}
	}

	if w.zipWriteCloser == nil {
		n, err = w.ResponseWriter.Write(data)
	} else {
		n, err = w.zipWriteCloser.Write(data)
	}
	w.BodySize += n
	return n, err
}

// serializes the given struct as JSON into the response body.
// and set Content-Type.
func (w *ResponseWriter) Marshal(marshaler func(interface{}) ([]byte, error), data interface{}, contentType string) (n int, err error) {

	var b []byte
	b, err = marshaler(data)
	if err != nil {
		return 0, err
	}

	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	if len(w.Header().Values("Content-Length")) == 0 {
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	}

	n, err = w.writeGzipBody(b)
	if err != nil {
		return n, err
	}

	return n, err
}

// serializes the given struct as JSON into the response body,
// and set Content-Type to "application/json".
func (w *ResponseWriter) MarshalJSON(data interface{}) (n int, err error) {
	return w.Marshal(json.Marshal, data, "application/json; charset=UTF-8")
}

// serializes the given struct as JSONP into the response body,
// set the Content-Type to "text/javascript".
func (w *ResponseWriter) MarshalJSONP(data interface{}, callback string, escapeHTML bool) (n int, err error) {

	buffer := &bytes.Buffer{}
	encoder := json.NewEncoder(buffer)
	encoder.SetEscapeHTML(escapeHTML)
	err = encoder.Encode(data)
	if err != nil {
		return 0, err
	}

	w.Header().Set("Content-Type", "text/javascript; charset=UTF-8")

	return fmt.Fprintf(w, "%s(%s)", callback, buffer.Bytes())
}

// serializes the given struct using tpl.Execute.
//
// if contentType is "", set Content-Type = "text/html; charset=UTF-8".
func (w *ResponseWriter) WriteTpl(data interface{}, tpl *template.Template, contentType string) (n int, err error) {
	buffer := &bytes.Buffer{}
	err = tpl.Execute(buffer, data)
	if err != nil {
		xlog.Errorf("WriteTpl fail: %v", err)
		return 0, err
	}

	if contentType == "" {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	} else {
		w.Header().Set("Content-Type", contentType)
	}

	return w.writeGzipBody(buffer.Bytes())
}

// if debugFormat == "json", use c.MarshalJSON.
//
// else use tpl.Execute.
//
// useful for debug.
func (w *ResponseWriter) WriteTplEx(data interface{}, tpl *template.Template, contentType, debugFormat string) (n int, err error) {
	switch debugFormat {
	case "json":
		return w.MarshalJSON(data)
	}
	return w.WriteTpl(data, tpl, contentType)
}

// WriteError replies to the request with the specified error message and HTTP code.
// It does not otherwise end the request;
// the caller should ensure no further writes are done to w. The error message should be plain text.
func (w *ResponseWriter) WriteError(err string, code int) {
	w.StatusCode = code
	http.Error(w, err, code)
}

// Get RESTful params.
//
// Same as w.ValCtxGet(xhttp.ParamKey).(*xhttp.Params)
func (w *ResponseWriter) Params() *Params {
	ps := w.ValCtxGet(ParamKey)
	if ps != nil {
		if ps, ok := ps.(*Params); ok {
			return ps
		}
	}
	return nil
}

// Get mux match pattern.
//
// Same as w.ValCtxGet(xhttp.PatternKey).(string)
func (w *ResponseWriter) Pattern() string {
	p := w.ValCtxGet(PatternKey)
	if p != nil {
		if p, ok := p.(string); ok {
			return p
		}
	}
	return ""
}

var copyBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 32*1024)
		return &b
	},
}

// errInvalidWrite means that a write returned an impossible count.
var errInvalidWrite = errors.New("invalid write result")

func (w *ResponseWriter) ReadFrom(src io.Reader) (written int64, err error) {
	bufp := copyBufPool.Get().(*[]byte)
	buf := *bufp
	defer copyBufPool.Put(bufp)

	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := w.Write(buf[0:nr])
			if nw < 0 || nr < nw {
				nw = 0
				if ew == nil {
					ew = errInvalidWrite
				}
			}
			written += int64(nw)
			if ew != nil {
				err = ew
				break
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
		}
		if er != nil {
			if er != io.EOF {
				err = er
			}
			break
		}
	}
	return written, err

}

func (w *ResponseWriter) Flush() {
	if w.zipWriteCloser != nil && !w.zipWriteClosed {
		w.zipWriteCloser.Flush()
	}
	w.ResponseWriter.(http.Flusher).Flush()
}

func (w *ResponseWriter) Hijack() (rwc net.Conn, buf *bufio.ReadWriter, err error) {
	return w.ResponseWriter.(http.Hijacker).Hijack()
}

// CloseZip at the end of handler if not using xhttp.Server, xhttp.Handler,
// xhttp.ServePrefixMux and xhttp.ServeRESTfulMux.
func (w *ResponseWriter) CloseZip() error {
	if w.zipWriteCloser != nil && !w.zipWriteClosed {
		w.zipWriteCloser.Flush()
		w.zipWriteClosed = true
		return w.zipWriteCloser.Close()

	}
	return nil
}

// ----------------------------------------------------------------Server

// Base on http.Server and add balancer, tracer support.
type Server struct {
	http.Server

	Balancer *ServerBalancer
	Tracer   *ServerTracer

	// Set gzip level.
	//
	// see https://golang.org/pkg/compress/gzip. 0 is disable gzip.
	GzipLevel int

	// Disable gzip if body length less than GzipMinLength
	GzipMinLength int

	inited bool
	// mockConnState *mockConnState

	// connStatMu sync.Mutex
	// connStat   map[string]*xnet.ConnState
}

/*
func (srv *Server) markConnState(c net.Conn, info *xnet.ConnState) {

	srv.connStatMu.Lock()
	srv.connStat[c.RemoteAddr().String()] = info
	srv.connStatMu.Unlock()
}

func (srv *Server) getConnState(remoteAddr string, del bool) (info *xnet.ConnState) {

	srv.connStatMu.Lock()
	info = srv.connStat[remoteAddr]
	if del && info != nil {
		delete(srv.connStat, remoteAddr)
	}
	srv.connStatMu.Unlock()
	if info == nil {
		return &xnet.ConnState{}
	}
	return
}
*/

func (srv *Server) mockListener(l net.Listener) *mockListener {
	r := &mockListener{
		b: srv.Balancer,
		t: srv.Tracer,
	}

	r.l = l

	return r
}

// ListenAndServe listens on the TCP network address srv.Addr and then
// calls Serve to handle requests on incoming connections.
// Accepted connections are configured to enable TCP keep-alives.
//
// If srv.Addr is blank, ":http" is used.
//
// ListenAndServe always returns a non-nil error. After Shutdown or Close,
// the returned error is ErrServerClosed.
func (srv *Server) ListenAndServe() error {
	addr := srv.Addr
	if addr == "" {
		addr = ":http"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	return srv.Serve(ln)
}

// ListenAndServeTLS listens on the TCP network address srv.Addr and
// then calls Serve to handle requests on incoming TLS connections.
// Accepted connections are configured to enable TCP keep-alives.
//
// Filenames containing a certificate and matching private key for the
// server must be provided if neither the Server's TLSConfig.Certificates
// nor TLSConfig.GetCertificate are populated. If the certificate is
// signed by a certificate authority, the certFile should be the
// concatenation of the server's certificate, any intermediates, and
// the CA's certificate.
//
// If srv.Addr is blank, ":https" is used.
//
// ListenAndServeTLS always returns a non-nil error.
func (srv *Server) ListenAndServeTLS(certFile, keyFile string) error {

	addr := srv.Addr
	if addr == "" {
		addr = ":https"
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	defer ln.Close()

	return srv.ServeTLS(ln, certFile, keyFile)
}

// Serve accepts incoming connections on the Listener l, creating a
// new service goroutine for each. The service goroutines read requests and
// then call srv.Handler to reply to them.
//
// HTTP/2 support is only enabled if the Listener returns *tls.Conn
// connections and they were configured with "h2" in the TLS
// Config.NextProtos.
//
// Serve always returns a non-nil error and closes l.
// After Shutdown or Close, the returned error is ErrServerClosed.
func (srv *Server) Serve(l net.Listener) error {
	srv.initServer()
	return srv.Server.Serve(srv.mockListener(l))
}

// ServeTLS accepts incoming connections on the Listener l, creating a
// new service goroutine for each. The service goroutines perform TLS
// setup and then read requests, calling srv.Handler to reply to them.
//
// Files containing a certificate and matching private key for the
// server must be provided if neither the Server's
// TLSConfig.Certificates nor TLSConfig.GetCertificate are populated.
// If the certificate is signed by a certificate authority, the
// certFile should be the concatenation of the server's certificate,
// any intermediates, and the CA's certificate.
//
// ServeTLS always returns a non-nil error. After Shutdown or Close, the
// returned error is ErrServerClosed.
func (srv *Server) ServeTLS(l net.Listener, certFile, keyFile string) error {
	srv.initServer()
	return srv.Server.ServeTLS(srv.mockListener(l), certFile, keyFile)
}

func (srv *Server) initServer() {
	if srv.inited {
		return
	}

	if _, ok := srv.Handler.(*mockBaseHandler); !ok {
		srv.Handler = &mockBaseHandler{
			h:   srv.Handler,
			b:   srv.Balancer,
			t:   srv.Tracer,
			svr: srv,
		}
	}

	if srv.Balancer != nil || srv.Tracer != nil {
		// srv.mockConnState = &mockConnState{srv.Balancer, srv.Tracer, srv, srv.ConnState}
		// srv.ConnState = srv.mockConnState.ConnState

		cc := &connContext{f: srv.ConnContext}
		srv.ConnContext = cc.mock
	}
	// srv.connStat = make(map[string]*xnet.ConnState)
	srv.inited = true
}

// Set new handler.
// Do not set new handler using Srv.Handler = xxx directly.
func (srv *Server) SetHandler(handler http.Handler) {
	if _, ok := handler.(*mockBaseHandler); !ok {
		srv.Handler = &mockBaseHandler{
			h:   handler,
			b:   srv.Balancer,
			t:   srv.Tracer,
			svr: srv,
		}
	}
}

// ----------------------------------------------------------------mockBaseHandler

type mockBaseHandler struct {
	h   http.Handler
	b   *ServerBalancer
	t   *ServerTracer
	svr *Server
}

func (h *mockBaseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	tmp := r.Context().Value(connStateKey)
	var state *xnet.ConnState
	if tmp == nil {
		state = &xnet.ConnState{}
	} else {
		state = tmp.(*xnet.ConnState)
	}

	state.TlsState = r.TLS

	if state.Conn != nil {
		if h.b != nil && h.b.StateActive != nil {
			h.b.StateActive(state.Conn)
		}
	}

	ts := time.Now()
	mockW := &ResponseWriter{
		ResponseWriter: w,
		StatusCode:     0,
		req:            r,
		gZipLevel:      h.svr.GzipLevel,
		gZipMinLength:  h.svr.GzipMinLength,
		TimeStamp:      ts,
	}
	w = mockW

	reject := false
	if h.b != nil && h.b.StateNewRequest != nil {
		if e := h.b.StateNewRequest(w, r); e != nil {
			if h.t != nil && h.t.StateNewRequestReject != nil {
				h.t.StateNewRequestReject(w, r, e)
			}
			reject = true
		}
	}

	ctxx := &xctx.ValueCtx{}
	defer func() {
		mockW.CloseZip()
		if (h.b != nil && h.b.StateEndRequest != nil) || (h.t != nil && h.t.StateEndRequest != nil) {
			du := time.Since(ts)

			if h.b != nil && h.b.StateEndRequest != nil {
				h.b.StateEndRequest(w, r, du, state)
			}
			if h.t != nil && h.t.StateEndRequest != nil {
				h.t.StateEndRequest(w, r, du, state)
			}
			if h.t != nil && h.t.StateEndRequestWithValue != nil {
				h.t.StateEndRequestWithValue(w, r, du, state, ctxx)
			}
		}

		if state.Conn != nil {
			if h.b != nil && h.b.StateIdle != nil {
				h.b.StateIdle(state.Conn, state)
			}
			if h.t != nil && h.t.StateIdle != nil {
				h.t.StateIdle(state.Conn, state)
			}
		}

		state.Reset()
	}()

	if !reject {
		if h.t != nil && h.t.StateNewRequest != nil {
			h.t.StateNewRequest(w, r)
		}
		if h.t != nil && h.t.StateNewRequestWithValue != nil {
			h.t.StateNewRequestWithValue(w, r, ctxx)
		}

		h.h.ServeHTTP(w, r)
	}
}

// ----------------------------------------------------------------mockConnState
/*
type mockConnState struct {
	b *ServerBalancer
	t *ServerTracer
	s *Server
	c func(net.Conn, http.ConnState)
}

func (s *mockConnState) ConnState(c net.Conn, state http.ConnState) {

	switch state {

	case http.StateNew:
		if s.b != nil && s.b.StateNew != nil {
			s.b.StateNew(c)
		}
		if s.t != nil && s.t.StateNew != nil {
			s.t.StateNew(c)
		}

	case http.StateActive:
		if s.b != nil && s.b.StateActive != nil {
			s.b.StateActive(c)
		}

	case http.StateIdle:
		state := s.s.getConnState(c.RemoteAddr().String(), false)
		if s.b != nil && s.b.StateIdle != nil {
			s.b.StateIdle(c, state)
		}
		if s.t != nil && s.t.StateIdle != nil {
			s.t.StateIdle(c, state)
		}
		state.Reset()
	case http.StateClosed:
		state := s.s.getConnState(c.RemoteAddr().String(), true)
		if s.b != nil && s.b.StateClosed != nil {
			s.b.StateClosed(c, state)
		}
		if s.t != nil && s.t.StateClosed != nil {
			s.t.StateClosed(c, state)
		}
		state.Reset()
	}

	if s.c != nil {
		s.c(c, state)
	}
}
*/
// ----------------------------------------------------------------mockListener

type mockConn struct {
	net.Conn
	info *xnet.ConnState

	b *ServerBalancer
	t *ServerTracer
}

func (c *mockConn) Read(b []byte) (n int, err error) {
	n, err = c.Conn.Read(b)
	c.info.ReadBytes += int64(n)
	c.info.TotalReadBytes += int64(n)
	return
}

func (c *mockConn) Write(b []byte) (n int, err error) {
	n, err = c.Conn.Write(b)
	c.info.WriteBytes += int64(n)
	c.info.TotalWriteBytes += int64(n)
	return
}

func (c *mockConn) Close() error {

	e := c.Conn.Close()

	if c.b != nil && c.b.StateClosed != nil {
		c.b.StateClosed(c.Conn, c.info)
	}
	if c.t != nil && c.t.StateClosed != nil {
		c.t.StateClosed(c.Conn, c.info)
	}
	c.info.Reset()

	return e
}

type closeWriter interface {
	CloseWrite() error
}

func (c *mockConn) CloseWrite() error {
	if c, ok := c.Conn.(closeWriter); ok {
		return c.CloseWrite()
	}
	return nil
}

type mockListener struct {
	l net.Listener

	b *ServerBalancer
	t *ServerTracer
}

func (l *mockListener) Accept() (c net.Conn, e error) {

	for {
		if l.b != nil && l.b.AcceptPrepare != nil {
			l.b.AcceptPrepare()
		}

		c, e = l.l.Accept()
		if e != nil {
			return nil, e
		}

		if l.b != nil && l.b.Accept != nil {
			e = l.b.Accept(c)
			if e != nil {
				if l.t != nil && l.t.StateAcceptReject != nil {
					l.t.StateAcceptReject(c, e)
				}
				c.Close()
				continue
			}
		}

		if l.b != nil && l.b.StateNew != nil {
			l.b.StateNew(c)
		}
		if l.t != nil && l.t.StateNew != nil {
			l.t.StateNew(c)
		}

		// 生成一个新的ConnState，可在connContext中获取，写进req的ctx里面
		state := &xnet.ConnState{Conn: c}

		mockConn := &mockConn{
			Conn: c,
			info: state,

			b: l.b,
			t: l.t,
		}

		return mockConn, nil
	}
}

func (l *mockListener) Close() error {

	return l.l.Close()
}

func (l *mockListener) Addr() net.Addr {

	return l.l.Addr()
}

// ----------------------------------------------------------------mockListener

type connStateKeySt struct{}

var connStateKey connStateKeySt

type connContext struct {
	f func(ctx context.Context, c net.Conn) context.Context
}

func (cc connContext) mock(ctx context.Context, c net.Conn) context.Context {
	if cc.f != nil {
		ctx = cc.f(ctx, c)
	}

	var info *xnet.ConnState
	if mc, ok := c.(*mockConn); ok {
		info = mc.info
	} else if tlsc, ok := c.(*tls.Conn); ok {
		if mc, ok := tlsc.NetConn().(*mockConn); ok {
			info = mc.info
		}
	}

	if info == nil {
		// 一般不会拿到，万一呢
		info = &xnet.ConnState{Conn: c}
	}

	return context.WithValue(ctx, connStateKey, info)
}

// ----------------------------------------------------------------Server Balancer

type ServerBalancer struct {

	// 在调用listener.Accept前调用一次
	AcceptPrepare func()

	// 在listener.Accept后调用一次，由用户确定是否接受新连接，返回err != nil就是拒绝.
	Accept func(net.Conn) error

	// listener.Accept后调用一次
	StateNew func(net.Conn)

	// 从空闲连接读取到数据后调用一次
	StateActive func(net.Conn)

	// 在handler.ServeHTTP前调用一次，返回nil则决绝请求，注意使用需要自行设置返回信息.
	StateNewRequest func(http.ResponseWriter, *http.Request) error

	// 在handler.ServeHTTP后调用一次
	StateEndRequest func(http.ResponseWriter, *http.Request, time.Duration, *xnet.ConnState)

	// 请求处理完，连接状态变成空闲时调用一次
	StateIdle func(net.Conn, *xnet.ConnState)

	// 连接关闭时调用一次
	StateClosed func(net.Conn, *xnet.ConnState)
}

// ----------------------------------------------------------------Server Tracer

type ServerTracer struct {

	// balancer.Accept拒绝连接时调用一次
	StateAcceptReject func(net.Conn, error)

	// 新连接
	StateNew func(net.Conn)

	// Balancer.StateNewRequest拒绝请求时调用一次
	StateNewRequestReject func(http.ResponseWriter, *http.Request, error)

	/***** 以下是xhttp.Handler（主handler）的内部处理 *****/

	// 读取到新请求
	StateNewRequest func(http.ResponseWriter, *http.Request)
	// 读取到新请求（带ValueCtx的版本，可自行记录一些数据）
	StateNewRequestWithValue func(http.ResponseWriter, *http.Request, *xctx.ValueCtx)

	// 请求处理结束
	StateEndRequest func(http.ResponseWriter, *http.Request, time.Duration, *xnet.ConnState)
	// 请求处理结束（带ValueCtx的版本，可自行记录一些数据）
	StateEndRequestWithValue func(http.ResponseWriter, *http.Request, time.Duration, *xnet.ConnState, *xctx.ValueCtx)

	// 连接状态变成空闲
	StateIdle func(net.Conn, *xnet.ConnState)
	// 连接关闭
	StateClosed func(net.Conn, *xnet.ConnState)

	/***** 以下在xhttp.Handler.ServeHTTP内的记录点 *****/

	StateHandlerBegin         func(http.ResponseWriter, *http.Request, time.Duration)
	StateHandlerEnd           func(http.ResponseWriter, *http.Request, time.Duration)
	StateHandlerPrepareReject func(http.ResponseWriter, *http.Request, time.Duration, error)
	StateHandlerPanic         func(http.ResponseWriter, *http.Request, time.Duration, interface{})

	/***** 以下是mux的记录点 *****/
	StateHandlerNotFound func(http.ResponseWriter, *http.Request, time.Duration)
}

func CombineServerTracer(a *ServerTracer, b *ServerTracer) *ServerTracer {
	t := &ServerTracer{}

	if a.StateAcceptReject != nil || b.StateAcceptReject != nil {
		t.StateAcceptReject = func(c net.Conn, e error) {
			if a.StateAcceptReject != nil {
				a.StateAcceptReject(c, e)
			}
			if b.StateAcceptReject != nil {
				b.StateAcceptReject(c, e)
			}
		}
	}

	if a.StateNew != nil || b.StateNew != nil {
		t.StateNew = func(c net.Conn) {
			if a.StateNew != nil {
				a.StateNew(c)
			}
			if b.StateNew != nil {
				b.StateNew(c)
			}
		}
	}

	if a.StateNewRequestReject != nil || b.StateNewRequestReject != nil {
		t.StateNewRequestReject = func(w http.ResponseWriter, r *http.Request, e error) {
			if a.StateNewRequestReject != nil {
				a.StateNewRequestReject(w, r, e)
			}
			if b.StateNewRequestReject != nil {
				b.StateNewRequestReject(w, r, e)
			}
		}
	}

	if a.StateNewRequest != nil || b.StateNewRequest != nil {
		t.StateNewRequest = func(w http.ResponseWriter, r *http.Request) {
			if a.StateNewRequest != nil {
				a.StateNewRequest(w, r)
			}
			if b.StateNewRequest != nil {
				b.StateNewRequest(w, r)
			}
		}
	}

	if a.StateEndRequest != nil || b.StateEndRequest != nil {
		t.StateEndRequest = func(w http.ResponseWriter, r *http.Request, d time.Duration, s *xnet.ConnState) {
			if a.StateEndRequest != nil {
				a.StateEndRequest(w, r, d, s)
			}
			if b.StateEndRequest != nil {
				b.StateEndRequest(w, r, d, s)
			}
		}
	}

	if a.StateIdle != nil || b.StateIdle != nil {
		t.StateIdle = func(c net.Conn, s *xnet.ConnState) {
			if a.StateIdle != nil {
				a.StateIdle(c, s)
			}
			if b.StateIdle != nil {
				b.StateIdle(c, s)
			}
		}
	}

	if a.StateClosed != nil || b.StateClosed != nil {
		t.StateClosed = func(c net.Conn, s *xnet.ConnState) {
			if a.StateClosed != nil {
				a.StateClosed(c, s)
			}
			if b.StateClosed != nil {
				b.StateClosed(c, s)
			}
		}
	}

	if a.StateHandlerBegin != nil || b.StateHandlerBegin != nil {
		t.StateHandlerBegin = func(w http.ResponseWriter, r *http.Request, d time.Duration) {
			if a.StateHandlerBegin != nil {
				a.StateHandlerBegin(w, r, d)
			}
			if b.StateHandlerBegin != nil {
				b.StateHandlerBegin(w, r, d)
			}
		}
	}

	if a.StateHandlerPrepareReject != nil || b.StateHandlerPrepareReject != nil {
		t.StateHandlerPrepareReject = func(w http.ResponseWriter, r *http.Request, d time.Duration, e error) {
			if a.StateHandlerPrepareReject != nil {
				a.StateHandlerPrepareReject(w, r, d, e)
			}
			if b.StateHandlerPrepareReject != nil {
				b.StateHandlerPrepareReject(w, r, d, e)
			}
		}
	}

	if a.StateHandlerEnd != nil || b.StateHandlerEnd != nil {
		t.StateHandlerEnd = func(w http.ResponseWriter, r *http.Request, d time.Duration) {
			if a.StateHandlerEnd != nil {
				a.StateHandlerEnd(w, r, d)
			}
			if b.StateHandlerEnd != nil {
				b.StateHandlerEnd(w, r, d)
			}
		}
	}

	if a.StateHandlerPanic != nil || b.StateHandlerPanic != nil {
		t.StateHandlerPanic = func(w http.ResponseWriter, r *http.Request, d time.Duration, e interface{}) {
			if a.StateHandlerPanic != nil {
				a.StateHandlerPanic(w, r, d, e)
			}
			if b.StateHandlerPanic != nil {
				b.StateHandlerPanic(w, r, d, e)
			}
		}
	}

	if a.StateHandlerNotFound != nil || b.StateHandlerNotFound != nil {
		t.StateHandlerNotFound = func(w http.ResponseWriter, r *http.Request, d time.Duration) {
			if a.StateHandlerNotFound != nil {
				a.StateHandlerNotFound(w, r, d)
			}
			if b.StateHandlerNotFound != nil {
				b.StateHandlerNotFound(w, r, d)
			}
		}
	}

	if a.StateNewRequestWithValue != nil || b.StateNewRequestWithValue != nil {
		t.StateNewRequestWithValue = func(w http.ResponseWriter, r *http.Request, ctxx *xctx.ValueCtx) {
			if a.StateNewRequestWithValue != nil {
				a.StateNewRequestWithValue(w, r, ctxx)
			}
			if b.StateNewRequestWithValue != nil {
				b.StateNewRequestWithValue(w, r, ctxx)
			}
		}
	}

	if a.StateEndRequestWithValue != nil || b.StateEndRequestWithValue != nil {
		t.StateEndRequestWithValue = func(w http.ResponseWriter, r *http.Request, d time.Duration, s *xnet.ConnState, ctxx *xctx.ValueCtx) {
			if a.StateEndRequestWithValue != nil {
				a.StateEndRequestWithValue(w, r, d, s, ctxx)
			}
			if b.StateEndRequestWithValue != nil {
				b.StateEndRequestWithValue(w, r, d, s, ctxx)
			}
		}
	}

	return t
}

// ----------------------------------------------------------------Handler

// Extend from other handler, add prepare, tracer suport.
type Handler struct {
	Init    func(http.ResponseWriter, *http.Request)
	Handler http.Handler

	// Prepare is called before Handler.ServeHTTP,
	// It will not call Handler.ServeHTTP, if return error is not nil.
	Prepare func(http.ResponseWriter, *http.Request) error

	Tracer *ServerTracer

	// Panic is called after handler cause panic.
	Panic func(http.ResponseWriter, *http.Request, interface{})
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	var ww *ResponseWriter
	var ok bool
	if ww, ok = w.(*ResponseWriter); !ok {
		ww = newResponseWriter(w, r, false)
		w = ww
	}

	if h.Init != nil {
		h.Init(w, r)
	}

	if h.Tracer != nil && h.Tracer.StateHandlerBegin != nil {
		h.Tracer.StateHandlerBegin(w, r, time.Since(ww.TimeStamp))
	}

	defer func() {
		if err := recover(); err != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			xlog.Errorf("panic serving %v: %v\n%s", r.URL.Path, err, buf)

			if h.Panic != nil {
				h.Panic(w, r, err)
			} else {
				http.Error(w, "500 Internal Server Error", http.StatusInternalServerError)
			}
			if h.Tracer != nil && h.Tracer.StateHandlerPanic != nil {
				h.Tracer.StateHandlerPanic(w, r, time.Since(ww.TimeStamp), err)
			}
		}
		if h.Tracer != nil && h.Tracer.StateHandlerEnd != nil {
			h.Tracer.StateHandlerEnd(w, r, time.Since(ww.TimeStamp))
		}
	}()

	if h.Prepare != nil {
		err := h.Prepare(ww, r)
		if err != nil {
			if h.Tracer != nil && h.Tracer.StateHandlerPrepareReject != nil {
				h.Tracer.StateHandlerPrepareReject(w, r, time.Since(ww.TimeStamp), err)
			}
			return
		}
	}

	h.Handler.ServeHTTP(w, r)
	ww.CloseZip()
}

func PrepareCombine(a, b func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
	return func(w http.ResponseWriter, r *http.Request) error {
		e := a(w, r)
		if e != nil {
			return e
		}
		return b(w, r)
	}
}

// Add CORS support to handler.
//
// If will call handler.ServeHTTP if request.Method is not OPTIONS.
type CORSHandler struct {
	AllowOrigin  []string
	AllowHeaders string
	AllowMethods string
	Handler      http.Handler
}

func (h *CORSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	for _, domain := range h.AllowOrigin {
		if origin == domain {
			w.Header().Set("Access-Control-Allow-Headers", h.AllowHeaders)
			w.Header().Set("Access-Control-Allow-Methods", h.AllowMethods)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Timing-Allow-Origin", origin)
			break
		}
	}

	if r.Method == "OPTIONS" || h.Handler == nil {
		return
	}
	h.Handler.ServeHTTP(w, r)
}

type paramKey struct{}
type patternKey struct{}

var (
	// Used for ResponseWriter.ValCtxGet() to get RESTful params.
	ParamKey paramKey
	// Used for ResponseWriter.ValCtxGet() to get mux match pattern.
	PatternKey patternKey
)
