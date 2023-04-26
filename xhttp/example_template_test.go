package xhttp_test

import (
	"fmt"
	"github.com/ccxp/xgo/xhttp"
	"log"
	"net/http"
	"os"
)

var loader *xhttp.TemplateLoader

func myTemplateHandler(w http.ResponseWriter, r *http.Request) {
	ww := w.(*xhttp.ResponseWriter)

	v := map[string]int{"aa": 1, "bb": 2}
	// use ww.WriteTplEx for debug.
	ww.WriteTplEx(v, loader.Get("name"), "", r.FormValue("debug"))
	//ww.WriteTpl(v, loader.Get("name"), "")
}

func ExampleTemplateLoader() {

	loader = &xhttp.TemplateLoader{
		BaseDir:     "/home/qspace/tmp/",
		EnableCache: true,
	}

	// init mux
	mux := &xhttp.ServePrefixMux{}
	mux.Handle("/", http.HandlerFunc(myTemplateHandler))

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
