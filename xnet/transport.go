// xnet contains common transport interface.
package xnet

import (
	"bufio"
	"container/list"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ccxp/xgo/xlog"
	"github.com/ccxp/xgo/xutil/xctx"
	"golang.org/x/net/proxy"
)

// ---------------------------------------------Transport

const (
	// DefaultMaxIdleConnsPerHost is the default value of Transport's
	// MaxIdleConnsPerHost.
	DefaultMaxIdleConnsPerHost = 20

	DefaultMaxTryCount = 3
)

type Request interface {
	Context() context.Context

	// Transport will call Close() when Context() is done, or Balancer.Addr fail.
	Close()
}

type NoResponseRequest interface {
	IsNoResponse() bool
}

type ReplayableRequest interface {

	// IsReplayable is called after read failed.
	IsReplayable(err error) bool

	// reset to replay if is replayable.
	Reset() error
}

type Response interface {
}

// If Request or Response has IsClose function, test the connection state use IsClose().
type IsCloseTester interface {
	// if IsClose return true, the connection will be close after read response.
	IsClose() bool
}

type ReadDelayResponse interface {
	// IsReadDelay should return true, if the response's body is delay to read after Transport.Roundtrip.
	// In Transport.Roundtrip, if Response.IsReadDelay return true,
	// it will call GetReader to get the body reader, then call WrapReader,
	// Response should replace body reader with newR pass by WrapReader.
	IsReadDelay() bool
	GetReader() (body io.ReadCloser, readBodyTimeout time.Duration)
	WrapReader(newR io.ReadCloser)
}

type ConnState struct {
	Conn     net.Conn
	TlsState *tls.ConnectionState

	TotalWriteBytes int64
	TotalReadBytes  int64

	// written and readed bytes in recent request
	WriteBytes int64
	ReadBytes  int64
}

type RequestConn struct {
	BR *bufio.Reader
	BW *bufio.Writer

	// Do not use Conn to read or write.
	ConnInfo ConnInfo
	Conn     net.Conn
	TlsState *tls.ConnectionState

	// report when conn is closed.
	ConnClosed <-chan struct{}

	// set if proxy is used
	ProxyUrl *url.URL
}

func (c *ConnState) Reset() {
	atomic.StoreInt64(&c.WriteBytes, 0)
	atomic.StoreInt64(&c.ReadBytes, 0)
}

func (c *ConnState) Clone() (dest ConnState) {
	dest.Conn = c.Conn
	dest.TlsState = c.TlsState
	dest.TotalWriteBytes = atomic.LoadInt64(&c.TotalWriteBytes)
	dest.TotalReadBytes = atomic.LoadInt64(&c.TotalReadBytes)
	dest.WriteBytes = atomic.LoadInt64(&c.WriteBytes)
	dest.ReadBytes = atomic.LoadInt64(&c.ReadBytes)

	return
}

type mockConn struct {
	net.Conn
	info *ConnState
}

func (c *mockConn) Read(b []byte) (n int, err error) {
	n, err = c.Conn.Read(b)
	if n > 0 {
		atomic.AddInt64(&c.info.ReadBytes, int64(n))
		atomic.AddInt64(&c.info.TotalReadBytes, int64(n))
	}
	return
}

func (c *mockConn) Write(b []byte) (n int, err error) {
	n, err = c.Conn.Write(b)
	if n > 0 {
		atomic.AddInt64(&c.info.WriteBytes, int64(n))
		atomic.AddInt64(&c.info.TotalWriteBytes, int64(n))
	}
	return
}

// delayReaderCallback is used for Transport.Read.
//
// If the response's body is to read after Transport.RoundTrip,
// use delayReaderCallbackWrapper to make your body reader.
type delayReaderCallback func(err error, isEarlyClose bool) error

type delayReaderWithTimeout interface {
	StartTimer()
}

func delayReaderCallbackWrapper(r io.ReadCloser, cb delayReaderCallback, delayReadTimeout time.Duration) (rc io.ReadCloser) {
	if c, ok := r.(net.Conn); ok {
		rc = &connEofSignal{
			Conn:    c,
			r:       r,
			fn:      cb,
			timeout: delayReadTimeout,
		}
	} else if rwc, ok := r.(io.ReadWriteCloser); ok {
		rc = &rwcEofSignal{
			ReadWriteCloser: rwc,
			r:               r,
			fn:              cb,
			timeout:         delayReadTimeout,
		}
	} else {
		rc = &eofSignal{
			r:       r,
			fn:      cb,
			timeout: delayReadTimeout,
		}
	}

	if delayReadTimeout > 0 {
		rc.(delayReaderWithTimeout).StartTimer()
	}

	return rc
}

type atomicBool int32

func (b *atomicBool) isSet() bool { return atomic.LoadInt32((*int32)(b)) != 0 }
func (b *atomicBool) setTrue()    { atomic.StoreInt32((*int32)(b), 1) }

// eofSignal is used by transport when reading response
// bodies to make sure we see the end of a response body before
// proceeding and reading on the connection again.
//
// It wraps a ReadCloser but runs fn (if non-nil) at most
// once, right before its final (error-producing) Read or Close call
// returns. fn should return the new error to return from Read or Close.
type eofSignal struct {
	r      io.ReadCloser
	fn     func(e error, isEarlyClose bool) error // err will be nil on Read io.EOF
	closed bool                                   // whether Close has been called
	rerr   error                                  // sticky Read error
	mu     sync.Mutex                             // guards following 4 fields

	timeout     time.Duration
	timer       *time.Timer
	stopTimerCh chan struct{}
	timedOut    atomicBool
}

func (es *eofSignal) Read(p []byte) (n int, err error) {
	es.mu.Lock()
	closed, rerr := es.closed, es.rerr
	es.mu.Unlock()
	if closed {
		return 0, errReadOnClosedResBody
	}
	if rerr != nil {
		return 0, rerr
	}

	n, err = es.r.Read(p)
	if err != nil {
		if es.timeout > 0 {
			if es.timedOut.isSet() {
				err = newTransportReadError(errReadTimeout, true)
			} else {
				es.stopTimerCh <- struct{}{}
			}
		}

		es.mu.Lock()
		defer es.mu.Unlock()
		if es.rerr == nil {
			es.rerr = err
		}

		err = es.condfn(err, false)
	}
	return
}

func (es *eofSignal) Close() error {
	es.mu.Lock()
	defer es.mu.Unlock()
	if es.closed {
		return nil
	}
	es.closed = true
	err := es.r.Close()

	if es.timeout > 0 && !es.timedOut.isSet() {
		es.stopTimerCh <- struct{}{}
	}

	return es.condfn(err, true)
}

func (es *eofSignal) condfn(err error, isEarlyClose bool) error {
	if es.fn == nil {
		return err
	}
	err = es.fn(err, isEarlyClose)
	es.fn = nil
	return err
}

func (es *eofSignal) StartTimer() {
	if es.timeout <= 0 {
		return
	}

	es.stopTimerCh = make(chan struct{}, 2)
	es.timer = time.NewTimer(es.timeout)

	go func() {
		select {
		case <-es.timer.C:
			es.timedOut.setTrue()
			es.r.Close()
		case <-es.stopTimerCh:
			es.timer.Stop()
		}
	}()
}

type connEofSignal struct {
	net.Conn

	r      io.ReadCloser
	fn     func(e error, isEarlyClose bool) error // err will be nil on Read io.EOF
	closed bool                                   // whether Close has been called
	rerr   error                                  // sticky Read error
	mu     sync.Mutex                             // guards following 4 fields

	timeout     time.Duration
	timer       *time.Timer
	stopTimerCh chan struct{}
	timedOut    atomicBool
}

func (es *connEofSignal) Read(p []byte) (n int, err error) {
	es.mu.Lock()
	closed, rerr := es.closed, es.rerr
	es.mu.Unlock()
	if closed {
		return 0, errReadOnClosedResBody
	}
	if rerr != nil {
		return 0, rerr
	}

	n, err = es.r.Read(p)
	if err != nil {
		if es.timeout > 0 {
			if es.timedOut.isSet() {
				err = newTransportReadError(errReadTimeout, true)
			} else {
				es.stopTimerCh <- struct{}{}
			}
		}

		es.mu.Lock()
		defer es.mu.Unlock()
		if es.rerr == nil {
			es.rerr = err
		}
		err = es.condfn(err, false)
	}
	return
}

func (es *connEofSignal) Close() error {
	es.mu.Lock()
	defer es.mu.Unlock()
	if es.closed {
		return nil
	}
	es.closed = true
	err := es.r.Close()

	if es.timeout > 0 && !es.timedOut.isSet() {
		es.stopTimerCh <- struct{}{}
	}

	return es.condfn(err, true)
}

func (es *connEofSignal) condfn(err error, isEarlyClose bool) error {
	if es.fn == nil {
		return err
	}
	err = es.fn(err, isEarlyClose)
	es.fn = nil
	return err
}

func (es *connEofSignal) StartTimer() {
	if es.timeout <= 0 {
		return
	}

	es.stopTimerCh = make(chan struct{}, 2)
	es.timer = time.NewTimer(es.timeout)

	go func() {
		select {
		case <-es.timer.C:
			es.timedOut.setTrue()
			es.r.Close()
		case <-es.stopTimerCh:
			es.timer.Stop()
		}
	}()
}

type rwcEofSignal struct {
	io.ReadWriteCloser

	r      io.ReadCloser
	fn     func(e error, isEarlyClose bool) error // err will be nil on Read io.EOF
	closed bool                                   // whether Close has been called
	rerr   error                                  // sticky Read error
	mu     sync.Mutex                             // guards following 4 fields

	timeout     time.Duration
	timer       *time.Timer
	stopTimerCh chan struct{}
	timedOut    atomicBool
}

func (es *rwcEofSignal) Read(p []byte) (n int, err error) {
	es.mu.Lock()
	closed, rerr := es.closed, es.rerr
	es.mu.Unlock()
	if closed {
		return 0, errReadOnClosedResBody
	}
	if rerr != nil {
		return 0, rerr
	}

	n, err = es.r.Read(p)
	if err != nil {
		if es.timeout > 0 {
			if es.timedOut.isSet() {
				err = newTransportReadError(errReadTimeout, true)
			} else {
				es.stopTimerCh <- struct{}{}
			}
		}

		es.mu.Lock()
		defer es.mu.Unlock()
		if es.rerr == nil {
			es.rerr = err
		}
		err = es.condfn(err, false)
	}
	return
}

func (es *rwcEofSignal) Close() error {
	es.mu.Lock()
	defer es.mu.Unlock()
	if es.closed {
		return nil
	}
	es.closed = true
	err := es.r.Close()

	if es.timeout > 0 && !es.timedOut.isSet() {
		es.stopTimerCh <- struct{}{}
	}

	return es.condfn(err, true)
}

func (es *rwcEofSignal) condfn(err error, isEarlyClose bool) error {
	if es.fn == nil {
		return err
	}
	err = es.fn(err, isEarlyClose)
	es.fn = nil
	return err
}

func (es *rwcEofSignal) StartTimer() {
	if es.timeout <= 0 {
		return
	}

	es.stopTimerCh = make(chan struct{}, 2)
	es.timer = time.NewTimer(es.timeout)

	go func() {
		select {
		case <-es.timer.C:
			es.timedOut.setTrue()
			es.r.Close()
		case <-es.stopTimerCh:
			es.timer.Stop()
		}
	}()
}

// Transport can be used as connection pool.
//
// Base on net.http.Transport, but support other protocol.
type Transport struct {
	idleMu     sync.Mutex
	wantIdle   bool                                // user has requested to close all idle conns
	idleConn   map[connectMethodKey][]*persistConn // most recently used at end
	idleConnCh map[connectMethodKey]chan *persistConn
	idleLRU    connLRU

	reqMu       sync.Mutex
	reqCanceler map[Request]func(error)

	connCountMu          sync.Mutex
	connPerHostCount     map[connectMethodKey]int
	connPerHostAvailable map[connectMethodKey]chan struct{}

	Balancer *Balancer
	Tracer   *Tracer

	// connnect
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)

	// TLS connections for requests.
	//
	// If DialTLS is nil, tls.Dial is used.
	//
	// If the returned net.Conn has a ConnectionState method like tls.Conn,
	// it will be used to set http.Response.TLS.
	DialTLS func(ctx context.Context, network, address string, cfg *tls.Config) (net.Conn, error)

	// if SetRequest is not nil, call SetRequest after got addr info by Balancer.GetEndpoint.
	// It can be used to set new Host to request.Url.
	SetRequest func(req Request, connInfo *ConnInfo) Request

	// 取得连接后调用，多次重试是串行调用。
	//
	// 对于重试有条件限制的，因为在连接情况下，如果取得旧连接的同时刚好断开，
	// 那第一个连接的WriteRequest和第二个连接的WriteRequest有可能会并发进行，
	// 而且GotConn是在取得连接后串行调用，所以可用于记录是否有第一次尝试写入请求。
	//
	GotConn func(req Request, connInfo *ConnInfo)

	// Write request.
	//
	// 注意如果是长连接，并且取得连接的瞬间断开，则重试的WriteRequest和上次的WriteRequest有可能并发进行,
	// 会对数据有影响，可使用GotConn记录状态.
	//
	WriteRequest func(w *bufio.Writer, req Request, rc *RequestConn) error

	// Read response.
	ReadResponse func(r *bufio.Reader, req Request, rc *RequestConn) (resp Response, err error)

	DialTimeout         time.Duration
	WriteRequestTimeout time.Duration
	ReadResponseTimeout time.Duration

	// MaxTryCount, if non-zero, controls the maximum tries per request.
	// If zero, DefaultMaxTryCount is used.
	MaxTryCount int

	// TLSClientConfig specifies the TLS configuration to use with
	// tls.Client.
	TLSClientConfig *tls.Config

	// TLSHandshakeTimeout specifies the maximum amount of time waiting to
	// wait for a TLS handshake. Zero means no timeout.
	TLSHandshakeTimeout time.Duration

	DisableKeepAlives bool

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

	Proxy func(Request, *ConnInfo) (*url.URL, error)
	// ProxyConnectHeader optionally specifies headers to send to
	// proxies during CONNECT requests.
	ProxyConnectHeader http.Header
}

// RoundTrip execute a RPC transaction.
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
func (t *Transport) RoundTrip(req Request) (resp Response, err error) {

	ctxx := &xctx.ValueCtx{}
	if t.Tracer != nil && t.Tracer.StatRoundTripBegin != nil {
		t.Tracer.StatRoundTripBegin(req, ctxx)
	}

	ctx := req.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	maxTryCount := t.maxTryCount()
	addrUsed := make([]string, 0, maxTryCount)

	respInfo := &TraceResultInfo{}
	ts := time.Now()
	defer func() {
		respInfo.Err = err
		respInfo.Duration = time.Since(ts)
		if t.Tracer != nil {
			if t.Tracer.StatResult != nil {
				t.Tracer.StatResult(req, resp, respInfo)
			}
			if t.Tracer.StatResultWithValue != nil {
				t.Tracer.StatResultWithValue(req, resp, respInfo, ctxx)
			}
		}
	}()

	var isRetry bool
	for respInfo.TryCount < maxTryCount {
		respInfo.TryCount++

		select {
		case <-ctx.Done():
			req.Close()
			return nil, ctx.Err()
		default:
		}

		connInfo, e := t.Balancer.GetEndpoint(ctx, req, addrUsed)
		if e != nil {
			if len(addrUsed) == 0 {
				err = &TransportGetEnpointError{e}

				if t.Tracer != nil && t.Tracer.StatGetEndpointFail != nil {
					t.Tracer.StatGetEndpointFail(req, err)
				}
			}

			req.Close()
			return nil, err
		}
		addrUsed = append(addrUsed, connInfo.Addr)
		respInfo.AddrUsed = addrUsed

		cm := &connectMethod{
			connInfo: &connInfo,
			connKey: &connectMethodKey{
				Network: connInfo.Network,
				Addr:    connInfo.Addr,
			},
		}
		if t.Proxy != nil {
			cm.proxyURL, _ = t.Proxy(req, cm.connInfo)
		}

		// treq gets modified by roundTrip, so we need to recreate for each retry.
		treq := &transportRequest{req: req, connInfo: &connInfo}
		if t.SetRequest != nil {
			treq.req = t.SetRequest(req, cm.connInfo)
		}

		pconn, e := t.getConn(treq, cm, respInfo)
		if e != nil {
			err = e
			t.setReqCanceler(req, nil)
			continue
		}

		if t.GotConn != nil {
			t.GotConn(req, cm.connInfo)
		}

		var resp Response
		resp, err = pconn.roundTrip(treq, respInfo)
		if err == nil {
			return resp, nil
		}

		if respInfo.TryCount >= maxTryCount {
			return nil, err
		}

		isRetry, err = pconn.shouldRetryRequest(treq.req, err)
		if !isRetry {
			return nil, err
		}

	}
	return
}

// CloseIdleConnections closes any connections which were previously
// connected from previous requests but are now sitting idle in
// a "keep-alive" state. It does not interrupt any connections currently
// in use.
func (t *Transport) CloseIdleConnections() {
	t.idleMu.Lock()
	m := t.idleConn
	t.idleConn = nil
	t.idleConnCh = nil
	t.wantIdle = true
	t.idleLRU = connLRU{}
	t.idleMu.Unlock()
	for _, conns := range m {
		for _, pconn := range conns {
			pconn.close(errCloseIdleConns)
		}
	}
}

// Cancel an in-flight request, recording the error value.
func (t *Transport) cancelRequest(req Request, err error) {
	t.reqMu.Lock()
	cancel := t.reqCanceler[req]
	delete(t.reqCanceler, req)
	t.reqMu.Unlock()
	if cancel != nil {
		cancel(err)
	}
}

func (t *Transport) putOrCloseIdleConn(pconn *persistConn) {
	if err := t.tryPutIdleConn(pconn); err != nil {
		pconn.close(err)
	}
}

func (t *Transport) maxIdleConnsPerHost() int {
	if v := t.MaxIdleConnsPerHost; v != 0 {
		return v
	}
	return DefaultMaxIdleConnsPerHost
}

func (t *Transport) maxTryCount() int {
	if v := t.MaxTryCount; v != 0 {
		return v
	}
	return DefaultMaxTryCount
}

// tryPutIdleConn adds pconn to the list of idle persistent connections awaiting
// a new request.
// If pconn is no longer needed or not in a good state, tryPutIdleConn returns
// an error explaining why it wasn't registered.
// tryPutIdleConn does not close pconn. Use putOrCloseIdleConn instead for that.
func (t *Transport) tryPutIdleConn(pconn *persistConn) error {
	if t.DisableKeepAlives || t.MaxIdleConnsPerHost < 0 {
		return errKeepAlivesDisabled
	}
	if pconn.isBroken() {
		return errConnBroken
	}

	pconn.markReused()
	key := pconn.cacheKey

	t.idleMu.Lock()
	defer t.idleMu.Unlock()

	waitingDialer := t.idleConnCh[key]
	select {
	case waitingDialer <- pconn:
		// We're done with this pconn and somebody else is
		// currently waiting for a conn of this type (they're
		// actively dialing, but this conn is ready
		// first). Chrome calls this socket late binding. See
		// https://insouciant.org/tech/connection-management-in-chromium/
		return nil
	default:
		if waitingDialer != nil {
			// They had populated this, but their dial won
			// first, so we can clean up this map entry.
			delete(t.idleConnCh, key)
		}
	}
	if t.wantIdle {
		return errWantIdle
	}
	if t.idleConn == nil {
		t.idleConn = make(map[connectMethodKey][]*persistConn)
	}
	idles := t.idleConn[key]
	if len(idles) >= t.maxIdleConnsPerHost() {
		return errTooManyIdleHost
	}
	for _, exist := range idles {
		if exist == pconn {
			xlog.Errorf("dup idle pconn %p in freelist", pconn)
		}
	}
	t.idleConn[key] = append(idles, pconn)
	t.idleLRU.add(pconn)
	if t.MaxIdleConns != 0 && t.idleLRU.len() > t.MaxIdleConns {
		oldest := t.idleLRU.removeOldest()
		oldest.close(errTooManyIdle)
		t.removeIdleConnLocked(oldest)
	}
	if t.IdleConnTimeout > 0 {
		if pconn.idleTimer != nil {
			pconn.idleTimer.Reset(t.IdleConnTimeout)
		} else {
			pconn.idleTimer = time.AfterFunc(t.IdleConnTimeout, pconn.closeConnIfStillIdle)
		}
	}
	pconn.idleAt = time.Now()
	return nil
}

// getIdleConnCh returns a channel to receive and return idle
// persistent connection for the given connectMethod.
// It may return nil, if persistent connections are not being used.
func (t *Transport) getIdleConnCh(key connectMethodKey) chan *persistConn {
	if t.DisableKeepAlives {
		return nil
	}
	t.idleMu.Lock()
	defer t.idleMu.Unlock()
	t.wantIdle = false
	if t.idleConnCh == nil {
		t.idleConnCh = make(map[connectMethodKey]chan *persistConn)
	}
	ch, ok := t.idleConnCh[key]
	if !ok {
		ch = make(chan *persistConn)
		t.idleConnCh[key] = ch
	}
	return ch
}

func (t *Transport) getIdleConn(key connectMethodKey) (pconn *persistConn, idleSince time.Time) {
	t.idleMu.Lock()
	defer t.idleMu.Unlock()

	for {
		pconns, ok := t.idleConn[key]
		if !ok {
			return nil, time.Time{}
		}
		if len(pconns) == 1 {
			pconn = pconns[0]
			delete(t.idleConn, key)
		} else {
			// 2 or more cached connections; use the most
			// recently used one at the end.
			pconn = pconns[len(pconns)-1]
			t.idleConn[key] = pconns[:len(pconns)-1]
		}
		t.idleLRU.remove(pconn)
		if pconn.isBroken() {
			// There is a tiny window where this is
			// possible, between the connecting dying and
			// the persistConn readLoop calling
			// Transport.removeIdleConn. Just skip it and
			// carry on.
			continue
		}
		return pconn, pconn.idleAt
	}
}

// removeIdleConn marks pconn as dead.
func (t *Transport) removeIdleConn(pconn *persistConn) {
	t.idleMu.Lock()
	defer t.idleMu.Unlock()
	t.removeIdleConnLocked(pconn)
}

// t.idleMu must be held.
func (t *Transport) removeIdleConnLocked(pconn *persistConn) {
	if pconn.idleTimer != nil {
		pconn.idleTimer.Stop()
	}
	t.idleLRU.remove(pconn)
	key := pconn.cacheKey
	pconns := t.idleConn[key]
	switch len(pconns) {
	case 0:
		// Nothing
	case 1:
		if pconns[0] == pconn {
			delete(t.idleConn, key)
		}
	default:
		for i, v := range pconns {
			if v != pconn {
				continue
			}
			// Slide down, keeping most recently-used
			// conns at the end.
			copy(pconns[i:], pconns[i+1:])
			t.idleConn[key] = pconns[:len(pconns)-1]
			break
		}
	}
}

func (t *Transport) setReqCanceler(r Request, fn func(error)) {
	t.reqMu.Lock()
	defer t.reqMu.Unlock()
	if t.reqCanceler == nil {
		t.reqCanceler = make(map[Request]func(error))
	}
	if fn != nil {
		t.reqCanceler[r] = fn
	} else {
		delete(t.reqCanceler, r)
	}
}

// replaceReqCanceler replaces an existing cancel function. If there is no cancel function
// for the request, we don't set the function and return false.
// Since CancelRequest will clear the canceler, we can use the return value to detect if
// the request was canceled since the last setReqCancel call.
func (t *Transport) replaceReqCanceler(r Request, fn func(error)) bool {
	t.reqMu.Lock()
	defer t.reqMu.Unlock()
	_, ok := t.reqCanceler[r]
	if !ok {
		return false
	}
	if fn != nil {
		t.reqCanceler[r] = fn
	} else {
		delete(t.reqCanceler, r)
	}
	return true
}

var zeroDialer net.Dialer

func (t *Transport) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if t.DialContext != nil {
		return t.DialContext(ctx, network, addr)
	}
	if t.DialTimeout == 0 {
		return zeroDialer.DialContext(ctx, network, addr)
	}
	dialer := net.Dialer{Timeout: t.DialTimeout}
	return dialer.DialContext(ctx, network, addr)
}

// getConn dials and creates a new persistConn to the target as
// specified in the connectMethod. This includes doing a proxy CONNECT
// and/or setting up TLS.  If this doesn't return an error, the persistConn
// is ready to write requests to.
func (t *Transport) getConn(treq *transportRequest, cm *connectMethod, respInfo *TraceResultInfo) (*persistConn, error) {
	req := treq.req
	ctx := req.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	ts := time.Now()
	gotConnInfo := &TraceGotConnInfo{
		TryCount: respInfo.TryCount,
	}
	connInfo := *cm.connInfo
	connKey := *cm.connKey
	defer func() {
		gotConnInfo.Duration = time.Since(ts)
		if t.Balancer.StatConn != nil {
			t.Balancer.StatConn(treq.req, &connInfo, gotConnInfo)
		}
		if t.Tracer != nil && t.Tracer.StatConn != nil {
			t.Tracer.StatConn(treq.req, &connInfo, gotConnInfo)
		}
	}()

	if pc, idleSince := t.getIdleConn(connKey); pc != nil {
		pc.gotIdleConnTrace(idleSince, gotConnInfo)

		// set request canceler to some non-nil function so we
		// can detect whether it was cleared between now and when
		// we enter roundTrip
		t.setReqCanceler(req, func(error) {})
		return pc, nil
	}

	type dialRes struct {
		pc  *persistConn
		err error
	}
	dialc := make(chan dialRes)

	handlePendingDial := func() {
		go func() {
			if v := <-dialc; v.err == nil {
				t.putOrCloseIdleConn(v.pc)
			} else {
				t.decHostConnCount(connKey)
			}
		}()
	}

	cancelc := make(chan error, 1)
	t.setReqCanceler(req, func(err error) { cancelc <- err })

	if t.MaxConnsPerHost > 0 {
		select {
		case <-t.incHostConnCount(connKey):
			// count below conn per host limit; proceed
		case pc := <-t.getIdleConnCh(connKey):
			gotConnInfo.Conn = pc.conn
			gotConnInfo.Reused = pc.isReused()
			return pc, nil
		case <-ctx.Done():
			gotConnInfo.Err = ctx.Err()
			return nil, gotConnInfo.Err
		case err := <-cancelc:
			if err == ErrRequestCanceled {
				err = ErrRequestCanceledConn
			}
			gotConnInfo.Err = err
			return nil, err
		}
	}

	go func() {
		pc, err := t.dialConn(ctx, cm)
		dialc <- dialRes{pc, err}
	}()

	idleConnCh := t.getIdleConnCh(connKey)
	select {
	case v := <-dialc:
		// Our dial finished.
		if v.pc != nil {
			gotConnInfo.Conn = v.pc.conn
			return v.pc, nil
		}
		// Our dial failed. See why to return a nicer error
		// value.
		t.decHostConnCount(connKey)
		select {
		case <-ctx.Done():
			gotConnInfo.Err = ctx.Err()
			return nil, gotConnInfo.Err
		case err := <-cancelc:
			if err == ErrRequestCanceled {
				err = ErrRequestCanceledConn
			}
			gotConnInfo.Err = err
			return nil, err
		default:
			err := newTransportConnError(v.err)
			gotConnInfo.Err = err
			return nil, err
		}
	case pc := <-idleConnCh:
		// Another request finished first and its net.Conn
		// became available before our dial. Or somebody
		// else's dial that they didn't use.
		// But our dial is still going, so give it away
		// when it finishes:
		handlePendingDial()
		gotConnInfo.Conn = pc.conn
		gotConnInfo.Reused = pc.isReused()
		return pc, nil
	case <-ctx.Done():
		handlePendingDial()
		gotConnInfo.Err = ctx.Err()
		return nil, gotConnInfo.Err
	case err := <-cancelc:
		handlePendingDial()
		if err == ErrRequestCanceled {
			err = ErrRequestCanceledConn
		}
		gotConnInfo.Err = err
		return nil, err
	}
}

// incHostConnCount increments the count of connections for a
// given host. It returns an already-closed channel if the count
// is not at its limit; otherwise it returns a channel which is
// notified when the count is below the limit.
func (t *Transport) incHostConnCount(cmKey connectMethodKey) <-chan struct{} {
	if t.MaxConnsPerHost <= 0 {
		return connsPerHostClosedCh
	}
	t.connCountMu.Lock()
	defer t.connCountMu.Unlock()
	if t.connPerHostCount[cmKey] == t.MaxConnsPerHost {
		if t.connPerHostAvailable == nil {
			t.connPerHostAvailable = make(map[connectMethodKey]chan struct{})
		}
		ch, ok := t.connPerHostAvailable[cmKey]
		if !ok {
			ch = make(chan struct{})
			t.connPerHostAvailable[cmKey] = ch
		}
		return ch
	}
	if t.connPerHostCount == nil {
		t.connPerHostCount = make(map[connectMethodKey]int)
	}
	t.connPerHostCount[cmKey]++
	// return a closed channel to avoid race: if decHostConnCount is called
	// after incHostConnCount and during the nil check, decHostConnCount
	// will delete the channel since it's not being listened on yet.
	return connsPerHostClosedCh
}

// decHostConnCount decrements the count of connections
// for a given host.
// See Transport.MaxConnsPerHost.
func (t *Transport) decHostConnCount(cmKey connectMethodKey) {
	if t.MaxConnsPerHost <= 0 {
		return
	}
	t.connCountMu.Lock()
	defer t.connCountMu.Unlock()
	t.connPerHostCount[cmKey]--
	select {
	case t.connPerHostAvailable[cmKey] <- struct{}{}:
	default:
		// close channel before deleting avoids getConn waiting forever in
		// case getConn has reference to channel but hasn't started waiting.
		// This could lead to more than MaxConnsPerHost in the unlikely case
		// that > 1 go routine has fetched the channel but none started waiting.
		if t.connPerHostAvailable[cmKey] != nil {
			close(t.connPerHostAvailable[cmKey])
		}
		delete(t.connPerHostAvailable, cmKey)
	}
	if t.connPerHostCount[cmKey] == 0 {
		delete(t.connPerHostCount, cmKey)
	}
}

// connCloseListener wraps a connection, the transport that dialed it
// and the connected-to host key so the host connection count can be
// transparently decremented by whatever closes the embedded connection.
type connCloseListener struct {
	net.Conn
	t        *Transport
	cmKey    connectMethodKey
	didClose int32
}

func (c *connCloseListener) Close() error {
	if atomic.AddInt32(&c.didClose, 1) != 1 {
		return nil
	}
	err := c.Conn.Close()
	c.t.decHostConnCount(c.cmKey)
	return err
}

// Add TLS to a persistent connection, i.e. negotiate a TLS session. If pconn is already a TLS
// tunnel, this function establishes a nested TLS session inside the encrypted channel.
// The remote endpoint's name may be overridden by TLSClientConfig.ServerName.
func (pconn *persistConn) addTLS(name string) error {
	// Initiate TLS and check remote host name against certificate.
	cfg := cloneTLSConfig(pconn.t.TLSClientConfig)
	if cfg.ServerName == "" {
		cfg.ServerName = name
	}
	plainConn := pconn.conn
	tlsConn := tls.Client(plainConn, cfg)
	errc := make(chan error, 2)
	var timer *time.Timer // for canceling TLS handshake
	if d := pconn.t.TLSHandshakeTimeout; d != 0 {
		timer = time.AfterFunc(d, func() {
			errc <- errTlsHandshakeTimeout
		})
	}
	go func() {
		err := tlsConn.Handshake()
		if timer != nil {
			timer.Stop()
		}
		errc <- err
	}()
	if err := <-errc; err != nil {
		plainConn.Close()
		return err
	}
	cs := tlsConn.ConnectionState()
	/*if trace != nil && trace.TLSHandshakeDone != nil {
		trace.TLSHandshakeDone(cs, nil)
	}*/
	pconn.connState.TlsState = &cs
	pconn.conn = tlsConn
	return nil
}

func (t *Transport) dialConn(ctx context.Context, cm *connectMethod) (*persistConn, error) {
	pconn := &persistConn{
		t:             t,
		cacheKey:      *cm.connKey,
		connInfo:      *cm.connInfo,
		reqch:         make(chan requestAndChan, 1),
		writech:       make(chan writeRequest, 1),
		closech:       make(chan struct{}),
		writeErrCh:    make(chan error, 1),
		writeLoopDone: make(chan struct{}),
	}
	wrapErr := func(err error) error {
		if cm.proxyURL != nil {
			// Return a typed error, per Issue 16997
			return &net.OpError{Op: "proxyconnect", Net: "tcp", Err: err}
		}
		if _, ok := err.(*net.OpError); !ok {
			// Return a typed error, per Issue 16997
			return &net.OpError{Op: "dial", Net: cm.connInfo.Network, Err: err}
		}
		return err
	}

	if cm.connInfo.Scheme == "https" && t.DialTLS != nil {
		var err error
		pconn.conn, err = t.DialTLS(ctx, cm.connInfo.Network, cm.addr(), t.TLSClientConfig)
		if err != nil {
			return nil, wrapErr(err)
		}
		if pconn.conn == nil {
			return nil, wrapErr(errors.New("Transport.DialTLS returned (nil, nil)"))
		}
		if tc, ok := pconn.conn.(*tls.Conn); ok {
			// Handshake here, in case DialTLS didn't. TLSNextProto below
			// depends on it for knowing the connection state.
			if err := tc.Handshake(); err != nil {
				go pconn.conn.Close()
				return nil, err
			}
			cs := tc.ConnectionState()
			pconn.connState.TlsState = &cs
		}
	} else {
		var err error
		var conn net.Conn
		if cm.proxyURL == nil || cm.proxyURL.Scheme != "socks5" {
			conn, err = t.dial(ctx, cm.connInfo.Network, cm.addr())
			if err != nil {
				return nil, wrapErr(err)
			}
		}

		if cm.proxyURL != nil {
			switch {
			case cm.proxyURL.Scheme == "socks5":
				cm.isProxy = true
				var auth *proxy.Auth

				if u := cm.proxyURL.User; u != nil {
					auth = &proxy.Auth{
						User: u.Username(),
					}
					auth.Password, _ = u.Password()
				}
				d, err := proxy.SOCKS5("tcp", cm.addr(), auth, nil)
				if err != nil {
					return nil, err
				}
				if conn, err = d.Dial(cm.connInfo.Network, cm.connInfo.Addr); err != nil {
					return nil, err
				}
			case cm.connInfo.Scheme == "http":
				cm.isProxy = true
			case cm.connInfo.Scheme == "https":
				cm.isProxy = true
				hdr := t.ProxyConnectHeader
				if hdr == nil {
					hdr = make(http.Header)
				}
				connectReq := &http.Request{
					Method: "CONNECT",
					URL:    &url.URL{Opaque: cm.connInfo.Addr},
					Host:   cm.connInfo.Host,
					Header: hdr,
				}
				if pa := cm.proxyAuth(); pa != "" {
					connectReq.Header.Set("Proxy-Authorization", pa)
				}
				connectReq.Write(conn)

				// Read response.
				// Okay to use and discard buffered reader here, because
				// TLS server will not speak until spoken to.
				br := bufio.NewReader(conn)
				resp, err := http.ReadResponse(br, connectReq)
				if err != nil {
					conn.Close()
					return nil, err
				}
				if resp.StatusCode != 200 {
					f := strings.SplitN(resp.Status, " ", 2)
					conn.Close()
					if len(f) < 2 {
						return nil, errors.New("unknown status code")
					}
					return nil, errors.New(f[1])
				}
			}
		}

		pconn.conn = conn
		if cm.connInfo.Scheme == "https" {
			var firstTLSHost string
			if firstTLSHost, _, err = net.SplitHostPort(cm.connInfo.Host); err != nil {
				firstTLSHost = cm.connInfo.Host
			}
			if err = pconn.addTLS(firstTLSHost); err != nil {
				return nil, wrapErr(err)
			}
		}
	}

	if t.MaxConnsPerHost > 0 {
		pconn.conn = &connCloseListener{Conn: pconn.conn, t: t, cmKey: pconn.cacheKey}
	}

	if cm.isProxy {
		pconn.proxyURL = cm.proxyURL
	}

	pconn.connState.Conn = pconn.conn
	pconn.conn = &mockConn{pconn.conn, &pconn.connState}

	pconn.br = bufio.NewReader(pconn.conn)
	pconn.bw = bufio.NewWriter(pconn.conn)

	go pconn.readLoop()
	go pconn.writeLoop()
	return pconn, nil
}

// --------------------------------------------connLRU

type connLRU struct {
	ll *list.List // list.Element.Value type is of *persistConn
	m  map[*persistConn]*list.Element
}

// add adds pc to the head of the linked list.
func (cl *connLRU) add(pc *persistConn) {
	if cl.ll == nil {
		cl.ll = list.New()
		cl.m = make(map[*persistConn]*list.Element)
	}
	ele := cl.ll.PushFront(pc)
	if _, ok := cl.m[pc]; ok {
		panic("persistConn was already in LRU")
	}
	cl.m[pc] = ele
}

func (cl *connLRU) removeOldest() *persistConn {
	ele := cl.ll.Back()
	pc := ele.Value.(*persistConn)
	cl.ll.Remove(ele)
	delete(cl.m, pc)
	return pc
}

// remove removes pc from cl.
func (cl *connLRU) remove(pc *persistConn) {
	if ele, ok := cl.m[pc]; ok {
		cl.ll.Remove(ele)
		delete(cl.m, pc)
	}
}

// len returns the number of items in the cache.
func (cl *connLRU) len() int {
	return len(cl.m)
}

// --------------------------------------------persistConn

// persistConn wraps a connection, usually a persistent one
// (but may be used for non-keep-alive requests as well)
type persistConn struct {
	t         *Transport
	cacheKey  connectMethodKey
	conn      net.Conn
	connInfo  ConnInfo
	connState ConnState
	br        *bufio.Reader // from conn
	bw        *bufio.Writer // to conn

	reqch   chan requestAndChan // written by roundTrip; read by readLoop
	writech chan writeRequest   // written by roundTrip; read by writeLoop
	closech chan struct{}       // closed when conn closed

	sawEOF    bool  // whether we've seen EOF from conn; owned by readLoop
	readLimit int64 // bytes allowed to be read; owned by readLoop
	// writeErrCh passes the request write error (usually nil)
	// from the writeLoop goroutine to the readLoop which passes
	// it off to the res.Body reader, which then uses it to decide
	// whether or not a connection can be reused. Issue 7569.
	writeErrCh chan error

	writeLoopDone chan struct{} // closed when write loop ends

	// Both guarded by Transport.idleMu:
	idleAt    time.Time   // time it last become idle
	idleTimer *time.Timer // holding an AfterFunc to close it

	mu                   sync.Mutex // guards following fields
	numExpectedResponses int
	closed               error // set non-nil when conn is closed, before closech is closed
	canceledErr          error // set non-nil if conn is canceled
	broken               bool  // an error has happened on this connection; marked broken so it's not reused.
	reused               bool  // whether conn has had successful request/response and is being reused.

	proxyURL *url.URL // nil for no proxy, else full proxy URL

}

func (pc *persistConn) Read(p []byte) (n int, err error) {
	n, err = pc.conn.Read(p)
	if err == io.EOF {
		pc.sawEOF = true
	}
	return
}

// shouldRetryRequest reports whether we should retry sending a failed
// HTTP request on a new connection. The non-nil input error is the
// error from roundTrip.
func (pc *persistConn) shouldRetryRequest(req Request, err error) (bool, error) {

	if err == ErrServerClosedConn {
		// The server replied with io.EOF while we were trying to
		// read the response. Probably an unfortunately keep-alive
		// timeout, just as the client was writing a request.
		return true, err
	}

	if r, ok := req.(ReplayableRequest); ok {
		// Don't retry non-idempotent requests.
		ok = r.IsReplayable(err)
		if ok {
			e := r.Reset()
			if e == nil {
				return true, err
			}
			err = e
		}
	}
	return false, err
}

func (pc *persistConn) roundTrip(req *transportRequest, respInfo *TraceResultInfo) (resp Response, err error) {

	if !pc.t.replaceReqCanceler(req.req, pc.cancelRequest) {
		pc.t.putOrCloseIdleConn(pc)
		return nil, ErrRequestCanceled
	}

	noResponse := false
	if noRespReq, ok := req.req.(NoResponseRequest); ok {
		noResponse = noRespReq.IsNoResponse()
	}

	if !noResponse {
		pc.mu.Lock()
		pc.numExpectedResponses++
		pc.mu.Unlock()
	}

	gone := make(chan struct{})
	defer close(gone)

	pc.connState.Reset()
	ts := time.Now()

	defer func() {

		respInfo.Err = err
		respInfo.Duration = time.Since(ts)

		connInfo := req.connInfo
		connState := pc.connState.Clone()

		if pc.t.Balancer.StatRoundTrip != nil {
			pc.t.Balancer.StatRoundTrip(req.req, resp, connInfo, &connState, respInfo)
		}
		if pc.t.Tracer != nil && pc.t.Tracer.StatRoundTrip != nil {
			pc.t.Tracer.StatRoundTrip(req.req, resp, connInfo, &connState, respInfo)
		}

		if err != nil {
			pc.t.setReqCanceler(req.req, nil)
		}

	}()

	const debugRoundTrip = false

	// Write the request concurrently with waiting for a response,
	// in case the server decides to reply before reading our full
	// request body.

	writeErrCh := make(chan error, 1)
	pc.writech <- writeRequest{req, writeErrCh}

	resc := make(chan responseAndError)
	if !noResponse {
		pc.reqch <- requestAndChan{
			req:        req.req,
			connInfo:   req.connInfo,
			ch:         resc,
			callerGone: gone,
		}
	}

	var reqT *time.Timer
	var reqTimer, respTimer <-chan time.Time
	{
		d := pc.t.WriteRequestTimeout
		if req.connInfo.WriteRequestTimeout > 0 {
			d = req.connInfo.WriteRequestTimeout
		}
		if d > 0 {
			reqT = time.NewTimer(d)
			reqTimer = reqT.C
			defer func() {
				if reqT != nil {
					reqT.Stop() // prevent leaks
				}
			}()
		}
	}

	ctx := req.req.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctxDoneChan := ctx.Done()

	for {
		select {
		case err := <-writeErrCh:
			if debugRoundTrip {
				xlog.Errorf("writeErrCh resv: %T/%#v", err, err)
			}
			if reqT != nil {
				reqT.Stop()
				reqT = nil
			}
			if err != nil {
				err = newTransportWriteError(err, false, pc.connState.WriteBytes)
				pc.close(fmt.Errorf("write error: %v", err))
				return nil, pc.mapRoundTripError(req, err)
			}
			if noResponse {
				alive := true
				if isCloseTester, ok := req.req.(IsCloseTester); ok {
					alive = !isCloseTester.IsClose()
				}

				var closeErr error = errReadLoopExiting
				tryPutIdleConn := func() bool {
					if err := pc.t.tryPutIdleConn(pc); err != nil {
						closeErr = err
						if pc.t.Balancer.PutIdleConn != nil {
							pc.t.Balancer.PutIdleConn(req.connInfo, err)
						}
						return false
					}
					if pc.t.Balancer.PutIdleConn != nil {
						pc.t.Balancer.PutIdleConn(req.connInfo, nil)
					}
					return true
				}
				pc.t.setReqCanceler(req.req, nil)

				alive = alive &&
					!pc.sawEOF &&
					pc.wroteRequest() &&
					tryPutIdleConn()
				if !alive {
					pc.close(closeErr)
				}

				return nil, nil
			}
			d := pc.t.ReadResponseTimeout
			if req.connInfo.ReadResponseTimeout > 0 {
				d = req.connInfo.ReadResponseTimeout
			}
			if d > 0 {
				if debugRoundTrip {
					xlog.Errorf("starting timer for %v", d)
				}
				timer := time.NewTimer(d)
				defer timer.Stop() // prevent leaks
				respTimer = timer.C
			}
		case <-pc.closech:
			if debugRoundTrip {
				xlog.Errorf("closech recv: %T %#v", pc.closed, pc.closed)
			}
			return nil, pc.mapRoundTripError(req, pc.closed)
		case <-reqTimer:
			if debugRoundTrip {
				xlog.Errorf("timeout waiting for write request.")
			}

			err = newTransportWriteError(errWriteTimeout, true, 0)
			pc.close(err)
			return nil, err
		case <-respTimer:
			if debugRoundTrip {
				xlog.Errorf("timeout waiting for response headers.")
			}

			// consume pc.writeErrCh
			select {
			case <-pc.writeErrCh:
			default:
			}

			err = newTransportReadError(errReadTimeout, true)
			pc.close(err)
			return nil, err
		case re := <-resc:
			if (re.res == nil) == (re.err == nil) {
				panic(fmt.Sprintf("internal error: exactly one of res or err should be set; nil=%v", re.res == nil))
			}
			if debugRoundTrip {
				xlog.Errorf("resc recv: %p, %T/%#v", re.res, re.err, re.err)
			}
			if re.err != nil {
				err = newTransportReadError(re.err, false)
				return nil, pc.mapRoundTripError(req, err)
			}
			return re.res, nil
		case <-ctxDoneChan:
			pc.t.cancelRequest(req.req, ctx.Err())
			ctxDoneChan = nil
		}
	}
}

// isBroken reports whether this connection is in a known broken state.
func (pc *persistConn) isBroken() bool {
	pc.mu.Lock()
	b := pc.closed != nil
	pc.mu.Unlock()
	return b
}

// canceled returns non-nil if the connection was closed due to
// CancelRequest or due to context cancelation.
func (pc *persistConn) canceled() error {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return pc.canceledErr
}

// isReused reports whether this connection is in a known broken state.
func (pc *persistConn) isReused() bool {
	pc.mu.Lock()
	r := pc.reused
	pc.mu.Unlock()
	return r
}

func (pc *persistConn) gotIdleConnTrace(idleAt time.Time, t *TraceGotConnInfo) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	t.Reused = pc.reused
	t.Conn = pc.conn
	t.WasIdle = true
	if !idleAt.IsZero() {
		t.IdleTime = time.Since(idleAt)
	}
	return
}

func (pc *persistConn) cancelRequest(err error) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.canceledErr = err
	pc.closeLocked(ErrRequestCanceled)
}

// closeConnIfStillIdle closes the connection if it's still sitting idle.
// This is what's called by the persistConn's idleTimer, and is run in its
// own goroutine.
func (pc *persistConn) closeConnIfStillIdle() {
	t := pc.t
	t.idleMu.Lock()
	defer t.idleMu.Unlock()
	if _, ok := t.idleLRU.m[pc]; !ok {
		// Not idle.
		return
	}
	t.removeIdleConnLocked(pc)
	pc.close(errIdleConnTimeout)
}

// mapRoundTripError returns the appropriate error value for
// persistConn.roundTrip.
//
// The provided err is the first error that (*persistConn).roundTrip
// happened to receive from its select statement.
func (pc *persistConn) mapRoundTripError(req *transportRequest, err error) error {
	if err == nil {
		return nil
	}

	// If the request was canceled, that's better than network
	// failures that were likely the result of tearing down the
	// connection.
	if cerr := pc.canceled(); cerr != nil {
		return cerr
	}

	// See if an error was set explicitly.
	req.mu.Lock()
	reqErr := req.err
	req.mu.Unlock()
	if reqErr != nil {
		return reqErr
	}

	if err == ErrServerClosedConn {
		// Don't decorate
		return err
	}

	if _, ok := err.(*TransportReadError); ok {
		// Don't decorate
		return err
	}
	if pc.isBroken() {
		<-pc.writeLoopDone
		if e, ok := err.(*TransportWriteError); ok {
			e.WriteBytes = pc.connState.WriteBytes
			return e
		} else {
			if pc.connState.WriteBytes == 0 {
				return newTransportError(err, true)
			}
		}
		return fmt.Errorf("transport connection broken: %v", err)
	}

	return err
}

func (pc *persistConn) readLoop() {
	br := pc.br
	rcc := &RequestConn{
		BR: pc.br,
		BW: pc.bw,

		ConnInfo:   pc.connInfo,
		Conn:       pc.connState.Conn,
		TlsState:   pc.connState.TlsState,
		ConnClosed: pc.closech,
	}

	closeErr := errReadLoopExiting // default value, if not changed below
	defer func() {
		pc.close(closeErr)
		pc.t.removeIdleConn(pc)
		pc.br = nil
	}()

	tryPutIdleConn := func() bool {
		if err := pc.t.tryPutIdleConn(pc); err != nil {
			closeErr = err
			if pc.t.Balancer.PutIdleConn != nil {
				pc.t.Balancer.PutIdleConn(&rcc.ConnInfo, err)
			}
			return false
		}
		if pc.t.Balancer.PutIdleConn != nil {
			pc.t.Balancer.PutIdleConn(&rcc.ConnInfo, nil)
		}
		return true
	}

	// eofc is used to block caller goroutines reading from Response.Body
	// at EOF until this goroutines has (potentially) added the connection
	// back to the idle pool.
	eofc := make(chan struct{})
	defer close(eofc) // unblock reader on errors

	alive := true
	for alive {
		_, err := br.Peek(1)

		pc.mu.Lock()
		if pc.numExpectedResponses == 0 {
			pc.readLoopPeekFailLocked(err)
			pc.mu.Unlock()
			return
		}
		pc.mu.Unlock()

		rc := <-pc.reqch
		rcc.ConnInfo = *rc.connInfo

		var resp Response
		if err == nil {
			resp, err = pc.t.ReadResponse(br, rc.req, rcc)
		} else {
			closeErr = err
		}

		if err != nil {

			// consume pc.writeErrCh
			select {
			case <-pc.writeErrCh:
			default:
			}

			select {
			case rc.ch <- responseAndError{err: err}:
			case <-rc.callerGone:
				return
			}
			return
		}

		pc.mu.Lock()
		pc.numExpectedResponses--
		pc.mu.Unlock()

		if isCloseTester, ok := resp.(IsCloseTester); ok {
			alive = !isCloseTester.IsClose()
		}
		if isCloseTester, ok := rc.req.(IsCloseTester); ok {
			alive = !isCloseTester.IsClose()
		}

		readDelayResponse, delay := resp.(ReadDelayResponse)
		var delayBody io.ReadCloser
		var delayReadTimeout time.Duration
		if delay {
			delay = readDelayResponse.IsReadDelay()
			if delay {
				delayBody, delayReadTimeout = readDelayResponse.GetReader()
			}
		}
		if !delay {
			pc.t.setReqCanceler(rc.req, nil)

			// Put the idle conn back into the pool before we send the response
			// so if they process it quickly and make another request, they'll
			// get this same conn. But we use the unbuffered channel 'rc'
			// to guarantee that persistConn.roundTrip got out of its select
			// potentially waiting for this persistConn to close.
			// but after
			alive = alive &&
				!pc.sawEOF &&
				pc.wroteRequest() &&
				tryPutIdleConn()

			select {
			case rc.ch <- responseAndError{res: resp}:
			case <-rc.callerGone:
				return
			}

			continue
		}

		waitForBodyRead := make(chan bool, 2)
		delayCb := func(err error, isEarlyClose bool) error {
			if isEarlyClose {
				waitForBodyRead <- false
				<-eofc // will be closed by deferred call at the end of the function
				return nil
			}

			isEOF := err == io.EOF
			waitForBodyRead <- isEOF
			if isEOF {
				<-eofc // see comment above eofc declaration
			} else if err != nil {
				if cerr := pc.canceled(); cerr != nil {
					return cerr
				}
			}
			return err
		}

		readDelayResponse.WrapReader(delayReaderCallbackWrapper(delayBody, delayCb, delayReadTimeout))

		ctx := rc.req.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		select {
		case rc.ch <- responseAndError{res: resp}:
		case <-rc.callerGone:
			return
		}

		// Before looping back to the top of this function and peeking on
		// the bufio.Reader, wait for the caller goroutine to finish
		// reading the response body. (or for cancelation or death)
		select {
		case bodyEOF := <-waitForBodyRead:
			pc.t.setReqCanceler(rc.req, nil) // before pc might return to idle pool
			alive = alive &&
				bodyEOF &&
				!pc.sawEOF &&
				pc.wroteRequest() &&
				tryPutIdleConn()

			if bodyEOF {
				eofc <- struct{}{}
			}
		case <-ctx.Done():
			alive = false
			pc.t.cancelRequest(rc.req, ctx.Err())
		case <-pc.closech:
			alive = false
		}
	}
}

func (pc *persistConn) readLoopPeekFailLocked(peekErr error) {
	if pc.closed != nil {
		return
	}
	if n := pc.br.Buffered(); n > 0 {
		buf, _ := pc.br.Peek(n)
		xlog.Errorf("Unsolicited response received on idle channel starting with %q; err=%v", buf, peekErr)
	}
	if peekErr == io.EOF {
		// common case.
		pc.closeLocked(ErrServerClosedConn)
	} else {
		pc.closeLocked(fmt.Errorf("readLoopPeekFailLocked: %v", peekErr))
	}
}

func (pc *persistConn) writeLoop() {
	bw := pc.bw
	rcc := &RequestConn{
		BR: pc.br,
		BW: pc.bw,

		ConnInfo:   pc.connInfo,
		Conn:       pc.connState.Conn,
		TlsState:   pc.connState.TlsState,
		ConnClosed: pc.closech,

		ProxyUrl: pc.proxyURL,
	}

	defer func() {
		close(pc.writeLoopDone)
		pc.bw = nil
	}()

	for {
		select {
		case wr := <-pc.writech:
			err := pc.t.WriteRequest(bw, wr.req.req, rcc)
			if err == nil {
				err = pc.bw.Flush()
			} else {
				wr.req.setError(err)
			}
			pc.writeErrCh <- err // to the body reader, which might recycle us
			wr.ch <- err         // to the roundTrip function
			if err != nil {
				pc.close(err)
				return
			}
		case <-pc.closech:
			return
		}
	}
}

// maxWriteWaitBeforeConnReuse is how long the a Transport RoundTrip
// will wait to see the Request's Body.Write result after getting a
// response from the server. See comments in (*persistConn).wroteRequest.
const maxWriteWaitBeforeConnReuse = 50 * time.Millisecond

// wroteRequest is a check before recycling a connection that the previous write
// (from writeLoop above) happened and was successful.
func (pc *persistConn) wroteRequest() bool {
	select {
	case err := <-pc.writeErrCh:
		// Common case: the write happened well before the response, so
		// avoid creating a timer.
		return err == nil
	default:
		// Rare case: the request was written in writeLoop above but
		// before it could send to pc.writeErrCh, the reader read it
		// all, processed it, and called us here. In this case, give the
		// write goroutine a bit of time to finish its send.
		//
		// Less rare case: We also get here in the legitimate case of
		// Issue 7569, where the writer is still writing (or stalled),
		// but the server has already replied. In this case, we don't
		// want to wait too long, and we want to return false so this
		// connection isn't re-used.
		select {
		case err := <-pc.writeErrCh:
			return err == nil
		case <-time.After(maxWriteWaitBeforeConnReuse):
			return false
		}
	}
}

// markReused marks this connection as having been successfully used for a
// request and response.
func (pc *persistConn) markReused() {
	pc.mu.Lock()
	pc.reused = true
	pc.mu.Unlock()
}

// close closes the underlying TCP connection and closes
// the pc.closech channel.
//
// The provided err is only for testing and debugging; in normal
// circumstances it should never be seen by users.
func (pc *persistConn) close(err error) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.closeLocked(err)
}

func (pc *persistConn) closeLocked(err error) {
	if err == nil {
		panic("nil error")
	}
	pc.broken = true
	if pc.closed == nil {
		pc.closed = err
		pc.conn.Close()
		close(pc.closech)
	}
}

// responseAndError is how the goroutine reading from an HTTP/1 server
// communicates with the goroutine doing the RoundTrip.
type responseAndError struct {
	res Response // else use this response (see res method)
	err error
}

type requestAndChan struct {
	req        Request
	connInfo   *ConnInfo
	ch         chan responseAndError // unbuffered; always send in select on callerGone
	callerGone <-chan struct{}       // closed when roundTrip caller has returned
}

// transportRequest is a wrapper around a *Request that adds
// optional extra headers to write and stores any error to return
// from roundTrip.
type transportRequest struct {
	req      Request // original request, not to be mutated
	connInfo *ConnInfo

	mu  sync.Mutex // guards err
	err error      // first setError value for mapRoundTripError to consider
}

func (tr *transportRequest) setError(err error) {
	tr.mu.Lock()
	if tr.err == nil {
		tr.err = err
	}
	tr.mu.Unlock()
}

// A writeRequest is sent by the readLoop's goroutine to the
// writeLoop's goroutine to write a request while the read loop
// concurrently waits on both the write response and the server's
// reply.
type writeRequest struct {
	req *transportRequest
	ch  chan<- error
}

// clneTLSConfig returns a shallow clone of cfg, or a new zero tls.Config if
// cfg is nil. This is safe to call even if cfg is in active use by a TLS
// client or server.
func cloneTLSConfig(cfg *tls.Config) *tls.Config {
	if cfg == nil {
		return &tls.Config{}
	}
	return cfg.Clone()
}

// error values for debugging and testing, not seen by users.
var (
	errKeepAlivesDisabled  = errors.New("putIdleConn: keep alives disabled")
	errConnBroken          = errors.New("putIdleConn: connection is in bad state")
	errWantIdle            = errors.New("putIdleConn: CloseIdleConnections was called")
	errTooManyIdle         = errors.New("putIdleConn: too many idle connections")
	errTooManyIdleHost     = errors.New("putIdleConn: too many idle connections for host")
	errCloseIdleConns      = errors.New("CloseIdleConnections called")
	errReadLoopExiting     = errors.New("persistConn.readLoop exiting")
	errIdleConnTimeout     = errors.New("idle connection timeout")
	errReadOnClosedResBody = errors.New("read on closed response body")
	errTlsHandshakeTimeout = errors.New("TLS handshake timeout")
	errReadTimeout         = errors.New("read response timeout")
	errWriteTimeout        = errors.New("write request timeout")
	errCloseOnNoResponse   = errors.New("close conn on no response")
)

var ErrServerClosedConn = errors.New("server closed connection")
var ErrGetEndpointFail = errors.New("get endpoint fail")
var ErrRequestCanceled = errors.New("request canceled")
var ErrRequestCanceledConn = errors.New("request canceled while waiting for connection")

/****************************************/

type TransportGetEnpointError struct {
	Err error
}

func (e *TransportGetEnpointError) Error() string { return e.Err.Error() }

func (e *TransportGetEnpointError) Timeout() bool   { return false }
func (e *TransportGetEnpointError) Temporary() bool { return false }

func (e *TransportGetEnpointError) Unwrap() error {
	return e.Err
}

/****************************************/

type TransportConnError struct {
	Err error

	timeout   bool
	temporary bool
}

func (e *TransportConnError) Error() string { return e.Err.Error() }

func (e *TransportConnError) Timeout() bool   { return e.timeout }
func (e *TransportConnError) Temporary() bool { return e.temporary }

func (e *TransportConnError) Unwrap() error {
	return e.Err
}

func newTransportConnError(e error) error {

	n := &TransportConnError{
		Err: e,
	}
	if e, ok := e.(net.Error); ok {
		n.timeout = e.Timeout()
		n.temporary = e.Temporary()
	}
	return n
}

/****************************************/

type TransportWriteError struct {
	Err        error
	WriteBytes int64

	timeout   bool
	temporary bool
}

func (e *TransportWriteError) Error() string { return e.Err.Error() }

func (e *TransportWriteError) Timeout() bool   { return e.timeout }
func (e *TransportWriteError) Temporary() bool { return e.temporary }

func (e *TransportWriteError) Unwrap() error {
	return e.Err
}

func newTransportWriteError(e error, timeout bool, writeBytes int64) error {

	n := &TransportWriteError{
		Err:        e,
		WriteBytes: writeBytes,

		timeout: timeout,
	}
	if e, ok := e.(net.Error); ok {
		n.timeout = e.Timeout()
		n.temporary = e.Temporary()
	}
	return n
}

/****************************************/

type TransportReadError struct {
	Err error

	timeout   bool
	temporary bool
}

func (e *TransportReadError) Error() string { return e.Err.Error() }

func (e *TransportReadError) Timeout() bool   { return e.timeout }
func (e *TransportReadError) Temporary() bool { return e.temporary }

func (e *TransportReadError) Unwrap() error {
	return e.Err
}

func newTransportReadError(e error, timeout bool) error {

	n := &TransportReadError{
		Err:     e,
		timeout: timeout,
	}
	if e, ok := e.(net.Error); ok {
		n.timeout = e.Timeout()
		n.temporary = e.Temporary()
	}
	return n
}

/****************************************/

type TransportError struct {
	Err            error
	NothingWritten bool

	timeout   bool
	temporary bool
}

func (e *TransportError) Error() string { return e.Err.Error() }

func (e *TransportError) Timeout() bool   { return e.timeout }
func (e *TransportError) Temporary() bool { return e.temporary }

func (e *TransportError) Unwrap() error {
	return e.Err
}

func newTransportError(e error, notWrite bool) error {

	n := &TransportError{
		Err:            e,
		NothingWritten: notWrite,
	}
	if e, ok := e.(net.Error); ok {
		n.timeout = e.Timeout()
		n.temporary = e.Temporary()
	}
	return n
}

/****************************************/

// connsPerHostClosedCh is a closed channel used by MaxConnsPerHost
// for the property that receives from a closed channel return the
// zero value.
var connsPerHostClosedCh = make(chan struct{})

func init() {
	close(connsPerHostClosedCh)
}

func ErrIsTimeout(e error) bool {
	if e, ok := e.(net.Error); ok {
		return e.Timeout()
	}
	return false
}
