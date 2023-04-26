package xhttp_test

import (
	"fmt"
	"github.com/ccxp/xgo/xhttp"
	"log"
	"net/http"
	"os"
)

func ExampleFileHandler() {

	fh := &xhttp.FileHandler{
		BaseDir:            "/home/qspace/tmp/",
		UrlPrefix:          "/static/",
		CacheControlHeader: "max-age=60",
		EnableCache:        true,
		CacheFileMaxSize:   1024 * 1024 * 10,
	}

	// init mux
	mux := &xhttp.ServePrefixMux{}
	mux.Handle("/", fh)

	// init svr
	svr := &xhttp.Server{
		Server: http.Server{
			Addr:    ":11111",
			Handler: mux,
		},
	}

	fmt.Fprintf(os.Stderr, "begin\n")
	log.Fatal(svr.ListenAndServe())

	fmt.Printf("end\n")
	// Output: end
}
