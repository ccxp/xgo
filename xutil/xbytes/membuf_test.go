package xbytes

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

func print(v interface{}) {
	b, e := json.MarshalIndent(v, "", "\t")
	fmt.Fprintf(os.Stderr, "%s %v\n", b, e)
}

func printLn(f string, v ...interface{}) {
	fmt.Fprintf(os.Stderr, f, v...)
}

func TestCall1(t *testing.T) {

	b := NewBuffer(100, 10)
	a := []byte("1234567890")
	c := make([]byte, 5)
	for i := 0; i < 20; i++ {
		e := b.Write(a)
		fmt.Fprintf(os.Stderr, "w %v %v %v %v\n", e, b.size, b.begin, b.end)
	}

	for n := b.Read(c); n > 0; n = b.Read(c) {
		fmt.Fprintf(os.Stderr, "r %v %s %v %v %v\n", n, c[0:n], b.size, b.begin, b.end)
	}

	for i := 0; i < 30; i++ {
		e := b.Write(a)
		fmt.Fprintf(os.Stderr, "w1 %v %v %v %v\n", e, b.size, b.begin, b.end)

		n := b.Read(c)
		fmt.Fprintf(os.Stderr, "r1                      %v %s %v %v %v\n", n, c[0:n], b.size, b.begin, b.end)
	}

}

func TestCall2(t *testing.T) {

	c := NewContainer(100, 10)
	a := []byte("1234567890a")
	for i := 0; i < 10; i++ {
		e := c.Push(a)
		fmt.Fprintf(os.Stderr, "%v\n", e)
	}

	for b, e := c.Pop(); e == nil; b, e = c.Pop() {
		fmt.Fprintf(os.Stderr, "%v %s\n", e, b)
	}

}
