package xsender_test

import (
	"fmt"
	"github.com/ccxp/xgo/xhttp"
	"github.com/ccxp/xgo/xlog"
	"github.com/ccxp/xgo/xutil/xfile"
	"github.com/ccxp/xgo/xutil/xsender"
	"log"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

var recvCount uint32
var sendCount uint32

func ExampleDataSender() {

	xlog.SetLevel(xlog.Ldebug)

	transport := &xhttp.Transport{

		DialTimeout:           500 * time.Millisecond,
		WriteRequestTimeout:   time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ResponseBodyTimeout:   60 * time.Second,
	}

	ds := &xsender.DataSender{
		GetAddrs: func(sect int) []string {
			return []string{"127.0.0.1:11111"}
		},
		IsAddrOK: func(addr string) (bool, bool) {
			return true, true
		},

		NewRequest: func(sect int, addr string) (*http.Request, error) {
			return http.NewRequest("GET", "http://127.0.0.1:11111/", nil)
		},
		RoundTripper: func(sect int, addr string, req *http.Request) (*http.Response, error) {
			return transport.RoundTrip(req)
		},

		Step: func(sect int, b []byte) error {
			v := atomic.AddUint32(&recvCount, 1)
			xlog.Debugf("recvCount %v", v)
			return nil
		},

		WriteTimeoutDuration: time.Second,
		ReadTimeoutDuration:  time.Second * 5,
		ReplyTimeoutDuration: time.Second,
		HeartbeatDuration:    time.Second,

		// 一个来源节点发送消息的缓冲大小 (msg个数)
		SendPoolSize: 10,

		CompressType: 1,

		RetryBinLogPath:      "/tmp/retry",
		RetryTimeoutDuration: time.Minute,

		RetryBinLogBufioReaderPool: xfile.NewBufioReaderPool(1 << 20),
		RetryBinLogBufferPool:      xfile.NewBufferPool(1 << 20),
		RetryBinLogBufioWriterPool: xfile.NewBufioWriterPool(1 << 20),
	}

	ds.Init("127.0.0.1:11112")

	// init mux
	mux := &xhttp.ServePrefixMux{}
	mux.Handle("/", http.HandlerFunc(ds.ServReader)) // no prepare, auto add tracer

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

	go func() {
		ticker := time.NewTicker(time.Second * 2)
		defer ticker.Stop()
		var b = []byte("aaa")
		for {
			<-ticker.C

			v := atomic.AddUint32(&sendCount, 1)
			xlog.Debugf("sendCount %v", v)

			ds.SendEnqueue(0, b)
		}
	}()

	fmt.Fprintf(os.Stderr, "begin\n")
	log.Fatal(svr.ListenAndServe())

	fmt.Printf("end\n")
	// Output: end
}
