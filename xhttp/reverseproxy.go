package xhttp

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ccxp/xgo/xlog"
	"golang.org/x/net/http/httpguts"
)

// ReverseProxyHandler is an HTTP Handler that takes an incoming request and
// sends it to another server, proxying the response back to the
// client.
//
// Base on net.http.httputil.ReverseProxy
type ReverseProxyHandler struct {
	// Director must be a function which modifies
	// the request into a new request to be sent
	// using Transport. Its response is then copied
	// back to the original client unmodified.
	// Director must not access the provided Request
	// after returning.
	//
	// If Director return error, ReverseProxyHandler will stop handling.
	Director func(w http.ResponseWriter, r *http.Request, target *http.Request) error

	// RoundTrip is used to perform proxy requests.
	// If nil, http.DefaultTransport is used.
	RoundTrip func(w http.ResponseWriter, r *http.Request, target *http.Request) (*http.Response, error)

	// ModifyResponse is an optional function that modifies the
	// Response from the backend. It is called if the backend
	// returns a response at all, with any HTTP status code.
	// If the backend is unreachable, the optional ErrorHandler is
	// called without any call to ModifyResponse.
	//
	// If ModifyResponse returns an error, ErrorHandler is called
	// with its error value. If ErrorHandler is nil, its default
	// implementation is used.
	//
	// You should only modify targetResp in ModifyResponse.
	ModifyResponse func(w http.ResponseWriter, r *http.Request, target *http.Request, targetResp *http.Response) error

	// ErrorHandler is an optional function that handles errors
	// reaching the backend or errors from ModifyResponse.
	//
	// If nil, the default is to log the provided error and return
	// a 502 Status Bad Gateway response.
	ErrorHandler func(w http.ResponseWriter, r *http.Request, target *http.Request, e error)

	// FlushInterval specifies the flush interval
	// to flush to the client while copying the
	// response body.
	// If zero, no periodic flushing is done.
	// A negative value means to flush immediately
	// after each write to the client.
	// The FlushInterval is ignored when ReverseProxy
	// recognizes a response as a streaming response, or
	// if its ContentLength is -1; for such responses, writes
	// are flushed to the client immediately.
	FlushInterval time.Duration

	// BufferPool optionally specifies a buffer pool to
	// get byte slices for use by io.CopyBuffer when
	// copying HTTP response bodies.
	BufferPool BufferPool

	// 自定义copy buffer处理
	CopyBuffer func(resp *http.Response, dst io.Writer, src io.Reader, buf []byte) (int64, error)

	// upgrade的连接可以需要单独设置超时时间
	UpgradeReadTimeout  time.Duration
	UpgradeWriteTimeout time.Duration
}

// A BufferPool is an interface for getting and returning temporary
// byte slices for use by io.CopyBuffer.
type BufferPool interface {
	Get() []byte
	Put([]byte)
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func cloneHeader(h http.Header) http.Header {
	h2 := make(http.Header, len(h))
	for k, vv := range h {
		vv2 := make([]string, len(vv))
		copy(vv2, vv)
		h2[k] = vv2
	}
	return h2
}

// Hop-by-hop headers. These are removed when sent to the backend.
// As of RFC 7230, hop-by-hop headers are required to appear in the
// Connection header field. These are the headers defined by the
// obsoleted RFC 2616 (section 13.5.1) and are used for backward
// compatibility.
var hopHeaders = []string{
	"Connection",
	"Proxy-Connection", // non-standard but still sent by libcurl and rejected by e.g. google
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",      // canonicalized version of "TE"
	"Trailer", // not Trailers per URL above; https://www.rfc-editor.org/errata_search.php?eid=4522
	"Transfer-Encoding",
	"Upgrade",
}

func (p *ReverseProxyHandler) defaultErrorHandler(w http.ResponseWriter, r *http.Request, target *http.Request, err error) {
	p.logf("ReverseProxyHandler: handler %s error: %v", r.URL.Path, err)
	w.WriteHeader(http.StatusBadGateway)
}

func (p *ReverseProxyHandler) getErrorHandler() func(w http.ResponseWriter, r *http.Request, target *http.Request, e error) {
	if p.ErrorHandler != nil {
		return p.ErrorHandler
	}
	return p.defaultErrorHandler
}

func defaultRoundTrip(w http.ResponseWriter, r *http.Request, target *http.Request) (*http.Response, error) {
	return http.DefaultTransport.RoundTrip(target)
}

func (p *ReverseProxyHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	roundTrip := p.RoundTrip
	if roundTrip == nil {
		roundTrip = defaultRoundTrip
	}

	ctx := req.Context()
	/*if cn, ok := rw.(http.CloseNotifier); ok { // 没用到，暂时不加
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		notifyChan := cn.CloseNotify()
		go func() {
			select {
			case <-notifyChan:
				cancel()
			case <-ctx.Done():
			}
		}()
	}*/

	outreq := req.WithContext(ctx) // includes shallow copies of maps, but okay
	if req.ContentLength == 0 {
		outreq.Body = nil // Issue 16036: nil Body for http.Transport retries
	}

	outreq.Header = cloneHeader(req.Header)

	ww := NewResponseWriter(rw, req)
	rw = ww.ResponseWriter

	err := p.Director(ww, req, outreq)
	if err != nil {
		return
	}

	outreq.Close = false

	reqUpType := outreq.Header.Get("Upgrade")

	removeConnectionHeaders(outreq.Header)

	// Remove hop-by-hop headers to the backend. Especially
	// important is "Connection" because we want a persistent
	// connection, regardless of what the client sent to us.
	for _, h := range hopHeaders {
		outreq.Header.Del(h)
	}

	// Issue 21096: tell backend applications that care about trailer support
	// that we support trailers. (We do, but we don't go out of our way to
	// advertise that unless the incoming client request thought it was worth
	// mentioning.) Note that we look at req.Header, not outreq.Header, since
	// the latter has passed through removeConnectionHeaders.
	if httpguts.HeaderValuesContainsToken(req.Header["Te"], "trailers") {
		outreq.Header.Set("Te", "trailers")
	}

	// After stripping all the hop-by-hop connection headers above, add back any
	// necessary for protocol upgrades, such as for websockets.
	if reqUpType != "" {
		outreq.Header.Set("Connection", "Upgrade")
		outreq.Header.Set("Upgrade", reqUpType)
	}

	if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		// If we aren't the first proxy retain prior
		// X-Forwarded-For information as a comma+space
		// separated list and fold multiple headers into one.
		prior, ok := outreq.Header["X-Forwarded-For"]
		omit := ok && prior == nil // Issue 38079: nil now means don't populate the header
		if len(prior) > 0 {
			clientIP = strings.Join(prior, ", ") + ", " + clientIP
		}
		if !omit {
			outreq.Header.Set("X-Forwarded-For", clientIP)
		}
	}

	res, err := roundTrip(ww, req, outreq)
	if err != nil {
		p.getErrorHandler()(ww, req, outreq, err)
		return
	}

	// Deal with 101 Switching Protocols responses: (WebSocket, h2c, etc)
	if res.StatusCode == http.StatusSwitchingProtocols {

		if p.ModifyResponse != nil {
			if err := p.ModifyResponse(ww, req, outreq, res); err != nil {
				res.Body.Close()
				p.getErrorHandler()(ww, req, outreq, err)
				return
			}
		}

		p.handleUpgradeResponse(rw, outreq, outreq, res)
		return
	}

	removeConnectionHeaders(res.Header)

	for _, h := range hopHeaders {
		res.Header.Del(h)
	}

	if p.ModifyResponse != nil {
		if err := p.ModifyResponse(ww, req, outreq, res); err != nil {
			res.Body.Close()
			p.getErrorHandler()(ww, req, outreq, err)
			return
		}
	}

	copyHeader(ww.Header(), res.Header)

	// The "Trailer" header isn't included in the Transport's response,
	// at least for *http.Transport. Build it up from Trailer.
	announcedTrailers := len(res.Trailer)
	if announcedTrailers > 0 {
		trailerKeys := make([]string, 0, len(res.Trailer))
		for k := range res.Trailer {
			trailerKeys = append(trailerKeys, k)
		}
		ww.Header().Add("Trailer", strings.Join(trailerKeys, ", "))
	}

	ww.WriteHeader(res.StatusCode)

	err = p.copyResponse(ww, res.Body, p.flushInterval(res), res)
	if err != nil {
		xlog.Errorf("copy response fail: %v", err)
		res.Body.Close()
		return
	}
	res.Body.Close() // close now, instead of defer, to populate res.Trailer

	if len(res.Trailer) > 0 {
		// Force chunking if we saw a response trailer.
		// This prevents net/http from calculating the length for short
		// bodies and adding a Content-Length.
		if fl, ok := rw.(http.Flusher); ok {
			fl.Flush()
		}
	}

	if len(res.Trailer) == announcedTrailers {
		copyHeader(ww.Header(), res.Trailer)
		return
	}

	for k, vv := range res.Trailer {
		k = http.TrailerPrefix + k
		for _, v := range vv {
			ww.Header().Add(k, v)
		}
	}
}

// removeConnectionHeaders removes hop-by-hop headers listed in the "Connection" header of h.
// See RFC 7230, section 6.1
func removeConnectionHeaders(h http.Header) {
	if c := h.Get("Connection"); c != "" {
		for _, f := range strings.Split(c, ",") {
			if f = strings.TrimSpace(f); f != "" {
				h.Del(f)
			}
		}
	}
}

func (p *ReverseProxyHandler) copyResponse(dst io.Writer, src io.Reader, flushInterval time.Duration, res *http.Response) error {

	if flushInterval != 0 {
		if wf, ok := dst.(writeFlusher); ok {
			mlw := &maxLatencyWriter{
				dst:     wf,
				latency: flushInterval,
			}
			defer mlw.stop()

			// set up initial timer so headers get flushed even if body writes are delayed
			mlw.flushPending = true
			mlw.t = time.AfterFunc(flushInterval, mlw.delayedFlush)

			dst = mlw
		}
	}

	var buf []byte
	if p.BufferPool != nil {
		buf = p.BufferPool.Get()
		defer p.BufferPool.Put(buf)
	}

	var err error
	if p.CopyBuffer != nil {
		_, err = p.CopyBuffer(res, dst, src, buf)
	} else {
		_, err = p.copyBuffer(dst, src, buf)
	}
	return err
}

// copyBuffer returns any write errors or non-EOF read errors, and the amount
// of bytes written.
func (p *ReverseProxyHandler) copyBuffer(dst io.Writer, src io.Reader, buf []byte) (int64, error) {
	if len(buf) == 0 {
		buf = make([]byte, 32*1024)
	}
	var written int64
	for {
		nr, rerr := src.Read(buf)
		if rerr != nil && rerr != io.EOF && rerr != context.Canceled {
			p.logf("ReverseProxyHandler read error during body copy: %v", rerr)
		}
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			if nw > 0 {
				written += int64(nw)
			}
			if werr != nil {
				return written, werr
			}
			if nr != nw {
				return written, io.ErrShortWrite
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				rerr = nil
			}
			return written, rerr
		}
	}
}

func (p *ReverseProxyHandler) logf(format string, args ...interface{}) {
	xlog.Outputf(xlog.Lerror, 1, format, args...)
}
func (p *ReverseProxyHandler) flushInterval(res *http.Response) time.Duration {
	resCT := res.Header.Get("Content-Type")

	// For Server-Sent Events responses, flush immediately.
	// The MIME type is defined in https://www.w3.org/TR/eventsource/#text-event-stream
	if baseCT, _, _ := mime.ParseMediaType(resCT); baseCT == "text/event-stream" {
		return -1 // negative means immediately
	}

	// We might have the case of streaming for which Content-Length might be unset.
	if res.ContentLength == -1 {
		return -1
	}

	return p.FlushInterval
}

type writeFlusher interface {
	io.Writer
	http.Flusher
}

type maxLatencyWriter struct {
	dst     writeFlusher
	latency time.Duration // non-zero; negative means to flush immediately

	mu           sync.Mutex // protects t, flushPending, and dst.Flush
	t            *time.Timer
	flushPending bool
}

func (m *maxLatencyWriter) Write(p []byte) (n int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err = m.dst.Write(p)
	if m.latency < 0 {
		m.dst.Flush()
		return
	}
	if m.flushPending {
		return
	}
	if m.t == nil {
		m.t = time.AfterFunc(m.latency, m.delayedFlush)
	} else {
		m.t.Reset(m.latency)
	}
	m.flushPending = true
	return
}

func (m *maxLatencyWriter) delayedFlush() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.flushPending { // if stop was called but AfterFunc already started this goroutine
		return
	}
	m.dst.Flush()
	m.flushPending = false
}

func (m *maxLatencyWriter) stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flushPending = false
	if m.t != nil {
		m.t.Stop()
	}
}

func (p *ReverseProxyHandler) handleUpgradeResponse(rw http.ResponseWriter, req *http.Request, target *http.Request, res *http.Response) {
	reqUpType := req.Header.Get("Upgrade")
	resUpType := res.Header.Get("Upgrade")

	if reqUpType != resUpType {
		p.getErrorHandler()(rw, req, target, fmt.Errorf("backend tried to switch protocol %q when %q was requested", resUpType, reqUpType))
		return
	}

	hj, ok := rw.(http.Hijacker)
	if !ok {
		p.getErrorHandler()(rw, req, target, fmt.Errorf("can't switch protocols using non-Hijacker ResponseWriter type %T", rw))
		return
	}

	backConn, ok := res.Body.(io.ReadWriteCloser)
	if !ok {
		p.getErrorHandler()(rw, req, target, fmt.Errorf("internal error: 101 switching protocols response with non-writable body, headers %v", res.Header))
		return
	}

	backConnCloseCh := make(chan bool)
	go func() {
		// Ensure that the cancellation of a request closes the backend.
		// See issue https://golang.org/issue/35559.
		select {
		case <-req.Context().Done():
		case <-backConnCloseCh:
		}
		backConn.Close()
	}()

	defer close(backConnCloseCh)

	conn, brw, err := hj.Hijack()
	if err != nil {
		p.getErrorHandler()(rw, req, target, fmt.Errorf("Hijack failed on protocol switch: %v", err))
		return
	}
	defer conn.Close()

	copyHeader(rw.Header(), res.Header)

	res.Header = rw.Header()
	res.Body = nil // so res.Write only writes the headers; we have res.Body in backConn above
	if err := res.Write(brw); err != nil {
		p.getErrorHandler()(rw, req, target, fmt.Errorf("response write: %v", err))
		return
	}
	if err := brw.Flush(); err != nil {
		p.getErrorHandler()(rw, req, target, fmt.Errorf("response flush: %v", err))
		return
	}
	errc := make(chan error, 2)
	spc := switchProtocolCopier{user: p.newConnWithTimeout(conn), backend: p.newConnWithTimeout(backConn)}
	go spc.copyToBackend(errc)
	go spc.copyFromBackend(errc)
	<-errc
	return
}

// switchProtocolCopier exists so goroutines proxying data back and
// forth have nice names in stacks.
type switchProtocolCopier struct {
	user, backend io.ReadWriter
}

func (c switchProtocolCopier) copyFromBackend(errc chan<- error) {
	_, err := io.Copy(c.user, c.backend)
	errc <- err
}

func (c switchProtocolCopier) copyToBackend(errc chan<- error) {
	_, err := io.Copy(c.backend, c.user)
	errc <- err
}

func (p *ReverseProxyHandler) newConnWithTimeout(rwc io.ReadWriteCloser) io.ReadWriter {
	if p.UpgradeReadTimeout <= 0 && p.UpgradeWriteTimeout <= 0 {
		return rwc
	}

	if c, ok := rwc.(net.Conn); ok {
		return &connWithTimeout{
			Conn:         c,
			ReadTimeout:  p.UpgradeReadTimeout,
			WriteTimeout: p.UpgradeWriteTimeout,
		}
	}

	return rwc
}

type connWithTimeout struct {
	net.Conn
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

func (c *connWithTimeout) Read(p []byte) (n int, err error) {

	if c.ReadTimeout > 0 {
		c.SetReadDeadline(time.Now().Add(c.ReadTimeout))
	}

	return c.Conn.Read(p)
}

func (c *connWithTimeout) Write(b []byte) (n int, err error) {
	if c.WriteTimeout > 0 {
		c.SetWriteDeadline(time.Now().Add(c.WriteTimeout))
	}

	return c.Conn.Write(b)
}
