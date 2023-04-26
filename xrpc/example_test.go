package xrpc_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/ccxp/xgo/xnet"
	"github.com/ccxp/xgo/xrpc"
)

const helloCmd int = 1

const (
	statusSucc              int32 = 0
	statusBadCmdId                = -101
	statusReadReqError            = -102
	statusUnmarshalReqError       = -103
	statusHandlerError            = -104
)

var headerLenNotMatchError = errors.New("header length not match")

// user req, resp and handler
type helloReq struct {
	Str string
}

type helloResp struct {
	Str string
}

func handleHello(req *helloReq) (resp *helloResp, code int32) {
	resp = &helloResp{req.Str}
	return
}

func writeUint8(v uint8, tmp []byte, offset int) int {
	tmp[offset] = byte(v)
	return offset + 1
}

func writeUint32(v uint32, tmp []byte, offset int) int {
	tmp[offset] = byte(v >> 24)
	tmp[1+offset] = byte(v >> 16)
	tmp[2+offset] = byte(v >> 8)
	tmp[3+offset] = byte(v)
	return offset + 4
}

func writeInt32(v int32, tmp []byte, offset int) int {
	tmp[offset] = byte(v >> 24)
	tmp[1+offset] = byte(v >> 16)
	tmp[2+offset] = byte(v >> 8)
	tmp[3+offset] = byte(v)
	return offset + 4
}

func readUint8(tmp []byte, offset int) (uint8, int) {
	return uint8(tmp[offset]), offset + 1
}

func readUint32(tmp []byte, offset int) (uint32, int) {

	v := uint32(tmp[offset]) << 24
	v |= uint32(tmp[1+offset]) << 16
	v |= uint32(tmp[2+offset]) << 8
	v |= uint32(tmp[3+offset])

	return v, offset + 4
}

func readInt32(tmp []byte, offset int) (int32, int) {

	v := int32(tmp[offset]) << 24
	v |= int32(tmp[1+offset]) << 16
	v |= int32(tmp[2+offset]) << 8
	v |= int32(tmp[3+offset])

	return v, offset + 4
}

func readAll(r io.Reader, buf []byte, size int) (int, error) {
	s := buf[0:len(buf)]
	toread := size
	n := 0
	var err error
	for toread > 0 {
		n, err = r.Read(s)
		if err != nil {
			return size - toread + n, err
		}
		s = s[n:len(s)]
		toread -= n
	}

	return size - toread, nil
}

const headerLen = 10

type header struct {
	HeaderLen  uint8
	CmdId      uint8
	BodyLen    uint32
	StatusCode int32
}

func (h *header) Write(w io.Writer) (int, error) {

	h.HeaderLen = uint8(headerLen)

	buf := make([]byte, headerLen)
	offset := 0
	offset = writeUint8(h.HeaderLen, buf, offset)
	offset = writeUint8(h.CmdId, buf, offset)
	offset = writeUint32(h.BodyLen, buf, offset)
	offset = writeInt32(h.StatusCode, buf, offset)

	return w.Write(buf)
}

func (h *header) Read(r io.Reader) error {

	buf := make([]byte, headerLen)
	_, err := readAll(r, buf, headerLen)
	if err != nil {
		return err
	}

	offset := 0
	h.HeaderLen, offset = readUint8(buf, offset)
	h.CmdId, offset = readUint8(buf, offset)
	h.BodyLen, offset = readUint32(buf, offset)
	h.StatusCode, offset = readInt32(buf, offset)

	return nil
}

// server req, resp and handler
type request struct {
	Header header

	ctx context.Context

	ReqBuffer []byte
}

func (r *request) WithContext(ctx context.Context) xrpc.Request {
	r.ctx = ctx
	return r
}

func (r *request) Context() context.Context {
	if r.ctx == nil {
		return context.Background()
	}
	return r.ctx
}

type response struct {
	Header header
	resp   interface{}
}

func myHandler(r xrpc.Request) (xrpc.Response, error) {
	req := r.(*request)
	resp := &response{}
	resp.Header.CmdId = req.Header.CmdId
	resp.Header.StatusCode = statusBadCmdId

	var unmarshalErr error

	switch int(req.Header.CmdId) {
	case helloCmd:
		rr := &helloReq{}
		unmarshalErr = json.Unmarshal(req.ReqBuffer, rr)
		if unmarshalErr == nil {
			resp.resp, resp.Header.StatusCode = handleHello(rr)
		}
	default:
		resp.Header.StatusCode = statusBadCmdId
	}

	if unmarshalErr != nil {
		resp.Header.StatusCode = statusUnmarshalReqError
	}
	return resp, nil
}

func myServerReadRequest(ctx context.Context, conn net.Conn, br *bufio.Reader, bw *bufio.Writer,
	connState *xnet.ConnState) (xrpc.Request, error) {
	_, e := br.Peek(1)
	if e != nil {
		return nil, e
	}

	req := &request{}
	e = req.Header.Read(br)
	if e != nil {
		return nil, e
	}

	fmt.Fprintf(os.Stderr, "myServerReadRequest header %v\n", req.Header)

	if req.Header.HeaderLen != headerLen {
		return nil, headerLenNotMatchError
	}

	buflen := int(req.Header.BodyLen)
	req.ReqBuffer = make([]byte, buflen)
	_, e = readAll(br, req.ReqBuffer, buflen)
	if e != nil {
		return nil, e
	}

	return req, e
}

func myServerWriteResponse(ctx context.Context, c net.Conn, bw *bufio.Writer, req xrpc.Request, resp xrpc.Response, e error) error {
	if e != nil { // in this example, e is always nil.
		h := header{StatusCode: statusHandlerError}
		_, e = h.Write(bw)
		return e
	}

	rresp := resp.(*response)
	var body []byte
	if rresp.Header.StatusCode == statusSucc {
		body, e = json.Marshal(rresp.resp)
		if e != nil {
			rresp.Header.StatusCode = statusHandlerError
		}
	}
	rresp.Header.BodyLen = uint32(len(body))

	_, e = rresp.Header.Write(bw)
	if e != nil {
		return e
	}

	if len(body) > 0 {
		_, e = bw.Write(body)
	}
	return e
}

// -------------------- Server Balancer

type mySvrBalancer struct {
	MaxConn    int32
	MaxRequest int32

	conn    int32
	request int32
}

func (b *mySvrBalancer) Accept(net.Conn) error {
	fmt.Fprintf(os.Stderr, "Balancer Accept\n")
	if atomic.LoadInt32(&b.conn) > b.MaxConn {
		return errExceedMaxConn
	}
	return nil
}

func (b *mySvrBalancer) StateNew(net.Conn) {
	fmt.Fprintf(os.Stderr, "Balancer StateNew\n")
	atomic.AddInt32(&b.conn, 1)
}

func (b *mySvrBalancer) StateActive(net.Conn) {
	fmt.Fprintf(os.Stderr, "Balancer StateActive\n")
}

func (b *mySvrBalancer) StateNewRequest(req xrpc.Request) error {
	rreq := req.(*request)
	fmt.Fprintf(os.Stderr, "Balancer StateNewRequest %v\n", rreq.Header)

	if atomic.AddInt32(&b.request, 1) > b.MaxRequest {
		return errExceedMaxRequest
	}

	return nil
}

func (b *mySvrBalancer) StateEndRequest(req xrpc.Request, resp xrpc.Response, e error, d time.Duration, state *xnet.ConnState) {
	rreq := req.(*request)
	if resp != nil {
		rresp := resp.(*response)
		fmt.Fprintf(os.Stderr, "Balancer StateEndRequest %v %v %v %v %v\n", rreq.Header, rresp.Header, e, d, state)
	} else {
		fmt.Fprintf(os.Stderr, "Balancer StateEndRequest %v %v %v %v\n", rreq.Header, e, d, state)
	}
	atomic.AddInt32(&b.request, -1)
}

func (b *mySvrBalancer) StateIdle(c net.Conn, state *xnet.ConnState) {
	fmt.Fprintf(os.Stderr, "Balancer StateIdle %v\n", state)

}

func (b *mySvrBalancer) StateClosed(c net.Conn, state *xnet.ConnState) {
	fmt.Fprintf(os.Stderr, "Balancer StateClosed %v\n", state)
	atomic.AddInt32(&b.conn, -1)
}

// -------------------- Tracer

type mySvrTracer struct {
}

func (b *mySvrTracer) StateAcceptReject(conn net.Conn, err error) {
	fmt.Fprintf(os.Stderr, "Tracer StateAcceptReject %v\n", err)
}

func (b *mySvrTracer) StateNew(net.Conn) {
	fmt.Fprintf(os.Stderr, "Tracer StateNew\n")
}

func (b *mySvrTracer) StateNewRequestReject(req xrpc.Request, e error) {
	rreq := req.(*request)
	fmt.Fprintf(os.Stderr, "Tracer StateNewRequestReject %v %v\n", rreq.Header, e)
}

func (b *mySvrTracer) StateNewRequest(req xrpc.Request) {
	rreq := req.(*request)
	fmt.Fprintf(os.Stderr, "Balancer StateNewRequest %v\n", rreq.Header)
}

func (b *mySvrTracer) StateEndRequest(req xrpc.Request, resp xrpc.Response, e error, d time.Duration, state *xnet.ConnState) {
	rreq := req.(*request)
	if resp != nil {
		rresp := resp.(*response)
		fmt.Fprintf(os.Stderr, "Balancer StateEndRequest %v %v %v %v %v\n", rreq.Header, rresp.Header, e, d, state)
	} else {
		fmt.Fprintf(os.Stderr, "Balancer StateEndRequest %v %v %v %v\n", rreq.Header, e, d, state)
	}
}

func (b *mySvrTracer) StateClosed(conn net.Conn, state *xnet.ConnState) {
	fmt.Fprintf(os.Stderr, "Tracer StateClosed %v\n", state)
}

func (b *mySvrTracer) StateIdle(conn net.Conn, state *xnet.ConnState) {
	fmt.Fprintf(os.Stderr, "Tracer StateIdle %v\n", state)
}

func ExampleServer_balancerAndTracer() {

	// init balance and tracer, optional
	balance := &mySvrBalancer{
		MaxConn:    150,
		MaxRequest: 100,
	}
	tracer := &mySvrTracer{}

	svrBalancer := &xrpc.ServerBalancer{
		Accept:          balance.Accept,
		StateNew:        balance.StateNew,
		StateActive:     balance.StateActive,
		StateNewRequest: balance.StateNewRequest,
		StateEndRequest: balance.StateEndRequest,
		StateIdle:       balance.StateIdle,
		StateClosed:     balance.StateClosed,
	}
	svrTracer := &xrpc.ServerTracer{
		StateAcceptReject: tracer.StateAcceptReject,
		StateNew:          tracer.StateNew,

		StateNewRequestReject: tracer.StateNewRequestReject,
		StateNewRequest:       tracer.StateNewRequest,
		StateEndRequest:       tracer.StateEndRequest,

		StateIdle:   tracer.StateIdle,
		StateClosed: tracer.StateClosed,
	}

	svr := &xrpc.Server{
		Addr:          ":11111",
		ReadRequest:   myServerReadRequest,
		Handler:       myHandler,
		WriteResponse: myServerWriteResponse,

		Balancer: svrBalancer, // optional
		Tracer:   svrTracer,   // optional

		ReadTimeout:  time.Second,
		WriteTimeout: time.Second,
		IdleTimeout:  60 * time.Second,
	}

	fmt.Fprintf(os.Stderr, "begin\n")
	log.Fatal(svr.ListenAndServe())

	fmt.Printf("end\n")
	// Output: end
}

var errExceedMaxConn = fmt.Errorf("errExceedMaxConn")
var errExceedMaxRequest = fmt.Errorf("errExceedMaxRequest")
var errUnauthorized = fmt.Errorf("errUnauthorized")

func ExampleServer() {

	svr := &xrpc.Server{
		Addr:          ":11111",
		ReadRequest:   myServerReadRequest,
		Handler:       myHandler,
		WriteResponse: myServerWriteResponse,

		ReadTimeout:  time.Second,
		WriteTimeout: time.Second,
		IdleTimeout:  60 * time.Second,
	}

	fmt.Fprintf(os.Stderr, "begin\n")
	log.Fatal(svr.ListenAndServe())

	fmt.Printf("end\n")
	// Output: end
}

// server req, resp and handler
type clientRequest struct {
	Header header

	ctx context.Context

	req     interface{}
	resp    interface{}
	written bool
}

func (r *clientRequest) Context() context.Context {
	if r.ctx == nil {
		return context.Background()
	}
	return r.ctx
}

func (r *clientRequest) WithContext(ctx context.Context) xrpc.Request {
	r.ctx = ctx
	return r
}

func (r *clientRequest) Close() {
	return
}

// for transport, optional
func (r *clientRequest) IsClose() bool {
	fmt.Fprintf(os.Stderr, "request IsClose\n")
	return false
}

// for transport, optional
func (r *clientRequest) IsReplayable(err error) bool {
	fmt.Fprintf(os.Stderr, "request IsReplayable %v\n", err)

	if !r.written {
		return true
	}

	if err != nil {
		switch e := err.(type) {
		case *xnet.TransportGetEnpointError:
			return false
		case *xnet.TransportConnError:
			return true
		case *xnet.TransportWriteError:
			return e.WriteBytes == 0
		case *xnet.TransportError:
			return e.NothingWritten
		}
	}

	return false
}

// for transport, optional
func (r *clientRequest) Reset() error {
	fmt.Fprintf(os.Stderr, "request Reset\n")
	return nil
}

type clientResponse struct {
	Header header
}

// for transport, optional
func (r *clientResponse) IsClose() bool {
	fmt.Fprintf(os.Stderr, "response IsClose\n")
	return false
}

// for transport, optional
func (r *clientResponse) IsReadDelay() bool {
	fmt.Fprintf(os.Stderr, "response IsReadDelay\n")
	return false
}

// for transport, optional
func (r *clientResponse) GetReader() io.ReadCloser {
	fmt.Fprintf(os.Stderr, "response GetReader\n")
	return nil
}

// for transport, optional
func (r *clientResponse) WrapReader(newR io.ReadCloser) {
	fmt.Fprintf(os.Stderr, "response WrapReader\n")
}

type myCliBalancer struct {
}

// Get addr info.
func (b *myCliBalancer) GetEndpoint(ctx context.Context, req xnet.Request, addrTried []string) (connInfo xnet.ConnInfo, err error) {

	addr := "10.123.102.140:11111"
	if len(addrTried) > 0 && addrTried[0] == addr {
		fmt.Fprintf(os.Stderr, "no more endpoint\n")
		return xnet.ConnInfo{}, xnet.ErrGetEndpointFail
	}

	return xnet.ConnInfo{
		Network: "tcp",
		Addr:    addr,
	}, nil

}

func printTransportError(err error) {
	if err == nil {
		return
	} else if err == xnet.ErrRequestCanceled || err == xnet.ErrRequestCanceledConn {
		fmt.Fprintf(os.Stderr, "\tErrRequestCanceled: %v\n", err)
	} else {
		switch e := err.(type) {
		case *xnet.TransportGetEnpointError:
			fmt.Fprintf(os.Stderr, "\tTransportGetEnpointError: %v\n", e)
		case *xnet.TransportConnError:
			fmt.Fprintf(os.Stderr, "\tTransportConnError: %v\n", e)
		case *xnet.TransportWriteError:
			fmt.Fprintf(os.Stderr, "\tTransportWriteError: %v\n", e)
		case *xnet.TransportReadError:
			fmt.Fprintf(os.Stderr, "\tTransportReadError: %v\n", e)
		case *xnet.TransportError:
			fmt.Fprintf(os.Stderr, "\tTransportError: %v\n", e)
		default:
			fmt.Fprintf(os.Stderr, "\tother error: %v\n", err)
		}
	}
}

// optional
type myCliTracer struct {
}

// optional
func (t *myCliTracer) StatGetEndpointFail(req xnet.Request, err error) {
	treq := req.(*clientRequest)
	fmt.Fprintf(os.Stderr, "StatGetEndpointFail %v:\n", treq.Header)
	printTransportError(err)
}

// optional
func (t *myCliTracer) StatConn(req xnet.Request, connInfo *xnet.ConnInfo, gotConnInfo *xnet.TraceGotConnInfo) {
	fmt.Fprintf(os.Stderr, "StatConn: %v %v\n", connInfo, gotConnInfo)
	printTransportError(gotConnInfo.Err)
}

// optional
func (t *myCliTracer) StatRoundTrip(req xnet.Request, resp xnet.Response, connInfo *xnet.ConnInfo, connState *xnet.ConnState, respInfo *xnet.TraceResultInfo) {
	treq := req.(*clientRequest)
	fmt.Fprintf(os.Stderr, "StatRoundTrip: %v %v %v %v\n", treq.Header, connInfo, connState, respInfo)
	printTransportError(respInfo.Err)
}

// optional
func (t *myCliTracer) StatResult(req xnet.Request, resp xnet.Response, info *xnet.TraceResultInfo) {
	treq := req.(*clientRequest)
	fmt.Fprintf(os.Stderr, "StatResult: %v %v\n", treq.Header, info)
	printTransportError(info.Err)
}

func myCliWriteRequest(w *bufio.Writer, req xnet.Request, rc *xnet.RequestConn) (e error) {
	rreq := req.(*clientRequest)
	rreq.written = true

	var body []byte
	if rreq.req != nil {
		body, e = json.Marshal(rreq.req)
		if e != nil {
			return
		}
	}

	rreq.Header.BodyLen = uint32(len(body))
	_, e = rreq.Header.Write(w)
	if e != nil {
		return
	}

	if len(body) > 0 {
		_, e = w.Write(body)
	}
	return
}

func myCliReadResponse(r *bufio.Reader, req xnet.Request, rc *xnet.RequestConn) (resp xnet.Response, e error) {

	_, e = r.Peek(1)
	if e != nil {
		return nil, e
	}

	rreq := req.(*clientRequest)

	rresp := &clientResponse{}
	e = rresp.Header.Read(r)
	if e != nil {
		return nil, e
	}

	fmt.Fprintf(os.Stderr, "myCliReadResponse header %v\n", rresp.Header)

	if rresp.Header.StatusCode == statusSucc && rresp.Header.CmdId != rreq.Header.CmdId {
		return nil, responseCmdidNotMatchError
	}

	if rresp.Header.BodyLen > 0 {
		body := make([]byte, int(rresp.Header.BodyLen))
		_, e = readAll(r, body, int(rresp.Header.BodyLen))
		if e != nil {
			return nil, e
		}

		e = json.Unmarshal(body, rreq.resp)
		if e != nil {
			return nil, e
		}
	}

	return rresp, nil
}

type myClient struct {
	transport *xnet.Transport
}

func newClientWithTracer() *myClient {
	// init balance and tracer
	b := &myCliBalancer{}
	t := &myCliTracer{} // optional

	balancer := &xnet.Balancer{
		GetEndpoint: b.GetEndpoint,
	}
	tracer := &xnet.Tracer{
		StatGetEndpointFail: t.StatGetEndpointFail,
		StatConn:            t.StatConn,
		StatRoundTrip:       t.StatRoundTrip,
		StatResult:          t.StatResult,
	}

	transport := &xnet.Transport{
		Balancer: balancer, // optional, use DefaultClientBalancer if nil.
		Tracer:   tracer,   // optional

		WriteRequest: myCliWriteRequest,
		ReadResponse: myCliReadResponse,

		MaxTryCount: 3,

		DialTimeout:         500 * time.Millisecond,
		WriteRequestTimeout: time.Second,
		ReadResponseTimeout: 1 * time.Second,
	}

	return &myClient{transport}
}

func (c *myClient) roundTrip(ctx context.Context, req interface{}, resp interface{}) (e error) {

	treq := &clientRequest{
		Header: header{CmdId: uint8(helloCmd)},
		ctx:    ctx,
		req:    req,
		resp:   resp,
	}
	var tresp xnet.Response
	tresp, e = c.transport.RoundTrip(treq)
	if e != nil {
		return e
	}

	rresp := tresp.(*clientResponse)
	if rresp.Header.StatusCode != statusSucc {
		return fmt.Errorf("server return fail, status code %v", rresp.Header.StatusCode)
	}

	return nil
}

func (c *myClient) hello(ctx context.Context, req *helloReq) (resp *helloResp, e error) {
	resp = &helloResp{}
	e = c.roundTrip(ctx, req, resp)
	return
}

func Example_clientAndTracer() {
	cli := newClientWithTracer()

	for i := 0; i < 2; i++ {
		req := &helloReq{"hello world"}
		resp, e := cli.hello(context.Background(), req)
		fmt.Fprintf(os.Stderr, "%v %v\n", resp, e)
	}

	fmt.Printf("end\n")
	// Output: end
}

var responseCmdidNotMatchError = errors.New("response header cmdid not match")

func newClient() *myClient {
	// init balance and tracer
	b := &myCliBalancer{}
	balancer := &xnet.Balancer{
		GetEndpoint: b.GetEndpoint,
	}

	transport := &xnet.Transport{
		Balancer: balancer, // optional, use DefaultClientBalancer if nil.

		WriteRequest: myCliWriteRequest,
		ReadResponse: myCliReadResponse,

		MaxTryCount: 3,

		DialTimeout:         500 * time.Millisecond,
		WriteRequestTimeout: time.Second,
		ReadResponseTimeout: 1 * time.Second,
	}

	return &myClient{transport}
}

func Example_client() {
	cli := newClient()

	for i := 0; i < 2; i++ {
		req := &helloReq{"hello world"}
		resp, e := cli.hello(context.Background(), req)
		fmt.Fprintf(os.Stderr, "%v %v\n", resp, e)
	}

	fmt.Printf("end\n")
	// Output: end
}
