package xhttp_test

import (
	"fmt"
	"github.com/ccxp/xgo/xhttp"
	"github.com/ccxp/xgo/xnet"
	"log"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

func myHandler(w http.ResponseWriter, r *http.Request) {
	// 以下两行在wego2内作用一致，但NewResponseWriter具有更好的移植兼容性
	//ww := w.(*xhttp.ResponseWriter)
	ww := xhttp.NewResponseWriter(w, r)
	
	fmt.Fprintf(os.Stderr, "match %v\n", ww.Pattern())

	helper := &xhttp.RequestHelper{r}
	v := helper.Form("key", "def")

	ww.MarshalJSON(map[string]string{"aa": v})
}

func myHandlerPrepare(w http.ResponseWriter, r *http.Request) error {
	if false {
		w.(*xhttp.ResponseWriter).WriteError("401 StatusUnauthorized", http.StatusUnauthorized)
		return errUnauthorized
	}
	return nil
}

func myHandlerPanic(w http.ResponseWriter, r *http.Request, err interface{}) {
	w.(*xhttp.ResponseWriter).WriteError("Internal Server Error", http.StatusInternalServerError)
}

// -------------------- Balancer

type myBalancer struct {
	MaxConn    int32
	MaxRequest int32

	conn    int32
	request int32
}

func (b *myBalancer) Accept(net.Conn) error {
	fmt.Fprintf(os.Stderr, "Balancer Accept\n")
	if atomic.LoadInt32(&b.conn) > b.MaxConn {
		return errExceedMaxConn
	}
	return nil
}

func (b *myBalancer) StateNew(net.Conn) {
	fmt.Fprintf(os.Stderr, "Balancer StateNew\n")
	atomic.AddInt32(&b.conn, 1)
}

func (b *myBalancer) StateActive(net.Conn) {
	fmt.Fprintf(os.Stderr, "Balancer StateActive\n")
}

func (b *myBalancer) StateNewRequest(w http.ResponseWriter, r *http.Request) error {
	fmt.Fprintf(os.Stderr, "Balancer StateNewRequest %v\n", r.URL)
	if atomic.AddInt32(&b.request, 1) > b.MaxRequest {
		w.(*xhttp.ResponseWriter).WriteError("503 balancer reject", http.StatusServiceUnavailable)
		return errExceedMaxRequest
	}

	return nil
}

func (b *myBalancer) StateEndRequest(w http.ResponseWriter, r *http.Request, d time.Duration, state *xnet.ConnState) {
	fmt.Fprintf(os.Stderr, "Balancer StateEndRequest %v %v %v\n", r.URL, d, state)
	atomic.AddInt32(&b.request, -1)
}

func (b *myBalancer) StateIdle(c net.Conn, state *xnet.ConnState) {
	fmt.Fprintf(os.Stderr, "Balancer StateIdle %v\n", state)

}

func (b *myBalancer) StateClosed(c net.Conn, state *xnet.ConnState) {
	fmt.Fprintf(os.Stderr, "Balancer StateClosed %v\n", state)
	atomic.AddInt32(&b.conn, -1)
}

// -------------------- Tracer

type myTracer struct {
}

func (b *myTracer) StateAcceptReject(conn net.Conn, err error) {
	fmt.Fprintf(os.Stderr, "Tracer StateAcceptReject %v\n", err)
}

func (b *myTracer) StateNew(net.Conn) {
	fmt.Fprintf(os.Stderr, "Tracer StateNew\n")
}

func (b *myTracer) StateClosed(conn net.Conn, state *xnet.ConnState) {
	fmt.Fprintf(os.Stderr, "Tracer StateClosed %v\n", state)
}

func (b *myTracer) StateIdle(conn net.Conn, state *xnet.ConnState) {
	fmt.Fprintf(os.Stderr, "Tracer StateIdle %v\n", state)
}

func (b *myTracer) StateNewRequestReject(w http.ResponseWriter, r *http.Request, e error) {
	fmt.Fprintf(os.Stderr, "Tracer StateNewRequestReject %v %v\n", r.URL, e)
}

func (b *myTracer) StateNewRequest(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(os.Stderr, "Tracer StateNewRequest %v\n", r.URL)
}

func (b *myTracer) StateEndRequest(w http.ResponseWriter, r *http.Request, d time.Duration, state *xnet.ConnState) {
	fmt.Fprintf(os.Stderr, "Tracer StateEndRequest %v %v %v\n", r.URL, d, state)
}

func (b *myTracer) StateHandlerBegin(w http.ResponseWriter, r *http.Request, d time.Duration) {
	fmt.Fprintf(os.Stderr, "Tracer StateHandlerBegin %v %v\n", r.URL, d)
}

func (b *myTracer) StateHandlerPrepareReject(w http.ResponseWriter, r *http.Request, d time.Duration, e error) {
	fmt.Fprintf(os.Stderr, "Tracer StateHandlerPrepareReject %v %v %v\n", r.URL, e, d)
}

func (b *myTracer) StateHandlerEnd(w http.ResponseWriter, r *http.Request, d time.Duration) {
	fmt.Fprintf(os.Stderr, "Tracer StateHandlerEnd %v %v\n", r.URL, d)
}

func (b *myTracer) StateHandlerPanic(w http.ResponseWriter, r *http.Request, d time.Duration, err interface{}) {
	fmt.Fprintf(os.Stderr, "Tracer StateHandlerPanic %v %v %v\n", r.URL, d, err)
}

func (b *myTracer) StateHandlerNotFound(w http.ResponseWriter, r *http.Request, d time.Duration) {
	fmt.Fprintf(os.Stderr, "Tracer StateHandlerNotFound %v %v\n", r.URL, d)
}

func ExampleServer_balancerAndTracer() {

	// init balance and tracer, optional
	balance := &myBalancer{
		MaxConn:    150,
		MaxRequest: 100,
	}
	tracer := &myTracer{}

	svrBalancer := &xhttp.ServerBalancer{
		Accept:          balance.Accept,
		StateNew:        balance.StateNew,
		StateActive:     balance.StateActive,
		StateNewRequest: balance.StateNewRequest,
		StateEndRequest: balance.StateEndRequest,
		StateIdle:       balance.StateIdle,
		StateClosed:     balance.StateClosed,
	}
	svrTracer := &xhttp.ServerTracer{
		StateAcceptReject: tracer.StateAcceptReject,
		StateNew:          tracer.StateNew,

		StateNewRequestReject: tracer.StateNewRequestReject,
		StateNewRequest:       tracer.StateNewRequest,
		StateEndRequest:       tracer.StateEndRequest,

		StateIdle:   tracer.StateIdle,
		StateClosed: tracer.StateClosed,

		StateHandlerBegin:         tracer.StateHandlerBegin,
		StateHandlerPrepareReject: tracer.StateHandlerPrepareReject,
		StateHandlerEnd:           tracer.StateHandlerEnd,
		StateHandlerPanic:         tracer.StateHandlerPanic,
		StateHandlerNotFound:      tracer.StateHandlerNotFound,
	}

	// init mux
	mux := &xhttp.ServePrefixMux{
		Tracer: svrTracer, // optional
	}
	mux.Handle("/aa", &xhttp.Handler{
		Handler: http.HandlerFunc(myHandler),
		Prepare: myHandlerPrepare, // optional
		Tracer:  svrTracer,        // optional
		Panic:   myHandlerPanic,   // optional
	})
	mux.Handle("/bb", http.HandlerFunc(myHandler)) // no prepare, auto add tracer

	// Add CORS support if needed.
	// CORSmux := xhttp.CORSHandler([]string{"xxx.com"}, mux)
	// or use CORSHandler for single pattern:
	// mux.Handle("/bb", xhttp.CORSHandler([]string{"xxx.com"}, handler))

	// init svr
	svr := &xhttp.Server{
		Server: http.Server{
			Addr:    ":11111",
			Handler: mux, // CORSmux,

			ReadHeaderTimeout: 2 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       60 * time.Second,
		},

		Balancer: svrBalancer, // optional
		Tracer:   svrTracer,   // optional
	}

	fmt.Fprintf(os.Stderr, "begin\n")
	log.Fatal(svr.ListenAndServe())
	fmt.Fprintf(os.Stderr, "end\n")
	// Output: end
}

var errExceedMaxConn = fmt.Errorf("errExceedMaxConn")
var errExceedMaxRequest = fmt.Errorf("errExceedMaxRequest")
var errUnauthorized = fmt.Errorf("errUnauthorized")

func ExampleServer() {

	// init mux
	mux := &xhttp.ServePrefixMux{}
	mux.Handle("/", http.HandlerFunc(myHandler)) // no prepare, auto add tracer

	// init svr
	svr := &xhttp.Server{
		Server: http.Server{
			Addr:    ":11111",
			Handler: mux,

			ReadHeaderTimeout: 2 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
	}

	fmt.Fprintf(os.Stderr, "begin\n")
	log.Fatal(svr.ListenAndServe())

	fmt.Printf("end\n")
	// Output: end
}

func ExampleCORSHandler() {

	mux := &xhttp.ServePrefixMux{}
	mux.Handle("/", http.HandlerFunc(myHandler))

	// set mux to CORSHandler
	CORSmux := &xhttp.CORSHandler{
		AllowOrigin:  []string{"qq.com"},
		AllowHeaders: "content-type, accept",
		AllowMethods: "POST, GET, OPTIONS",
		Handler:      mux,
	}

	// or set single handler to CORSHandler
	// mux.Handle("/", xhttp.CORSHandler([]string{"qq.com"}, myHandler))

	// init svr
	svr := &xhttp.Server{
		Server: http.Server{
			Addr:    ":11111",
			Handler: CORSmux,

			ReadHeaderTimeout: 2 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
	}

	fmt.Fprintf(os.Stderr, "begin\n")
	log.Fatal(svr.ListenAndServe())

	fmt.Printf("end\n")
	// Output: end
}
