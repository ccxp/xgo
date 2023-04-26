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

func myRESTHandler(w http.ResponseWriter, r *http.Request) {
	// 以下两行在wego2内作用一致，但NewResponseWriter具有更好的移植兼容性
	//ww := w.(*xhttp.ResponseWriter)
	ww := xhttp.NewResponseWriter(w, r)

	fmt.Fprintf(os.Stderr, "match %v\n", ww.Pattern())

	// get RESTful params by ww.Params()
	params := ww.Params()
	//v, ok := params.Get("key")
	ww.MarshalJSON(params)
}

func myRESTHandlerPrepare(w http.ResponseWriter, r *http.Request) error {
	if false {
		w.(*xhttp.ResponseWriter).WriteError("401 StatusUnauthorized", http.StatusUnauthorized)
		return errRESTUnauthorized
	}
	return nil
}

func myRESTHandlerPanic(w http.ResponseWriter, r *http.Request, err interface{}) {
	w.(*xhttp.ResponseWriter).WriteError("500 Internal Server Error", http.StatusInternalServerError)
}

// -------------------- Balancer

type myRESTBalancer struct {
	MaxConn    int32
	MaxRequest int32

	conn    int32
	request int32
}

func (b *myRESTBalancer) Accept(net.Conn) error {
	fmt.Fprintf(os.Stderr, "Balancer Accept\n")
	if atomic.LoadInt32(&b.conn) > b.MaxConn {
		return errRESTExceedMaxConn
	}
	return nil
}

func (b *myRESTBalancer) StateNew(net.Conn) {
	fmt.Fprintf(os.Stderr, "Balancer StateNew\n")
	atomic.AddInt32(&b.conn, 1)
}

func (b *myRESTBalancer) StateActive(net.Conn) {
	fmt.Fprintf(os.Stderr, "Balancer StateActive\n")
}

func (b *myRESTBalancer) StateNewRequest(w http.ResponseWriter, r *http.Request) error {
	fmt.Fprintf(os.Stderr, "Balancer StateNewRequest %v\n", r.URL)
	if atomic.AddInt32(&b.request, 1) > b.MaxRequest {
		w.(*xhttp.ResponseWriter).WriteError("503 balancer reject", http.StatusServiceUnavailable)
		return errRESTExceedMaxRequest
	}

	return nil
}

func (b *myRESTBalancer) StateEndRequest(w http.ResponseWriter, r *http.Request, d time.Duration, state *xnet.ConnState) {
	fmt.Fprintf(os.Stderr, "Balancer StateEndRequest %v %v %v\n", r.URL, d, state)
	atomic.AddInt32(&b.request, -1)
}

func (b *myRESTBalancer) StateIdle(c net.Conn, state *xnet.ConnState) {
	fmt.Fprintf(os.Stderr, "Balancer StateIdle %v\n", state)

}

func (b *myRESTBalancer) StateClosed(c net.Conn, state *xnet.ConnState) {
	fmt.Fprintf(os.Stderr, "Balancer StateClosed %v\n", state)
	atomic.AddInt32(&b.conn, -1)
}

// -------------------- Tracer

type myRESTTracer struct {
}

func (b *myRESTTracer) StateAcceptReject(conn net.Conn, err error) {
	fmt.Fprintf(os.Stderr, "Tracer StateAcceptReject %v\n", err)
}

func (b *myRESTTracer) StateNew(net.Conn) {
	fmt.Fprintf(os.Stderr, "Tracer StateNew\n")
}

func (b *myRESTTracer) StateClosed(conn net.Conn, state *xnet.ConnState) {
	fmt.Fprintf(os.Stderr, "Tracer StateClosed %v\n", state)
}

func (b *myRESTTracer) StateIdle(conn net.Conn, state *xnet.ConnState) {
	fmt.Fprintf(os.Stderr, "Tracer StateIdle %v\n", state)
}

func (b *myRESTTracer) StateNewRequestReject(w http.ResponseWriter, r *http.Request, e error) {
	fmt.Fprintf(os.Stderr, "Tracer StateNewRequestReject %v %v\n", r.URL, e)
}

func (b *myRESTTracer) StateNewRequest(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(os.Stderr, "Tracer StateNewRequest %v\n", r.URL)
}

func (b *myRESTTracer) StateEndRequest(w http.ResponseWriter, r *http.Request, d time.Duration, state *xnet.ConnState) {
	fmt.Fprintf(os.Stderr, "Tracer StateEndRequest %v %v %v\n", r.URL, d, state)
}

func (b *myRESTTracer) StateHandlerBegin(w http.ResponseWriter, r *http.Request, d time.Duration) {
	fmt.Fprintf(os.Stderr, "Tracer StateHandlerBegin %v %v\n", r.URL, d)
}

func (b *myRESTTracer) StateHandlerPrepareReject(w http.ResponseWriter, r *http.Request, d time.Duration, e error) {
	fmt.Fprintf(os.Stderr, "Tracer StateHandlerPrepareReject %v %v %v\n", r.URL, e, d)
}

func (b *myRESTTracer) StateHandlerEnd(w http.ResponseWriter, r *http.Request, d time.Duration) {
	fmt.Fprintf(os.Stderr, "Tracer StateHandlerEnd %v %v\n", r.URL, d)
}

func (b *myRESTTracer) StateHandlerPanic(w http.ResponseWriter, r *http.Request, d time.Duration, err interface{}) {
	fmt.Fprintf(os.Stderr, "Tracer StateHandlerPanic %v %v %v\n", r.URL, d, err)
}

func (b *myRESTTracer) StateHandlerNotFound(w http.ResponseWriter, r *http.Request, d time.Duration) {
	fmt.Fprintf(os.Stderr, "Tracer StateHandlerNotFound %v %v\n", r.URL, d)
}

func ExampleServer_rESTfulBalancerAndTracer() {

	// init balance and tracer, optional
	balance := &myRESTBalancer{
		MaxConn:    150,
		MaxRequest: 100,
	}
	tracer := &myRESTTracer{}

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
	mux := &xhttp.ServeRESTfulMux{
		Tracer: svrTracer, // optional
	}
	mux.Handle("GET", "/:aa", &xhttp.Handler{
		Handler: http.HandlerFunc(myRESTHandler),
		Prepare: myRESTHandlerPrepare, // optional
		Tracer:  svrTracer,            // optional
		Panic:   myRESTHandlerPanic,   // optional
	})

	mux.Handle("GET", "/bb", &xhttp.Handler{
		Handler: http.HandlerFunc(myRESTHandler),
		Prepare: myRESTHandlerPrepare, // optional
		Tracer:  svrTracer,            // optional
		Panic:   myRESTHandlerPanic,   // optional
	})
	mux.Handle("GET", "/*/:cc/", http.HandlerFunc(myRESTHandler)) // no prepare, auto add tracer

	// Add CORS support if needed.
	// CORSmux := xhttp.CORSHandler([]string{"xxx.com"}, mux)
	// or use CORSHandler for single pattern:
	// mux.Handle("GET", "/bb", xhttp.CORSHandler([]string{"xxx.com"}, handler))
	// mux.Handle("OPTIONS", "/bb", xhttp.CORSHandler([]string{"xxx.com"}, nil))
	// * is for global pattern
	// mux.Handle("OPTIONS", "*", xhttp.CORSHandler([]string{"xxx.com"}, nil))

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

	fmt.Printf("end\n")
	// Output: end
}

var errRESTExceedMaxConn = fmt.Errorf("errExceedMaxConn")
var errRESTExceedMaxRequest = fmt.Errorf("errExceedMaxRequest")
var errRESTUnauthorized = fmt.Errorf("errUnauthorized")

func ExampleServer_rESTful() {

	// init mux
	mux := &xhttp.ServeRESTfulMux{}
	mux.Handle("GET", "/*/:cc/", http.HandlerFunc(myRESTHandler)) // no prepare, auto add tracer

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
