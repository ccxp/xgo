package xhttp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func print(x interface{}) {
	b, e := json.MarshalIndent(x, "", "\t")
	fmt.Printf("%s %v\n", b, e)
}

type MyData struct {
	Aa int      `query:"aa" json:"aa"`
	Bb []string `query:"bb" json:"bb"`
	Cc *int     `query:"cc"`
	Dd []*int   `query:"dd"`
}

type MyDataA struct {
	Aa int `query:"aa" json:"aa"`
}

type MyDataB struct {
	MyDataA
	Bb []string `query:"bb" json:"bb"`
	Cc *int     `query:"cc"`
	Dd []*int   `query:"dd"`
}

func Int(v int) *int {
	t := v
	return &t
}

func TestCoder(t *testing.T) {

	p := MyData{
		Aa: 10,
		Bb: []string{"a", "b"},
		Cc: Int(1),
		Dd: []*int{Int(2), nil},
	}

	v := &Values{}
	err := v.Marshal(p)
	fmt.Printf("ret %v %v\n", v, err)

	r := &MyDataB{}
	e := v.Unmarshal(r)
	fmt.Printf("ret %+v %v\n", r, e)

	/*a := make(map[string][]string)
	e = v.Unmarshal(&a)
	fmt.Printf("ret %v %v\n", a, e)

	v = &Values{}
	err = v.Marshal(p)
	fmt.Printf("ret %v %v\n", v, err)*/
}

func myHandler111(w http.ResponseWriter, r *http.Request) {
}

func TestREST(t *testing.T) {
	r := newRestFulRoot()
	r.Handle("GET", "/", http.HandlerFunc(myHandler111))
	r.Handle("GET", "/aa", http.HandlerFunc(myHandler111))
	r.Handle("GET", "/bb/", http.HandlerFunc(myHandler111))
	r.Handle("GET", "/:ff", http.HandlerFunc(myHandler111))
	r.Handle("GET", "/:ff/", http.HandlerFunc(myHandler111))
	r.Handle("GET", "/:ff/gg", http.HandlerFunc(myHandler111))
	r.Handle("GET", "/:ff/:hh/ii", http.HandlerFunc(myHandler111))
	//print(r.Root)

	p, _, u := r.Handler("/ffaf/ggg/ii")
	fmt.Printf("u %v\n", u)
	print(p)

	loop := 100000
	t1 := time.Now()
	for i := 0; i < loop; i++ {
		r.Handler("/ffaf/ggg/ii")
	}
	t2 := time.Now()
	span := t2.Sub(t1)
	fmt.Printf("%v %.3f\n", span.Nanoseconds()/time.Millisecond.Nanoseconds(), float64(loop)*1e9/float64(span.Nanoseconds()))
}
