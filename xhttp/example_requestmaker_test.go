package xhttp_test

import (
	"fmt"
	"github.com/ccxp/xgo/xhttp"
)

type queryData struct {
	Aa int      `query:"aa" json:"aa"`
	Bb []string `query:"bb" json:"bb"`
	Cc *int     `query:"cc"`
	Dd []*int   `query:"dd"`
}

func ExampleRequestBuilder() {

	bu := &xhttp.RequestBuilder{
		Url: "http://qq.com/aa",
	}

	bu.AddForm("xxx", "yyy")

	q := &queryData{
		Aa: 1,
		Bb: []string{"bb", "bbb"},
	}
	bu.FormMarshal(q)

	req, _ := bu.Build()
	fmt.Printf("%v\n", req.URL)

	// Output: http://qq.com/aa?aa=1&bb=bb&bb=bbb&xxx=yyy

}
