package xhttp_test

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/ccxp/xgo/xhttp"
	"github.com/ccxp/xgo/xnet"
)

type myCliHostAddrs struct {
	addrs       []string
	resolveTime int64
}

type myCliBalancer struct {
	mu        sync.Mutex
	hostAddrs map[string]myCliHostAddrs
}

// Get addr info.
func (b *myCliBalancer) GetEndpoint(ctx context.Context, req xnet.Request, addrUsed []string) (connInfo xnet.ConnInfo, err error) {

	r := xhttp.GetTransportRequest(req)
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
		b.hostAddrs = make(map[string]myCliHostAddrs)
	}
	now := time.Now().Unix()

	hostAddrs := b.hostAddrs[host]
	if hostAddrs.resolveTime < now-60 {
		hostAddrs.resolveTime = now
		addrs, err := net.LookupHost(host)
		if err == nil {
			hostAddrs.addrs = make([]string, len(addrs))
			for i, a := range addrs {
				hostAddrs.addrs[i] = net.JoinHostPort(a, port)
			}
		}
		b.hostAddrs[host] = hostAddrs
	}

	addrs = hostAddrs.addrs
	b.mu.Unlock()

	if len(addrs) > 0 {

		cnt := len(addrs)
		offset := 0
		if len(addrs) > 1 {
			offset = rand.Intn(len(addrs))
		}

		var used bool

		for i := 0; i < cnt; i++ {
			a := addrs[(i+offset)%cnt]
			used = false
			for _, au := range addrUsed {
				if au == a {
					used = true
					break
				}
			}
			if used {
				continue
			}

			return xnet.ConnInfo{
				Network: "tcp",
				Scheme:  r.URL.Scheme,
				Addr:    a,
				Host:    host,
			}, nil
		}
	}
	fmt.Fprintf(os.Stderr, "no more endpoint\n")
	return xnet.ConnInfo{}, xnet.ErrGetEndpointFail
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
	rreq := xhttp.GetTransportRequest(req)
	fmt.Fprintf(os.Stderr, "StatGetEndpointFail %v:\n", rreq.URL)
	printTransportError(err)
}

// optional
func (t *myCliTracer) StatConn(req xnet.Request, connInfo *xnet.ConnInfo, gotConnInfo *xnet.TraceGotConnInfo) {
	fmt.Fprintf(os.Stderr, "StatConn: %v %v\n", connInfo, gotConnInfo)
	printTransportError(gotConnInfo.Err)
}

// optional
func (t *myCliTracer) StatRoundTrip(req xnet.Request, resp xnet.Response, connInfo *xnet.ConnInfo, connState *xnet.ConnState, respInfo *xnet.TraceResultInfo) {
	rreq := xhttp.GetTransportRequest(req)
	/*if resp != nil{
		rresp := xhttp.GetTransportResponse(resp)
	}*/
	fmt.Fprintf(os.Stderr, "StatRoundTrip: %v %v %v %v\n", rreq.URL, connInfo, connState, respInfo)
	printTransportError(respInfo.Err)
}

// optional
func (t *myCliTracer) StatResult(req xnet.Request, resp xnet.Response, respInfo *xnet.TraceResultInfo) {
	rreq := xhttp.GetTransportRequest(req)
	fmt.Fprintf(os.Stderr, "StatResult: %v %v\n", rreq.URL, respInfo)
	printTransportError(respInfo.Err)
}

func ExampleTransport_balancerAndTracer() {

	// init balance and tracer
	b := &myCliBalancer{}
	t := &myCliTracer{} // optional

	balancer := &xnet.Balancer{
		GetEndpoint: b.GetEndpoint,
		//StatConn:      xx, // optional
		//StatRoundTrip: xx, // optional
		//PutIdleConn:   xx, // optional
	}
	tracer := &xnet.Tracer{
		StatGetEndpointFail: t.StatGetEndpointFail,
		StatConn:            t.StatConn,
		StatRoundTrip:       t.StatRoundTrip,
		StatResult:          t.StatResult,
	}

	transport := &xhttp.Transport{
		Balancer: balancer, // optional, use DefaultClientBalancer if nil.
		Tracer:   tracer,   // optional

		RetryWritten: false,
		MaxTryCount:  3,

		DialTimeout:           500 * time.Millisecond,
		WriteRequestTimeout:   time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ResponseBodyTimeout:   60 * time.Second,
	}

	req, _ := http.NewRequest("GET", "http://127.0.0.1:11111/aa", nil)
	resp, e := transport.RoundTrip(req)
	fmt.Fprintf(os.Stderr, "resp %v %v\n", resp, e)

	helper := &xhttp.ResponseHelper{
		Resp:          resp,
		Err:           e,
		CheckStatusOK: true,
	}
	body, e := helper.Body()
	// e := helper.UnmarshalJSON(xxx)
	if e == nil {
		fmt.Fprintf(os.Stderr, "body: %v\n%s\n", e, body)
	}

	fmt.Printf("end\n")
	// Output: end
}

func ExampleTransport() {

	transport := &xhttp.Transport{
		RetryWritten:          false,
		MaxTryCount:           3,
		DialTimeout:           500 * time.Millisecond,
		WriteRequestTimeout:   time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ResponseBodyTimeout:   60 * time.Second,
	}

	req, _ := http.NewRequest("GET", "http://127.0.0.1:11111/aa", nil)
	resp, e := transport.RoundTrip(req)
	fmt.Fprintf(os.Stderr, "resp %v %v\n", resp, e)

	helper := &xhttp.ResponseHelper{
		Resp:          resp,
		Err:           e,
		CheckStatusOK: true,
	}
	body, e := helper.Body()
	// e := helper.UnmarshalJSON(xxx)
	if e == nil {
		fmt.Fprintf(os.Stderr, "body: %v\n%s\n", e, body)
	}

	fmt.Printf("end\n")
	// Output: end

}
