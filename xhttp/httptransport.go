package xhttp

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ccxp/xgo/xhttp/internal"
	"github.com/ccxp/xgo/xnet"
	"github.com/pierrec/lz4"
	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/idna"
)

// ---------------------------------------------Transport

type httpRequest struct {
	req      *http.Request
	userData interface{}

	written   bool
	t         *Transport
	addedGzip bool

	extra http.Header // extra headers to write, or nil

	Method           string
	Body             io.Reader
	BodyCloser       io.Closer
	ResponseToHEAD   bool
	ContentLength    int64 // -1 means unknown, 0 means exactly none
	close            bool
	TransferEncoding []string
	Header           http.Header
	Trailer          http.Header

	FlushHeaders bool            // flush headers to network before body
	ByteReadCh   chan readResult // non-nil if probeRequestBody called

	continueCh chan struct{}
}

func (r *httpRequest) Context() context.Context {
	return r.req.Context()
}

// call Close() when Context() is done.
func (r *httpRequest) Close() {

	if r.req.Body != nil {
		r.req.Body.Close()
	}
}

// if IsClose return true, the connection will be close after read response.
func (r *httpRequest) IsClose() bool {
	return r.req.Close
}

// IsReplayable is called after read failed.
func (r *httpRequest) IsReplayable(err error) bool {

	if !r.written {
		return true
	}

	if r.t.RetryWritten {
		if r.req.Body == nil || r.req.Body == http.NoBody || r.req.GetBody != nil {
			switch r.req.Method {
			case "", "GET", "HEAD", "OPTIONS", "TRACE":
				return true
			}
			// The Idempotency-Key, while non-standard, is widely used to
			// mean a POST or other request is idempotent. See
			// https://golang.org/issue/19943#issuecomment-421092421
			if _, ok := r.req.Header["Idempotency-Key"]; ok {
				return true
			}
			if _, ok := r.req.Header["X-Idempotency-Key"]; ok {
				return true
			}
		}
	}

	return false
}

// reset to replay if is replayable.
func (r *httpRequest) Reset() error {

	if r.req.GetBody != nil {
		newReq := *r.req
		var err error
		newReq.Body, err = r.req.GetBody()
		if err != nil {
			return err
		}
		r.req = &newReq

		r.Body = newReq.Body
		r.BodyCloser = newReq.Body
		r.ContentLength = r.outgoingLength()
	}
	return nil
}

func (r *httpRequest) String() string {
	return r.req.URL.Path
}

type httpResponse struct {
	req      *http.Request
	userData interface{}
	resp     *http.Response
	t        *Transport
}

// if IsClose return true, the connection will be close after read response.
func (r *httpResponse) IsClose() bool {
	return r.resp.Close
}

// IsReadDelay should return true, if the response's body is delay to read after Transport.Roundtrip.
// In Transport.Roundtrip, if Response.IsReadDelay return true,
// it will call GetReader to get the body reader, the call WrapReader,
// Response should replace body reader with newR pass by WrapReader.
func (r *httpResponse) IsReadDelay() bool {
	var bodyIsWritable bool
	_, bodyIsWritable = r.resp.Body.(io.Writer)

	return bodyIsWritable || (r.req.Method != "HEAD" && r.resp.ContentLength != 0)
}

func (r *httpResponse) GetReader() (io.ReadCloser, time.Duration) {

	d := r.t.ResponseBodyTimeout
	if isProtocolSwitch(r.resp) {
		d = 0
	} else if r.t.GetResponseBodyTimeout != nil {
		d = r.t.GetResponseBodyTimeout(r.req, r.userData)
	}
	return r.resp.Body, d
}

func (r *httpResponse) WrapReader(newR io.ReadCloser) {
	r.resp.Body = newR
}

// Transport is base on http.net.Transport,
// and supprt Balancer, Tracer, dial timeout and write timeout.
//
// It use xnet.Transport as Transport and connection pool.
type Transport struct {
	Balancer *xnet.Balancer
	Tracer   *xnet.Tracer

	// if SetRequest is not nil, call SetRequest after got addr info by Balancer.GetEndpoint.
	// It can be used to set new Host to request.Url.
	SetRequest func(req *http.Request, userData interface{}, connInfo *xnet.ConnInfo) *http.Request

	// connnect
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)

	// TLS connections for requests.
	//
	// If DialTLS is nil, tls.Dial is used.
	//
	// If the returned net.Conn has a ConnectionState method like tls.Conn,
	// it will be used to set http.Response.TLS.
	DialTLS func(ctx context.Context, network, address string, cfg *tls.Config) (net.Conn, error)

	DialTimeout           time.Duration
	WriteRequestTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	ResponseBodyTimeout   time.Duration

	// 如果存在，则在读body时用这个函数取超时时间，否则直接用ResponseBodyTimeout.
	GetResponseBodyTimeout func(req *http.Request, userData interface{}) time.Duration

	// ExpectContinueTimeout, if non-zero, specifies the amount of
	// time to wait for a server's first response headers after fully
	// writing the request headers if the request has an
	// "Expect: 100-continue" header. Zero means no timeout and
	// causes the body to be sent immediately, without
	// waiting for the server to approve.
	// This time does not include the time to send the request header.
	ExpectContinueTimeout time.Duration

	// MaxTryCount, if non-zero, controls the maximum tries per request.
	// If zero, DefaultMaxTryCount is used.
	MaxTryCount int

	// if request.Write has called and transport fail, transport will only retry where RetryWritten is true,
	// and request has no body or GetBody is not nil.
	RetryWritten bool

	// TLSClientConfig specifies the TLS configuration to use with
	// tls.Client.
	TLSClientConfig *tls.Config

	// TLSHandshakeTimeout specifies the maximum amount of time waiting to
	// wait for a TLS handshake. Zero means no timeout.
	TLSHandshakeTimeout time.Duration

	DisableKeepAlives bool

	// 默认Accept-Encoding为空时，自动增加gzip头，但返回body时会自动解码，DisableCompression=true时关闭此特性
	DisableCompression bool

	// MaxIdleConns controls the maximum number of idle (keep-alive)
	// connections across all hosts. Zero means no limit.
	MaxIdleConns int

	// MaxIdleConnsPerHost, if non-zero, controls the maximum idle
	// (keep-alive) connections to keep per-host. If zero,
	// DefaultMaxIdleConnsPerHost is used.
	MaxIdleConnsPerHost int

	// MaxConnsPerHost optionally limits the total number of
	// connections per host, including connections in the dialing,
	// active, and idle states. On limit violation, dials will block.
	//
	// Zero means no limit.
	MaxConnsPerHost int

	// IdleConnTimeout is the maximum amount of time an idle
	// (keep-alive) connection will remain idle before closing
	// itself.
	// Zero means no limit.
	IdleConnTimeout time.Duration

	Proxy func(*http.Request, *xnet.ConnInfo) (*url.URL, error)
	// ProxyConnectHeader optionally specifies headers to send to
	// proxies during CONNECT requests.
	ProxyConnectHeader http.Header

	// WriteBufferSize specifies the size of the write buffer used
	// when writing to the transport.
	// If zero, a default (currently 4KB) is used.
	WriteBufferSize int

	// ReadBufferSize specifies the size of the read buffer used
	// when reading from the transport.
	// If zero, a default (currently 4KB) is used.
	ReadBufferSize int

	xTransport         xnet.Transport
	xTransportInitOnce sync.Once
}

func (t *Transport) setRequest(xReq xnet.Request, connInfo *xnet.ConnInfo) xnet.Request {
	if t.SetRequest != nil {
		req := xReq.(*httpRequest)
		req.req = t.SetRequest(req.req, req.userData, connInfo)
		return req
	}
	return xReq
}

func (t *Transport) gotConn(xReq xnet.Request, connInfo *xnet.ConnInfo) {
	xReq.(*httpRequest).written = true
}

// Write request, for xnet.Transport
func (t *Transport) writeRequest(w *bufio.Writer, xReq xnet.Request, rc *xnet.RequestConn) error {
	req := xReq.(*httpRequest)
	req.written = true
	return req.write(w, rc, t.waitForContinue(req.continueCh, rc.ConnClosed))
}

// Read response, for xnet.Transport
func (t *Transport) readResponse(r *bufio.Reader, xReq xnet.Request, rc *xnet.RequestConn) (xResp xnet.Response, err error) {
	req := xReq.(*httpRequest)

	var resp *http.Response

	num1xx := 0               // number of informational 1xx headers received
	const max1xxResponses = 5 // arbitrary bound on number of informational responses

	continueCh := req.continueCh
	for {
		resp, err = http.ReadResponse(r, req.req)
		if err != nil {
			return
		}

		resCode := resp.StatusCode
		if continueCh != nil {
			if resCode == 100 {
				continueCh <- struct{}{}
				continueCh = nil
			} else if resCode >= 200 {
				close(continueCh)
				continueCh = nil
			}
		}
		is1xx := 100 <= resCode && resCode <= 199
		// treat 101 as a terminal status, see issue 26161
		is1xxNonTerminal := is1xx && resCode != http.StatusSwitchingProtocols
		if is1xxNonTerminal {
			num1xx++
			if num1xx > max1xxResponses {
				return nil, errors.New("xhttp: too many 1xx informational responses")
			}
			continue
		}
		break
	}

	if isProtocolSwitch(resp) {
		resp.Body = newReadWriteCloserBody(r, rc.Conn)
	}

	resp.TLS = rc.TlsState

	if req.addedGzip && strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		resp.Body = &gzipReader{body: resp.Body}
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
		resp.Uncompressed = true
	}

	return &httpResponse{req.req, req.userData, resp, t}, nil
}

func (t *Transport) CloseIdleConnections() {
	if t.xTransport.Balancer != nil {
		t.xTransport.CloseIdleConnections()
	}
}

// RoundTrip execute a HTTP transaction.
//
// err return by RoundTrip is:
//
//	ErrRequestCanceled, ErrRequestCanceledConn
//	*TransportGetEnpointError
//	*TransportConnError
//	*TransportWriteError
//	*TransportReadError
//	*TransportError
//	other
func (t *Transport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	return t.RoundTripWithUserData(req, nil)
}

// RoundTrip with user data.
//
// data can be obtain by GetTransportRequestUserData in balancer and tracer.
func (t *Transport) RoundTripWithUserData(req *http.Request, data interface{}) (resp *http.Response, err error) {

	t.xTransportInitOnce.Do(t.initTransport)

	xReq := &httpRequest{
		req:      req,
		userData: data,
		t:        t,
	}

	if req.ContentLength != 0 && req.Body == nil {
		return nil, fmt.Errorf("http: Request.ContentLength=%d with nil Body", req.ContentLength)
	}
	xReq.Method = valueOrDefault(req.Method, "GET")
	xReq.close = req.Close
	xReq.TransferEncoding = req.TransferEncoding
	xReq.Header = req.Header
	xReq.Trailer = req.Trailer
	xReq.Body = req.Body
	xReq.BodyCloser = req.Body
	xReq.ContentLength = xReq.outgoingLength()
	if xReq.ContentLength < 0 && len(xReq.TransferEncoding) == 0 && xReq.shouldSendChunkedRequestBody() {
		xReq.TransferEncoding = []string{"chunked"}
	}
	// If there's a body, conservatively flush the headers
	// to any bufio.Writer we're writing to, just in case
	// the server needs the headers early, before we copy
	// the body and possibly block. We make an exception
	// for the common standard library in-memory types,
	// though, to avoid unnecessary TCP packets on the
	// wire. (Issue 22088.)
	if xReq.ContentLength != 0 && !isKnownInMemoryReader(xReq.Body) {
		xReq.FlushHeaders = true
	}

	if xReq.Body == nil {
		xReq.TransferEncoding = nil
	}
	if chunked(xReq.TransferEncoding) {
		xReq.ContentLength = -1
	} else {
		if xReq.Body == nil { // no chunking, no body
			xReq.ContentLength = 0
		}
		// Sanitize Trailer
		xReq.Trailer = nil
	}

	if !t.DisableCompression &&
		req.Header.Get("Accept-Encoding") == "" &&
		req.Header.Get("Range") == "" &&
		req.Method != "HEAD" &&
		req.Header.Get("Upgrade") != "websocket" {
		// Request gzip only, not deflate. Deflate is ambiguous and
		// not as universally supported anyway.
		// See: http://www.gzip.org/zlib/zlib_faq.html#faq38
		//
		// Note that we don't request this for HEAD requests,
		// due to a bug in nginx:
		//   https://trac.nginx.org/nginx/ticket/358
		//   https://golang.org/issue/5522
		//
		// We don't request gzip if the request is for a range, since
		// auto-decoding a portion of a gzipped document will just fail
		// anyway. See https://golang.org/issue/8923
		xReq.addedGzip = true
		xReq.extraHeaders().Set("Accept-Encoding", "gzip")
	}

	if t.DisableKeepAlives {
		xReq.extraHeaders().Set("Connection", "close")
	}

	if req.ProtoAtLeast(1, 1) && req.Body != nil && xReq.expectsContinue() {
		xReq.continueCh = make(chan struct{}, 1)
	}

	xResp, err := t.xTransport.RoundTrip(xReq)
	if err != nil {
		// same has net/http.Transport
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, err
	}
	return xResp.(*httpResponse).resp, nil
}

// waitForContinue returns the function to block until
// any response, timeout or connection close. After any of them,
// the function returns a bool which indicates if the body should be sent.
func (t *Transport) waitForContinue(continueCh, notifyConnClosed <-chan struct{}) func() bool {
	if continueCh == nil {
		return nil
	}
	return func() bool {
		timer := time.NewTimer(t.ExpectContinueTimeout)
		defer timer.Stop()

		select {
		case _, ok := <-continueCh:
			return ok
		case <-timer.C:
			return true
		case <-notifyConnClosed:
			return false
		}
	}
}

func GetTransportRequest(req xnet.Request) *http.Request {
	if req != nil {
		if r, ok := req.(*httpRequest); ok {
			return r.req
		}
	}
	return nil
}

func GetTransportRequestUserData(req xnet.Request) interface{} {
	if req != nil {
		if r, ok := req.(*httpRequest); ok {
			return r.userData
		}
	}
	return nil
}

func GetTransportResponse(resp xnet.Response) *http.Response {
	if resp != nil {
		if r, ok := resp.(*httpResponse); ok {
			return r.resp
		}
	}
	return nil
}

// ------------------- write request

func isProtocolSwitch(r *http.Response) bool {
	return r.StatusCode == http.StatusSwitchingProtocols &&
		r.Header.Get("Upgrade") != "" &&
		httpguts.HeaderValuesContainsToken(r.Header["Connection"], "Upgrade")
}

func (r *httpRequest) extraHeaders() http.Header {
	if r.extra == nil {
		r.extra = make(http.Header)
	}
	return r.extra
}

func (r *httpRequest) write(w *bufio.Writer, rc *xnet.RequestConn, waitForContinue func() bool) (err error) {

	// Find the target host. Prefer the Host: header, but if that
	// is not given, use the host from the request URL.
	//
	// Clean the host, in case itarrives with unexpected stuff in it.
	host := cleanHost(r.req.Host)
	if host == "" {
		if r.req.URL == nil {
			return errMissingHost
		}
		host = cleanHost(r.req.URL.Host)
	}

	// According to RFC 6874, an HTTP client, proxy, or other
	// intermediary must remove any IPv6 zone identifier attached
	// to an outgoing URI.
	host = removeZone(host)

	if rc.ProxyUrl != nil && rc.ConnInfo.Scheme == "http" {
		if u := rc.ProxyUrl.User; u != nil {
			username := u.Username()
			password, _ := u.Password()
			auth := username + ":" + password
			r.extraHeaders().Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(auth)))
		}
	}
	ruri := r.req.URL.RequestURI()
	if rc.ProxyUrl != nil && r.req.URL.Scheme == "http" {
		ruri = r.req.URL.Scheme + "://" + host + ruri
	} else if r.Method == "CONNECT" && r.req.URL.Path == "" {
		// CONNECT requests normally give just the host and port, not a full URL.
		ruri = host
	}
	// TODO(bradfitz): escape at least newlines in ruri?

	_, err = fmt.Fprintf(w, "%s %s HTTP/1.1\r\n", r.Method, ruri)
	if err != nil {
		return err
	}

	// Header lines
	_, err = fmt.Fprintf(w, "Host: %s\r\n", host)
	if err != nil {
		return err
	}

	// Use the defaultUserAgent unless the Header contains one, which
	// may be blank to not send the header.
	userAgent := defaultUserAgent
	if _, ok := r.Header["User-Agent"]; ok {
		userAgent = r.Header.Get("User-Agent")
	}
	if userAgent != "" {
		_, err = fmt.Fprintf(w, "User-Agent: %s\r\n", userAgent)
		if err != nil {
			return err
		}
	}

	// Process Body,ContentLength,Close,Trailer
	err = r.writeHeader(w)
	if err != nil {
		return err
	}

	err = r.Header.WriteSubset(w, reqWriteExcludeHeader)
	if err != nil {
		return err
	}

	if r.extra != nil {
		err = r.extra.Write(w)
		if err != nil {
			return err
		}
	}

	_, err = io.WriteString(w, "\r\n")
	if err != nil {
		return err
	}

	// Flush and wait for 100-continue if expected.
	if waitForContinue != nil {
		err = w.Flush()
		if err != nil {
			return err
		}

		if !waitForContinue() {
			if r.req.Body != nil {
				r.req.Body.Close()
			}
			return nil
		}
	}

	if r.FlushHeaders {
		if err := w.Flush(); err != nil {
			return err
		}
	}

	// Write body and trailer
	err = r.writeBody(w)
	if err != nil {
		return err
	}

	return w.Flush()
}

func (r *httpRequest) expectsContinue() bool {
	return hasToken(getheader(r.Header, "Expect"), "100-continue")
}

func (r *httpRequest) writeHeader(w io.Writer) error {
	if r.close && !hasToken(getheader(r.Header, "Connection"), "close") {
		if _, err := io.WriteString(w, "Connection: close\r\n"); err != nil {
			return err
		}
	}

	// Write Content-Length and/or Transfer-Encoding whose values are a
	// function of the sanitized field triple (Body, ContentLength,
	// TransferEncoding)
	if r.shouldSendContentLength() {
		if _, err := io.WriteString(w, "Content-Length: "); err != nil {
			return err
		}
		if _, err := io.WriteString(w, strconv.FormatInt(r.ContentLength, 10)+"\r\n"); err != nil {
			return err
		}
	} else if chunked(r.TransferEncoding) {
		if _, err := io.WriteString(w, "Transfer-Encoding: chunked\r\n"); err != nil {
			return err
		}
	}

	// Write Trailer header
	if r.Trailer != nil {
		keys := make([]string, 0, len(r.Trailer))
		for k := range r.Trailer {
			k = http.CanonicalHeaderKey(k)
			switch k {
			case "Transfer-Encoding", "Trailer", "Content-Length":
				return &badStringError{"invalid Trailer key", k}
			}
			keys = append(keys, k)
		}
		if len(keys) > 0 {
			sort.Strings(keys)
			// TODO: could do better allocation-wise here, but trailers are rare,
			// so being lazy for now.
			if _, err := io.WriteString(w, "Trailer: "+strings.Join(keys, ",")+"\r\n"); err != nil {
				return err
			}
		}
	}

	return nil
}

func (r *httpRequest) writeBody(w io.Writer) error {
	var err error
	var ncopy int64

	// Write body
	if r.Body != nil {
		if chunked(r.TransferEncoding) {
			w = &internal.FlushAfterChunkWriter{Writer: w.(*bufio.Writer)}
			cw := internal.NewChunkedWriter(w)
			_, err = io.Copy(cw, r.Body)
			if err == nil {
				err = cw.Close()
			}
		} else if r.ContentLength == -1 {
			ncopy, err = io.Copy(w, r.Body)
		} else {
			ncopy, err = io.Copy(w, io.LimitReader(r.Body, r.ContentLength))
			if err != nil {
				return err
			}
			var nextra int64
			nextra, err = io.Copy(ioutil.Discard, r.Body)
			ncopy += nextra
		}
		if err != nil {
			return err
		}
	}
	if r.BodyCloser != nil {
		if err := r.BodyCloser.Close(); err != nil {
			return err
		}
	}

	if r.ContentLength != -1 && r.ContentLength != ncopy {
		return fmt.Errorf("http: ContentLength=%d with Body length %d",
			r.ContentLength, ncopy)
	}

	if chunked(r.TransferEncoding) {
		// Write Trailer header
		if r.Trailer != nil {
			if err := r.Trailer.Write(w); err != nil {
				return err
			}
		}
		// Last chunk, empty trailer
		_, err = io.WriteString(w, "\r\n")
	}
	return err
}

func (r *httpRequest) shouldSendContentLength() bool {
	if chunked(r.TransferEncoding) {
		return false
	}
	if r.ContentLength > 0 {
		return true
	}
	if r.ContentLength < 0 {
		return false
	}
	// Many servers expect a Content-Length for these methods
	if r.Method == "POST" || r.Method == "PUT" {
		return true
	}
	if r.ContentLength == 0 && isIdentity(r.TransferEncoding) {
		if r.Method == "GET" || r.Method == "HEAD" {
			return false
		}
		return true
	}

	return false
}

func (r *httpRequest) outgoingLength() int64 {
	if r.req.Body == nil || r.req.Body == http.NoBody {
		return 0
	}
	if r.req.ContentLength != 0 {
		return r.req.ContentLength
	}
	return -1
}

func getheader(h http.Header, key string) string {
	if v := h[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// shouldSendChunkedRequestBody reports whether we should try to send a
// chunked request body to the server. In particular, the case we really
// want to prevent is sending a GET or other typically-bodyless request to a
// server with a chunked body when the body has zero bytes, since GETs with
// bodies (while acceptable according to specs), even zero-byte chunked
// bodies, are approximately never seen in the wild and confuse most
// servers. See Issue 18257, as one example.
//
// The only reason we'd send such a request is if the user set the Body to a
// non-nil value (say, ioutil.NopCloser(bytes.NewReader(nil))) and didn't
// set ContentLength, or NewRequest set it to -1 (unknown), so then we assume
// there's bytes to send.
//
// This code tries to read a byte from the Request.Body in such cases to see
// whether the body actually has content (super rare) or is actually just
// a non-nil content-less ReadCloser (the more common case). In that more
// common case, we act as if their Body were nil instead, and don't send
// a body.
func (r *httpRequest) shouldSendChunkedRequestBody() bool {
	// Note that t.ContentLength is the corrected content length
	// from rr.outgoingLength, so 0 actually means zero, not unknown.
	if r.ContentLength >= 0 || r.Body == nil { // redundant checks; caller did them
		return false
	}
	if requestMethodUsuallyLacksBody(r.Method) {
		// Only probe the Request.Body for GET/HEAD/DELETE/etc
		// requests, because it's only those types of requests
		// that confuse servers.
		r.probeRequestBody() // adjusts t.Body, t.ContentLength
		return r.Body != nil
	}
	// For all other request types (PUT, POST, PATCH, or anything
	// made-up we've never heard of), assume it's normal and the server
	// can deal with a chunked request body. Maybe we'll adjust this
	// later.
	return true
}

// probeRequestBody reads a byte from t.Body to see whether it's empty
// (returns io.EOF right away).
//
// But because we've had problems with this blocking users in the past
// (issue 17480) when the body is a pipe (perhaps waiting on the response
// headers before the pipe is fed data), we need to be careful and bound how
// long we wait for it. This delay will only affect users if all the following
// are true:
//   - the request body blocks
//   - the content length is not set (or set to -1)
//   - the method doesn't usually have a body (GET, HEAD, DELETE, ...)
//   - there is no transfer-encoding=chunked already set.
//
// In other words, this delay will not normally affect anybody, and there
// are workarounds if it does.
func (r *httpRequest) probeRequestBody() {
	r.ByteReadCh = make(chan readResult, 1)
	go func(body io.Reader) {
		var buf [1]byte
		var rres readResult
		rres.n, rres.err = body.Read(buf[:])
		if rres.n == 1 {
			rres.b = buf[0]
		}
		r.ByteReadCh <- rres
	}(r.Body)
	timer := time.NewTimer(200 * time.Millisecond)
	select {
	case rres := <-r.ByteReadCh:
		timer.Stop()
		if rres.n == 0 && rres.err == io.EOF {
			// It was empty.
			r.Body = nil
			r.ContentLength = 0
		} else if rres.n == 1 {
			if rres.err != nil {
				r.Body = io.MultiReader(&byteReader{b: rres.b}, errorReader{rres.err})
			} else {
				r.Body = io.MultiReader(&byteReader{b: rres.b}, r.Body)
			}
		} else if rres.err != nil {
			r.Body = errorReader{rres.err}
		}
	case <-timer.C:
		// Too slow. Don't wait. Read it later, and keep
		// assuming that this is ContentLength == -1
		// (unknown), which means we'll send a
		// "Transfer-Encoding: chunked" header.
		r.Body = io.MultiReader(finishAsyncByteRead{r}, r.Body)
		// Request that Request.Write flush the headers to the
		// network before writing the body, since our body may not
		// become readable until it's seen the response headers.
		r.FlushHeaders = true
	}
}

// cleanHost cleans up the host sent in request's Host header.
//
// It both strips anything after '/' or ' ', and puts the value
// into Punycode form, if necessary.
//
// Ideally we'd clean the Host header according to the spec:
//
//	https://tools.ietf.org/html/rfc7230#section-5.4 (Host = uri-host [ ":" port ]")
//	https://tools.ietf.org/html/rfc7230#section-2.7 (uri-host -> rfc3986's host)
//	https://tools.ietf.org/html/rfc3986#section-3.2.2 (definition of host)
//
// But practically, what we are trying to avoid is the situation in
// issue 11206, where a malformed Host header used in the proxy context
// would create a bad request. So it is enough to just truncate at the
// first offending character.
func cleanHost(in string) string {
	if i := strings.IndexAny(in, " /"); i != -1 {
		in = in[:i]
	}
	host, port, err := net.SplitHostPort(in)
	if err != nil { // input was just a host
		a, err := idnaASCII(in)
		if err != nil {
			return in // garbage in, garbage out
		}
		return a
	}
	a, err := idnaASCII(host)
	if err != nil {
		return in // garbage in, garbage out
	}
	return net.JoinHostPort(a, port)
}

func idnaASCII(v string) (string, error) {
	// TODO: Consider removing this check after verifying performance is okay.
	// Right now punycode verification, length checks, context checks, and the
	// permissible character tests are all omitted. It also prevents the ToASCII
	// call from salvaging an invalid IDN, when possible. As a result it may be
	// possible to have two IDNs that appear identical to the user where the
	// ASCII-only version causes an error downstream whereas the non-ASCII
	// version does not.
	// Note that for correct ASCII IDNs ToASCII will only do considerably more
	// work, but it will not cause an allocation.
	if isASCII(v) {
		return v, nil
	}
	return idna.Lookup.ToASCII(v)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// removeZone removes IPv6 zone identifier from host.
// E.g., "[fe80::1%en0]:8080" to "[fe80::1]:8080"
func removeZone(host string) string {
	if !strings.HasPrefix(host, "[") {
		return host
	}
	i := strings.LastIndex(host, "]")
	if i < 0 {
		return host
	}
	j := strings.LastIndex(host[:i], "%")
	if j < 0 {
		return host
	}
	return host[:j] + host[i:]
}

// Return value if nonempty, def otherwise.
func valueOrDefault(value, def string) string {
	if value != "" {
		return value
	}
	return def
}

// hasToken reports whether token appears with v, ASCII
// case-insensitive, with space or comma boundaries.
// token must be all lowercase.
// v may contain mixed cased.
func hasToken(v, token string) bool {
	if len(token) > len(v) || token == "" {
		return false
	}
	if v == token {
		return true
	}
	for sp := 0; sp <= len(v)-len(token); sp++ {
		// Check that first character is good.
		// The token is ASCII, so checking only a single byte
		// is sufficient. We skip this potential starting
		// position if both the first byte and its potential
		// ASCII uppercase equivalent (b|0x20) don't match.
		// False positives ('^' => '~') are caught by EqualFold.
		if b := v[sp]; b != token[0] && b|0x20 != token[0] {
			continue
		}
		// Check that start pos is on a valid token boundary.
		if sp > 0 && !isTokenBoundary(v[sp-1]) {
			continue
		}
		// Check that end pos is on a valid token boundary.
		if endPos := sp + len(token); endPos != len(v) && !isTokenBoundary(v[endPos]) {
			continue
		}
		if strings.EqualFold(v[sp:sp+len(token)], token) {
			return true
		}
	}
	return false
}

func isTokenBoundary(b byte) bool {
	return b == ' ' || b == ',' || b == '\t'
}

func chunked(te []string) bool { return len(te) > 0 && te[0] == "chunked" }

// Checks whether the encoding is explicitly "identity".
func isIdentity(te []string) bool { return len(te) == 1 && te[0] == "identity" }

// requestMethodUsuallyLacksBody reports whether the given request
// method is one that typically does not involve a request body.
// This is used by the Transport (via
// transferWriter.shouldSendChunkedRequestBody) to determine whether
// we try to test-read a byte from a non-nil Request.Body when
// Request.outgoingLength() returns -1. See the comments in
// shouldSendChunkedRequestBody.
func requestMethodUsuallyLacksBody(method string) bool {
	switch method {
	case "GET", "HEAD", "DELETE", "OPTIONS", "PROPFIND", "SEARCH":
		return true
	}
	return false
}

var nopCloserType = reflect.TypeOf(ioutil.NopCloser(nil))

// isKnownInMemoryReader reports whether r is a type known to not
// block on Read. Its caller uses this as an optional optimization to
// send fewer TCP packets.
func isKnownInMemoryReader(r io.Reader) bool {
	switch r.(type) {
	case *bytes.Reader, *bytes.Buffer, *strings.Reader:
		return true
	}
	if reflect.TypeOf(r) == nopCloserType {
		return isKnownInMemoryReader(reflect.ValueOf(r).Field(0).Interface().(io.Reader))
	}
	return false
}

// --------------------------------- gzipReader

type morkReaderCloser struct {
	r      io.ReadCloser
	e      error
	eof    bool
	closed bool
}

func (r *morkReaderCloser) Read(p []byte) (n int, err error) {

	if r.e != nil {
		return 0, r.e
	}

	n, err = r.r.Read(p)
	if err != nil {
		r.e = err
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			r.eof = true
		}
	}

	return n, err
}

func (r *morkReaderCloser) Close() error {
	if r.closed {
		return r.e
	}

	r.e = r.r.Close()
	r.closed = true
	return r.e
}

// gzipReader wraps a response body so it can lazily
// call gzip.NewReader on the first call to Read
type gzipReader struct {
	body   io.ReadCloser // underlying HTTP/1 response body framing
	mr     *morkReaderCloser
	zr     *gzip.Reader // lazily-initialized gzip reader
	zerr   error        // any error from gzip.NewReader; sticky
	closed bool
}

func (gz *gzipReader) Read(p []byte) (n int, err error) {
	if gz.zr == nil {
		if gz.zerr == nil {
			gz.mr = &morkReaderCloser{r: gz.body}
			gz.zr, gz.zerr = gzip.NewReader(gz.mr)
		}
		if gz.zerr != nil {
			return 0, gz.zerr
		}
	}

	if gz.closed {
		err = errReadOnClosedResBody
	}
	if err != nil {
		return 0, err
	}
	n, err = gz.zr.Read(p)
	if err != nil {
		if !gz.mr.eof {
			// 读到结尾
			io.Copy(ioutil.Discard, gz.mr)
		}
	}
	return n, err
}

func (gz *gzipReader) Close() error {
	if gz.mr != nil {
		return gz.mr.Close()
	}
	return gz.body.Close()
}

type lz4Reader struct {
	body   io.ReadCloser // underlying HTTP/1 response body framing
	mr     *morkReaderCloser
	zr     *lz4.Reader // lazily-initialized gzip reader
	zerr   error       // any error from gzip.NewReader; sticky
	closed bool
}

func (gz *lz4Reader) Read(p []byte) (n int, err error) {
	if gz.zr == nil {
		if gz.zerr == nil {
			gz.mr = &morkReaderCloser{r: gz.body}
			gz.zr = lz4.NewReader(gz.mr)
		}
	}

	if gz.closed {
		err = errReadOnClosedResBody
	}
	if err != nil {
		return 0, err
	}
	n, err = gz.zr.Read(p)
	if err != nil {
		if !gz.mr.eof {
			// 读到结尾
			io.Copy(ioutil.Discard, gz.mr)
		}
	}
	return n, err
}

func (gz *lz4Reader) Close() error {
	if gz.mr != nil {
		return gz.mr.Close()
	}
	return gz.body.Close()
}

// ---------------------------

type badStringError struct {
	what string
	str  string
}

func (e *badStringError) Error() string { return fmt.Sprintf("%s %q", e.what, e.str) }

// ---------------------------

type readResult struct {
	n   int
	err error
	b   byte // byte read, if n == 1
}

// ---------------------------

type errorReader struct {
	err error
}

func (r errorReader) Read(p []byte) (n int, err error) {
	return 0, r.err
}

// ---------------------------

type byteReader struct {
	b    byte
	done bool
}

func (br *byteReader) Read(p []byte) (n int, err error) {
	if br.done {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	br.done = true
	p[0] = br.b
	return 1, io.EOF
}

// ---------------------------

// finishAsyncByteRead finishes reading the 1-byte sniff
// from the ContentLength==0, Body!=nil case.
type finishAsyncByteRead struct {
	r *httpRequest
}

func (fr finishAsyncByteRead) Read(p []byte) (n int, err error) {
	if len(p) == 0 {
		return
	}
	rres := <-fr.r.ByteReadCh
	n, err = rres.n, rres.err
	if n == 1 {
		p[0] = rres.b
	}
	return
}

// ---------------------------

const defaultUserAgent = "Go-http-client/1.1"

var errReadOnClosedResBody = errors.New("read on closed response body")

// errMissingHost is returned by Write when there is no Host or URL present in
// the Request.
var errMissingHost = errors.New("http: Request.Write on Request with no Host or URL set")

// Headers that Request.Write handles itself and should be skipped.
var reqWriteExcludeHeader = map[string]bool{
	"Host":              true, // not in Header map anyway
	"User-Agent":        true,
	"Content-Length":    true,
	"Transfer-Encoding": true,
	"Trailer":           true,
}

// -----------------

func newReadWriteCloserBody(br *bufio.Reader, conn net.Conn) net.Conn {
	body := &readWriteCloserBody{Conn: conn}
	if br.Buffered() != 0 {
		body.br = br
	}
	return body
}

// readWriteCloserBody is the Response.Body type used when we want to
// give users write access to the Body through the underlying
// connection (TCP, unless using custom dialers). This is then
// the concrete type for a Response.Body on the 101 Switching
// Protocols response, as used by WebSockets, h2c, etc.
type readWriteCloserBody struct {
	br *bufio.Reader
	net.Conn
}

func (b *readWriteCloserBody) Read(p []byte) (n int, err error) {
	if b.br != nil {
		if n := b.br.Buffered(); len(p) > n {
			p = p[:n]
		}
		n, err = b.br.Read(p)
		if b.br.Buffered() == 0 {
			b.br = nil
		}
		return n, err
	}
	return b.Conn.Read(p)
}

// -----------------

type hostAddrs struct {
	addrs       []string
	resolveTime time.Time
}

type domainBalancer struct {
	ResolveInterval time.Duration
	mu              sync.Mutex
	hostAddrs       map[string]hostAddrs
}

// Get addr info.
func (b *domainBalancer) GetEndpoint(ctx context.Context, req xnet.Request, addrUsed []string) (connInfo xnet.ConnInfo, err error) {

	r := GetTransportRequest(req)
	hostport := r.URL.Host
	host, port, e := net.SplitHostPort(hostport)
	if e != nil {
		host = hostport
		if r.URL.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}

	var addrs []string
	b.mu.Lock()

	if b.hostAddrs == nil {
		b.hostAddrs = make(map[string]hostAddrs)
	}
	now := time.Now()

	hostAddrs := b.hostAddrs[hostport]
	if hostAddrs.resolveTime.Add(b.ResolveInterval).Before(now) {
		hostAddrs.resolveTime = now
		addrs, err := net.LookupHost(host)
		if err == nil {
			hostAddrs.addrs = make([]string, len(addrs))
			for i, a := range addrs {
				hostAddrs.addrs[i] = net.JoinHostPort(a, port)
			}
		}
		b.hostAddrs[hostport] = hostAddrs
	}

	addrs = hostAddrs.addrs
	b.mu.Unlock()

	if len(addrs) > 0 {

		var usedMap map[string]bool
		if len(addrUsed) > 5 {
			usedMap := make(map[string]bool, len(addrUsed))
			for _, a := range addrUsed {
				usedMap[a] = true
			}
		}

		cnt := len(addrs)
		offset := 0
		if len(addrs) > 1 {
			offset = rand.Intn(len(addrs))
		}

		for i := 0; i < cnt; i++ {
			a := addrs[(i+offset)%cnt]
			if len(addrUsed) > 5 {
				if _, ok := usedMap[a]; ok {
					continue
				}
			} else {
				used := false
				for _, au := range addrUsed {
					if a == au {
						used = true
						break
					}
				}
				if used {
					continue
				}
			}

			return xnet.ConnInfo{
				Network: "tcp",
				Scheme:  r.URL.Scheme,
				Addr:    a,
				Host:    host,
			}, nil
		}
	}
	return xnet.ConnInfo{}, xnet.ErrGetEndpointFail
}

func (t *Transport) initTransport() {

	t.xTransport.DialTimeout = t.DialTimeout
	t.xTransport.WriteRequestTimeout = t.WriteRequestTimeout
	t.xTransport.ReadResponseTimeout = t.ResponseHeaderTimeout

	t.xTransport.SetRequest = t.setRequest
	t.xTransport.GotConn = t.gotConn
	t.xTransport.Tracer = t.Tracer

	t.xTransport.DialContext = t.DialContext
	t.xTransport.DialTLS = t.DialTLS
	t.xTransport.WriteRequest = t.writeRequest
	t.xTransport.ReadResponse = t.readResponse
	t.xTransport.MaxTryCount = t.MaxTryCount
	t.xTransport.TLSClientConfig = t.TLSClientConfig
	t.xTransport.TLSHandshakeTimeout = t.TLSHandshakeTimeout
	t.xTransport.DisableKeepAlives = t.DisableKeepAlives
	t.xTransport.MaxIdleConns = t.MaxIdleConns
	t.xTransport.MaxIdleConnsPerHost = t.MaxIdleConnsPerHost
	t.xTransport.MaxConnsPerHost = t.MaxConnsPerHost
	t.xTransport.IdleConnTimeout = t.IdleConnTimeout
	t.xTransport.ProxyConnectHeader = t.ProxyConnectHeader
	t.xTransport.WriteBufferSize = t.WriteBufferSize
	t.xTransport.ReadBufferSize = t.ReadBufferSize

	if t.Proxy != nil {
		t.xTransport.Proxy = t.proxy
	}

	t.xTransport.Balancer = t.Balancer
	if t.xTransport.Balancer == nil {
		t.xTransport.Balancer = DefaultClientBalancer
	}

}

func (t *Transport) GetTransport() *xnet.Transport {
	t.xTransportInitOnce.Do(t.initTransport)
	return &t.xTransport
}

func (t *Transport) proxy(req xnet.Request, info *xnet.ConnInfo) (*url.URL, error) {
	r := GetTransportRequest(req)
	if t.Proxy != nil {
		return t.Proxy(r, info)
	}
	return nil, nil
}

var defaultDomainBalancer = &domainBalancer{ResolveInterval: 60 * time.Second}
var DefaultClientBalancer = &xnet.Balancer{
	GetEndpoint: defaultDomainBalancer.GetEndpoint,
}
