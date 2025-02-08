// common rpc interface.
package xrpc

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ccxp/xgo/xlog"
	"github.com/ccxp/xgo/xnet"
	"github.com/ccxp/xgo/xutil/xctx"
)

type Request interface {
	WithContext(context.Context) Request
	Context() context.Context
}

type Response interface {
}

// 读数据处理
type ReadRequestFunc func(ctx context.Context, conn net.Conn, br *bufio.Reader, bw *bufio.Writer, connState *xnet.ConnState) (Request, error)

// 处理Request
type Handler func(Request) (Response, error)

// 写回包，如果返回error非空，则返回后连接关闭.
//
// 参数中的resp、e是handler返回的，但在Balancer拒绝的情况下，resp=nil, e=*RejectByBalancerError.
type WriteResponseFunc func(ctx context.Context, c net.Conn, bw *bufio.Writer, req Request, resp Response, e error) error

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

	// 在handler前调用一次，返回nil则决绝请求，注意使用需要自行设置返回信息.
	StateNewRequest func(Request) error

	// 在handler后调用一次
	StateEndRequest func(Request, Response, error, time.Duration, *xnet.ConnState)

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
	StateNewRequestReject func(Request, error)

	// 读取到新请求
	StateNewRequest func(Request)

	// 请求处理结束
	StateEndRequest func(Request, Response, error, time.Duration, *xnet.ConnState)

	// 连接状态变成空闲
	StateIdle func(net.Conn, *xnet.ConnState)
	// 连接关闭
	StateClosed func(net.Conn, *xnet.ConnState)

	// 带ValueCtx的版本，可自行记录一些数据
	// 读取到新请求
	StateNewRequestWithValue func(Request, *xctx.ValueCtx)

	// 请求处理结束
	StateEndRequestWithValue func(Request, Response, error, time.Duration, *xnet.ConnState, *xctx.ValueCtx)
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
		t.StateNewRequestReject = func(r Request, e error) {
			if a.StateNewRequestReject != nil {
				a.StateNewRequestReject(r, e)
			}
			if b.StateNewRequestReject != nil {
				b.StateNewRequestReject(r, e)
			}
		}
	}

	if a.StateNewRequest != nil || b.StateNewRequest != nil {
		t.StateNewRequest = func(r Request) {
			if a.StateNewRequest != nil {
				a.StateNewRequest(r)
			}
			if b.StateNewRequest != nil {
				b.StateNewRequest(r)
			}
		}
	}

	if a.StateEndRequest != nil || b.StateEndRequest != nil {
		t.StateEndRequest = func(r Request, p Response, e error, d time.Duration, s *xnet.ConnState) {
			if a.StateEndRequest != nil {
				a.StateEndRequest(r, p, e, d, s)
			}
			if b.StateEndRequest != nil {
				b.StateEndRequest(r, p, e, d, s)
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

	if a.StateNewRequestWithValue != nil || b.StateNewRequestWithValue != nil {
		t.StateNewRequestWithValue = func(r Request, ctxx *xctx.ValueCtx) {
			if a.StateNewRequestWithValue != nil {
				a.StateNewRequestWithValue(r, ctxx)
			}
			if b.StateNewRequestWithValue != nil {
				b.StateNewRequestWithValue(r, ctxx)
			}
		}
	}

	if a.StateEndRequestWithValue != nil || b.StateEndRequest != nil {
		t.StateEndRequestWithValue = func(r Request, p Response, e error, d time.Duration, s *xnet.ConnState, ctxx *xctx.ValueCtx) {
			if a.StateEndRequestWithValue != nil {
				a.StateEndRequestWithValue(r, p, e, d, s, ctxx)
			}
			if b.StateEndRequestWithValue != nil {
				b.StateEndRequestWithValue(r, p, e, d, s, ctxx)
			}
		}
	}

	return t
}

// rejrect by balancer.StateNewRequest.
type RejectByBalancerError struct {
	Err error
}

func (e *RejectByBalancerError) Error() string { return e.Err.Error() }

// err return by handler
type HandlerError struct {
	Err error
}

func (e *HandlerError) Error() string { return e.Err.Error() }

type mockConn struct {
	net.Conn
	info xnet.ConnState
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

// A conn represents the server side of an HTTP connection.
type conn struct {
	// server is the server on which the connection arrived.
	// Immutable; never nil.
	server *Server

	// cancelCtx cancels the connection-level context.
	cancelCtx context.CancelFunc

	// rwc is the underlying network connection.
	// This is never wrapped by other types and is the value given out
	// to CloseNotifier callers. It is usually of type *net.TCPConn or
	// *tls.Conn.
	rwc net.Conn
	mc  *mockConn

	// remoteAddr is rwc.RemoteAddr().String(). It is not populated synchronously
	// inside the Listener's Accept goroutine, as some implementations block.
	// It is populated immediately inside the (*conn).serve goroutine.
	// This is the value of a Handler's (*Request).RemoteAddr.
	remoteAddr string

	// tlsState is the TLS connection state when using TLS.
	// nil means not TLS.
	tlsState *tls.ConnectionState

	// werr is set to the first write error to rwc.
	// It is set via checkConnErrorWriter{w}, where bufw writes.
	werr error

	// bufr reads from r.
	bufr *bufio.Reader

	// bufw writes to checkConnErrorWriter{c}, which populates werr on error.
	bufw *bufio.Writer

	curState struct{ atomic uint64 } // packed (unixtime<<8|uint8(ConnState))

}

// Create new connection from rwc.
func (srv *Server) newConn(rwc net.Conn) *conn {
	mc := &mockConn{rwc, xnet.ConnState{Conn: rwc}}
	if tc, ok := rwc.(*tls.Conn); ok {
		mc.info.TlsState = new(tls.ConnectionState)
		*mc.info.TlsState = tc.ConnectionState()
	}
	c := &conn{
		server: srv,
		rwc:    rwc,
		mc:     mc,
	}

	return c
}

var (
	bufioReaderPool sync.Pool
	bufioWriterPool sync.Pool
)

func newBufioReader(r io.Reader) *bufio.Reader {
	if v := bufioReaderPool.Get(); v != nil {
		br := v.(*bufio.Reader)
		br.Reset(r)
		return br
	}
	// Note: if this reader size is ever changed, update
	// TestHandlerBodyClose's assumptions.
	return bufio.NewReader(r)
}

func putBufioReader(br *bufio.Reader) {
	br.Reset(nil)
	bufioReaderPool.Put(br)
}

func newBufioWriter(w io.Writer) *bufio.Writer {
	if v := bufioWriterPool.Get(); v != nil {
		bw := v.(*bufio.Writer)
		bw.Reset(w)
		return bw
	}

	return bufio.NewWriterSize(w, 1024*2)
}

func putBufioWriter(bw *bufio.Writer) {
	bw.Reset(nil)
	bufioWriterPool.Put(bw)
}

// Read next request from connection.
func (c *conn) readRequest(ctx context.Context) (req Request, err error) {

	if d := c.server.ReadTimeout; d != 0 {
		c.rwc.SetReadDeadline(time.Now().Add(d))
	}

	req, err = c.server.ReadRequest(ctx, c.rwc, c.bufr, c.bufw, &c.mc.info)

	return
}

func (c *conn) finishRequest() error {
	return c.bufw.Flush()
}

// shouldReuseConnection reports whether the underlying TCP connection can be reused.
// It must only be called after the handler is done executing.
func (c *conn) shouldReuseConnection() bool {

	// There was some error writing to the underlying connection
	// during the request, so don't re-use this conn.
	if c.werr != nil {
		return false
	}

	return true
}

func (c *conn) finalFlush() {
	if c.bufr != nil {
		// Steal the bufio.Reader (~4KB worth of memory) and its associated
		// reader for a future connection.
		putBufioReader(c.bufr)
		c.bufr = nil
	}

	if c.bufw != nil {
		c.bufw.Flush()
		// Steal the bufio.Writer (~4KB worth of memory) and its associated
		// writer for a future connection.
		putBufioWriter(c.bufw)
		c.bufw = nil
	}
}

// Close the connection.
func (c *conn) close() {
	c.finalFlush()
	c.rwc.Close()
}

// rstAvoidanceDelay is the amount of time we sleep after closing the
// write side of a TCP connection before closing the entire socket.
// By sleeping, we increase the chances that the client sees our FIN
// and processes its final data before they process the subsequent RST
// from closing a connection with known unread data.
// This RST seems to occur mostly on BSD systems. (And Windows?)
// This timeout is somewhat arbitrary (~latency around the planet).
const rstAvoidanceDelay = 20 * time.Millisecond

type closeWriter interface {
	CloseWrite() error
}

var _ closeWriter = (*net.TCPConn)(nil)

// closeWrite flushes any outstanding data and sends a FIN packet (if
// client is connected via TCP), signalling that we're done. We then
// pause for a bit, hoping the client processes it before any
// subsequent RST.
//
// See https://golang.org/issue/3595
func (c *conn) closeWriteAndWait() {
	c.finalFlush()
	if tcp, ok := c.rwc.(closeWriter); ok {
		tcp.CloseWrite()
	}
	time.Sleep(rstAvoidanceDelay)
}

func (c *conn) setState(nc net.Conn, state int) {
	srv := c.server
	switch state {
	case stateNew:
		srv.trackConn(c, true)

		if c.server.Balancer != nil && c.server.Balancer.StateNew != nil {
			c.server.Balancer.StateNew(nc)
		}
		if c.server.Tracer != nil && c.server.Tracer.StateNew != nil {
			c.server.Tracer.StateNew(nc)
		}

	case stateActive:

		if c.server.Balancer != nil && c.server.Balancer.StateActive != nil {
			c.server.Balancer.StateActive(nc)
		}

	case stateIdle:

		info := &c.mc.info

		if c.server.Balancer != nil && c.server.Balancer.StateIdle != nil {
			c.server.Balancer.StateIdle(nc, info)
		}
		if c.server.Tracer != nil && c.server.Tracer.StateIdle != nil {
			c.server.Tracer.StateIdle(nc, info)
		}
		info.Reset()

	case stateClosed:
		srv.trackConn(c, false)

		info := &c.mc.info
		if c.server.Balancer != nil && c.server.Balancer.StateClosed != nil {
			c.server.Balancer.StateClosed(nc, info)
		}
		if c.server.Tracer != nil && c.server.Tracer.StateClosed != nil {
			c.server.Tracer.StateClosed(nc, info)
		}
		info.Reset()
	}
	if state > 0xff || state < 0 {
		panic("internal error")
	}
	packedState := uint64(time.Now().Unix()<<8) | uint64(state)
	atomic.StoreUint64(&c.curState.atomic, packedState)

}

func (c *conn) getState() (state int, unixSec int64) {
	packedState := atomic.LoadUint64(&c.curState.atomic)
	return int(packedState & 0xff), int64(packedState >> 8)
}

// Serve a new connection.
func (c *conn) serve(ctx context.Context) {
	c.remoteAddr = c.rwc.RemoteAddr().String()

	defer func() {
		if err := recover(); err != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			xlog.Errorf("panic serving %v: %v\n%s", c.remoteAddr, err, buf)
		}
		c.close()
		c.setState(c.rwc, stateClosed)
	}()

	if tlsConn, ok := c.rwc.(*tls.Conn); ok {
		if d := c.server.ReadTimeout; d != 0 {
			c.rwc.SetReadDeadline(time.Now().Add(d))
		}
		if d := c.server.WriteTimeout; d != 0 {
			c.rwc.SetWriteDeadline(time.Now().Add(d))
		}
		if err := tlsConn.Handshake(); err != nil {
			// If the handshake failed due to the client not speaking
			// TLS, assume they're speaking plaintext HTTP and write a
			// 400 response on the TLS conn's underlying net.Conn.
			if _, ok := err.(tls.RecordHeaderError); ok {
				tlsConn.Close()
				return
			}
			xlog.Errorf("TLS handshake error from %s: %v", c.remoteAddr, err)
			return
		}
		c.tlsState = new(tls.ConnectionState)
		*c.tlsState = tlsConn.ConnectionState()
	}

	// HTTP/1.x from here on.

	ctx, cancelCtx := context.WithCancel(ctx)
	c.cancelCtx = cancelCtx
	defer cancelCtx()

	c.bufr = newBufioReader(c.mc)
	c.bufw = newBufioWriter(checkConnErrorWriter{c})

	for {
		ts := time.Now()

		req, err := c.readRequest(ctx)

		c.setState(c.rwc, stateActive)

		if err != nil {
			c.closeWriteAndWait()
			return
		}

		if c.server.Balancer != nil && c.server.Balancer.StateNewRequest != nil {
			err = c.server.Balancer.StateNewRequest(req)
			if err != nil {
				if c.server.Tracer != nil && c.server.Tracer.StateNewRequestReject != nil {
					c.server.Tracer.StateNewRequestReject(req, err)
				}
				err = &RejectByBalancerError{err}
			}
		}

		var resp Response
		ctxx := &xctx.ValueCtx{}
		if err == nil {
			if c.server.Tracer != nil && c.server.Tracer.StateNewRequest != nil {
				c.server.Tracer.StateNewRequest(req)
			}
			if c.server.Tracer != nil && c.server.Tracer.StateNewRequestWithValue != nil {
				c.server.Tracer.StateNewRequestWithValue(req, ctxx)
			}

			resp, err = c.server.Handler(req)
		}

		if d := c.server.WriteTimeout; d != 0 {
			c.rwc.SetWriteDeadline(time.Now().Add(d))
		}

		err = c.server.WriteResponse(ctx, c.rwc, c.bufw, req, resp, err)
		if err == nil {
			err = c.finishRequest()
		}

		d := time.Since(ts)
		if c.server.Balancer != nil && c.server.Balancer.StateEndRequest != nil {
			c.server.Balancer.StateEndRequest(req, resp, err, d, &c.mc.info)
		}
		if c.server.Tracer != nil && c.server.Tracer.StateEndRequest != nil {
			c.server.Tracer.StateEndRequest(req, resp, err, d, &c.mc.info)
		}
		if c.server.Tracer != nil && c.server.Tracer.StateEndRequestWithValue != nil {
			c.server.Tracer.StateEndRequestWithValue(req, resp, err, d, &c.mc.info, ctxx)
		}

		if err != nil {
			return
		}

		if !c.shouldReuseConnection() {
			return
		}
		c.setState(c.rwc, stateIdle)

		if !c.server.doKeepAlives() {
			// We're in shutdown mode. We might've replied
			// to the user without "Connection: close" and
			// they might think they can send another
			// request, but such is life with HTTP/1.1.
			return
		}

		if d := c.server.idleTimeout(); d != 0 {
			c.rwc.SetReadDeadline(time.Now().Add(d))
			if _, err := c.bufr.Peek(1); err != nil {
				return
			}
			c.rwc.SetReadDeadline(time.Time{})
		} else {
			c.rwc.SetReadDeadline(time.Time{})
			if _, err := c.bufr.Peek(1); err != nil {
				return
			}
		}

	}
}

// A Server defines parameters for running an RPC server.
// The zero value for Server is a valid configuration.
type Server struct {
	Addr string // TCP address to listen on

	// use br to read data.
	ReadRequest ReadRequestFunc

	Handler Handler

	WriteResponse WriteResponseFunc

	Balancer *ServerBalancer
	Tracer   *ServerTracer

	// TLSConfig optionally provides a TLS configuration for use
	// by ServeTLS and ListenAndServeTLS. Note that this value is
	// cloned by ServeTLS and ListenAndServeTLS, so it's not
	// possible to modify the configuration with methods like
	// tls.Config.SetSessionTicketKeys. To use
	// SetSessionTicketKeys, use Server.Serve with a TLS Listener
	// instead.
	TLSConfig *tls.Config

	// ReadTimeout is the maximum duration for reading the entire
	// request, including the body.
	//
	// Because ReadTimeout does not let Handlers make per-request
	// decisions on each request body's acceptable deadline or
	// upload rate, most users will prefer to use
	// ReadHeaderTimeout. It is valid to use them both.
	ReadTimeout time.Duration

	// WriteTimeout is the maximum duration before timing out
	// writes of the response. It is reset whenever a new
	// request's header is read. Like ReadTimeout, it does not
	// let Handlers make decisions on a per-request basis.
	WriteTimeout time.Duration

	// IdleTimeout is the maximum amount of time to wait for the
	// next request when keep-alives are enabled. If IdleTimeout
	// is zero, the value of ReadTimeout is used. If both are
	// zero, ReadHeaderTimeout is used.
	IdleTimeout time.Duration

	disableKeepAlives int32 // accessed atomically.
	inShutdown        int32 // accessed atomically (non-zero means we're in Shutdown)
	// result of http2.ConfigureServer if used

	mu         sync.Mutex
	listeners  map[*net.Listener]struct{}
	activeConn map[*conn]struct{}
	doneChan   chan struct{}
	onShutdown []func()
}

func (s *Server) getDoneChan() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getDoneChanLocked()
}

func (s *Server) getDoneChanLocked() chan struct{} {
	if s.doneChan == nil {
		s.doneChan = make(chan struct{})
	}
	return s.doneChan
}

func (s *Server) closeDoneChanLocked() {
	ch := s.getDoneChanLocked()
	select {
	case <-ch:
		// Already closed. Don't close again.
	default:
		// Safe to close here. We're the only closer, guarded
		// by s.mu.
		close(ch)
	}
}

// Close immediately closes all active net.Listeners and any
// connections in state StateNew, StateActive, or StateIdle. For a
// graceful shutdown, use Shutdown.
//
// Close does not attempt to close (and does not even know about)
// any hijacked connections, such as WebSockets.
//
// Close returns any error returned from closing the Server's
// underlying Listener(s).
func (srv *Server) Close() error {
	atomic.StoreInt32(&srv.inShutdown, 1)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.closeDoneChanLocked()
	err := srv.closeListenersLocked()
	for c := range srv.activeConn {
		c.rwc.Close()
		delete(srv.activeConn, c)
	}
	return err
}

// shutdownPollInterval is how often we poll for quiescence
// during Server.Shutdown. This is lower during tests, to
// speed up tests.
// Ideally we could find a solution that doesn't involve polling,
// but which also doesn't have a high runtime cost (and doesn't
// involve any contentious mutexes), but that is left as an
// exercise for the reader.
var shutdownPollInterval = 500 * time.Millisecond

// Shutdown gracefully shuts down the server without interrupting any
// active connections. Shutdown works by first closing all open
// listeners, then closing all idle connections, and then waiting
// indefinitely for connections to return to idle and then shut down.
// If the provided context expires before the shutdown is complete,
// Shutdown returns the context's error, otherwise it returns any
// error returned from closing the Server's underlying Listener(s).
//
// When Shutdown is called, Serve, ListenAndServe, and
// ListenAndServeTLS immediately return ErrServerClosed. Make sure the
// program doesn't exit and waits instead for Shutdown to return.
//
// Shutdown does not attempt to close nor wait for hijacked
// connections such as WebSockets. The caller of Shutdown should
// separately notify such long-lived connections of shutdown and wait
// for them to close, if desired. See RegisterOnShutdown for a way to
// register shutdown notification functions.
//
// Once Shutdown has been called on a server, it may not be reused;
// future calls to methods such as Serve will return ErrServerClosed.
func (srv *Server) Shutdown(ctx context.Context) error {
	atomic.StoreInt32(&srv.inShutdown, 1)

	srv.mu.Lock()
	lnerr := srv.closeListenersLocked()
	srv.closeDoneChanLocked()
	for _, f := range srv.onShutdown {
		go f()
	}
	srv.mu.Unlock()

	ticker := time.NewTicker(shutdownPollInterval)
	defer ticker.Stop()
	for {
		if srv.closeIdleConns() {
			return lnerr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// RegisterOnShutdown registers a function to call on Shutdown.
// This can be used to gracefully shutdown connections that have
// undergone NPN/ALPN protocol upgrade or that have been hijacked.
// This function should start protocol-specific graceful shutdown,
// but should not wait for shutdown to complete.
func (srv *Server) RegisterOnShutdown(f func()) {
	srv.mu.Lock()
	srv.onShutdown = append(srv.onShutdown, f)
	srv.mu.Unlock()
}

// closeIdleConns closes all idle connections and reports whether the
// server is quiescent.
func (s *Server) closeIdleConns() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	quiescent := true
	for c := range s.activeConn {
		st, unixSec := c.getState()
		// Issue 22682: treat StateNew connections as if
		// they're idle if we haven't read the first request's
		// header in over 5 seconds.
		if st == stateNew && unixSec < time.Now().Unix()-5 {
			st = stateIdle
		}
		if st != stateIdle || unixSec == 0 {
			// Assume unixSec == 0 means it's a very new
			// connection, without state set yet.
			quiescent = false
			continue
		}
		c.rwc.Close()
		delete(s.activeConn, c)
	}
	return quiescent
}

func (s *Server) closeListenersLocked() error {
	var err error
	for ln := range s.listeners {
		if cerr := (*ln).Close(); cerr != nil && err == nil {
			err = cerr
		}
		delete(s.listeners, ln)
	}
	return err
}

const (
	// StateNew represents a new connection that is expected to
	// send a request immediately. Connections begin at this
	// state and then transition to either StateActive or
	// StateClosed.
	stateNew int = iota

	// StateActive represents a connection that has read 1 or more
	// bytes of a request. The Server.ConnState hook for
	// StateActive fires before the request has entered a handler
	// and doesn't fire again until the request has been
	// handled. After the request is handled, the state
	// transitions to StateClosed, StateHijacked, or StateIdle.
	// For HTTP/2, StateActive fires on the transition from zero
	// to one active request, and only transitions away once all
	// active requests are complete. That means that ConnState
	// cannot be used to do per-request work; ConnState only notes
	// the overall state of the connection.
	stateActive

	// StateIdle represents a connection that has finished
	// handling a request and is in the keep-alive state, waiting
	// for a new request. Connections transition from StateIdle
	// to either StateActive or StateClosed.
	stateIdle

	// StateClosed represents a closed connection.
	// This is a terminal state. Hijacked connections do not
	// transition to StateClosed.
	stateClosed
)

// ListenAndServe listens on the TCP network address srv.Addr and then
// calls Serve to handle requests on incoming connections.
// Accepted connections are configured to enable TCP keep-alives.
//
// If srv.Addr is blank, ":http" is used.
//
// ListenAndServe always returns a non-nil error. After Shutdown or Close,
// the returned error is ErrServerClosed.
func (srv *Server) ListenAndServe() error {
	if srv.shuttingDown() {
		return ErrServerClosed
	}
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

// ErrServerClosed is returned by the Server's Serve, ServeTLS, ListenAndServe,
// and ListenAndServeTLS methods after a call to Shutdown or Close.
var ErrServerClosed = errors.New("http: Server closed")

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

	l = &onceCloseListener{Listener: l}
	defer l.Close()

	if !srv.trackListener(&l, true) {
		return ErrServerClosed
	}
	defer srv.trackListener(&l, false)

	var tempDelay time.Duration // how long to sleep on accept failure
	ctx := context.Background() // base is always background, per Issue 16220
	for {

		if srv.Balancer != nil && srv.Balancer.AcceptPrepare != nil {
			srv.Balancer.AcceptPrepare()
		}
		rw, e := l.Accept()
		if e != nil {
			select {
			case <-srv.getDoneChan():
				return ErrServerClosed
			default:
			}
			if ne, ok := e.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				xlog.Errorf("http: Accept error: %v; retrying in %v", e, tempDelay)
				time.Sleep(tempDelay)
				continue
			}
			return e
		}

		if srv.Balancer != nil && srv.Balancer.Accept != nil {
			e = srv.Balancer.Accept(rw)
			if e != nil {
				if srv.Tracer != nil && srv.Tracer.StateAcceptReject != nil {
					srv.Tracer.StateAcceptReject(rw, e)
				}

				rw.Close()
				continue
			}
		}
		tempDelay = 0
		c := srv.newConn(rw)
		c.setState(c.rwc, stateNew) // before Serve can return
		go c.serve(ctx)
	}
}

func cloneTLSConfig(cfg *tls.Config) *tls.Config {
	if cfg == nil {
		return &tls.Config{}
	}
	return cfg.Clone()
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

	config := cloneTLSConfig(srv.TLSConfig)

	configHasCert := len(config.Certificates) > 0 || config.GetCertificate != nil
	if !configHasCert || certFile != "" || keyFile != "" {
		var err error
		config.Certificates = make([]tls.Certificate, 1)
		config.Certificates[0], err = tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return err
		}
	}

	tlsListener := tls.NewListener(l, config)
	return srv.Serve(tlsListener)
}

// trackListener adds or removes a net.Listener to the set of tracked
// listeners.
//
// We store a pointer to interface in the map set, in case the
// net.Listener is not comparable. This is safe because we only call
// trackListener via Serve and can track+defer untrack the same
// pointer to local variable there. We never need to compare a
// Listener from another caller.
//
// It reports whether the server is still up (not Shutdown or Closed).
func (s *Server) trackListener(ln *net.Listener, add bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listeners == nil {
		s.listeners = make(map[*net.Listener]struct{})
	}
	if add {
		if s.shuttingDown() {
			return false
		}
		s.listeners[ln] = struct{}{}
	} else {
		delete(s.listeners, ln)
	}
	return true
}

func (s *Server) trackConn(c *conn, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeConn == nil {
		s.activeConn = make(map[*conn]struct{})
	}
	if add {
		s.activeConn[c] = struct{}{}
	} else {
		delete(s.activeConn, c)
	}
}

func (s *Server) idleTimeout() time.Duration {
	if s.IdleTimeout != 0 {
		return s.IdleTimeout
	}
	return s.ReadTimeout
}

func (s *Server) doKeepAlives() bool {
	return atomic.LoadInt32(&s.disableKeepAlives) == 0 && !s.shuttingDown()
}

func (s *Server) shuttingDown() bool {
	// TODO: replace inShutdown with the existing atomicBool type;
	// see https://github.com/golang/go/issues/20239#issuecomment-381434582
	return atomic.LoadInt32(&s.inShutdown) != 0
}

// SetKeepAlivesEnabled controls whether HTTP keep-alives are enabled.
// By default, keep-alives are always enabled. Only very
// resource-constrained environments or servers in the process of
// shutting down should disable them.
func (srv *Server) SetKeepAlivesEnabled(v bool) {
	if v {
		atomic.StoreInt32(&srv.disableKeepAlives, 0)
		return
	}
	atomic.StoreInt32(&srv.disableKeepAlives, 1)

	// Close idle HTTP/1 conns:
	srv.closeIdleConns()

	// TODO: Issue 26303: close HTTP/2 conns as soon as they become idle.
}

func (srv *Server) IsKeepAlivesEnabled() bool {
	return srv.doKeepAlives()
}

// ListenAndServeTLS listens on the TCP network address srv.Addr and
// then calls ServeTLS to handle requests on incoming TLS connections.
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
// ListenAndServeTLS always returns a non-nil error. After Shutdown or
// Close, the returned error is ErrServerClosed.
func (srv *Server) ListenAndServeTLS(certFile, keyFile string) error {
	if srv.shuttingDown() {
		return ErrServerClosed
	}
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

// onceCloseListener wraps a net.Listener, protecting it from
// multiple Close calls.
type onceCloseListener struct {
	net.Listener
	once     sync.Once
	closeErr error
}

func (oc *onceCloseListener) Close() error {
	oc.once.Do(oc.close)
	return oc.closeErr
}

func (oc *onceCloseListener) close() { oc.closeErr = oc.Listener.Close() }

// checkConnErrorWriter writes to c.rwc and records any write errors to c.werr.
// It only contains one field (and a pointer field at that), so it
// fits in an interface value without an extra allocation.
type checkConnErrorWriter struct {
	c *conn
}

func (w checkConnErrorWriter) Write(p []byte) (n int, err error) {
	n, err = w.c.mc.Write(p)
	if err != nil && w.c.werr == nil {
		w.c.werr = err
		w.c.cancelCtx()
	}
	return
}

// 一个xrpc.Server需要的所有自定义处理函数
type ServerFunc struct {
	ReadRequest   ReadRequestFunc
	Handler       Handler
	WriteResponse WriteResponseFunc
}
