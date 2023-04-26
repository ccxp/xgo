package xhttp_test

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/ccxp/xgo/xhttp"
)

func ExampleReverseProxyHandler() {

	h := &xhttp.ReverseProxyHandler{

		Director: func(w http.ResponseWriter, r *http.Request, target *http.Request) error {
			target.URL.Scheme = "http"
			target.URL.Host = "127.0.0.1:22222"
			return nil
		},
	}

	// init mux
	mux := &xhttp.ServePrefixMux{}
	mux.Handle("/", h) // no prepare, auto add tracer

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
