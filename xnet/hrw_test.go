package xnet

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"
)

func TestNewHrwHash(t *testing.T) {
	rand.Seed(time.Now().Unix())
	nodes := make([]string, 32)
	for i, _ := range nodes {
		nodes[i] = Inet_ntoa(rand.Uint32())
	}
	// fmt.Fprintf(os.Stderr, "%v\n", nodes)

	hrw := NewHrwHash(nodes)
	// fmt.Fprintf(os.Stderr, "%v\n", hrw.ids)

	if true {
		m := make(map[string]int)
		k := make([]byte, 4)
		for i := 0; i < 8000; i++ {
			binary.BigEndian.PutUint32(k, uint32(i))
			ip := hrw.Calc(k)[2]
			m[ip] = m[ip] + 1
		}
		for k, v := range m {
			fmt.Fprintf(os.Stderr, "%v\t%v\n", k, v)
		}
	}

	loop := 100000
	t0 := time.Now()

	k := make([]byte, 4)
	for i := 0; i < loop; i++ {
		binary.BigEndian.PutUint32(k, uint32(i))
		hrw.Calc(k)
	}

	du := time.Now().Sub(t0)
	fmt.Fprintf(os.Stderr, "%v %v\n", du, float64(loop)*float64(time.Second.Nanoseconds())/float64(du.Nanoseconds()))

	if true {
		m := make(map[string]int)
		k := make([]byte, 4)
		for i := 0; i < 8000; i++ {
			binary.BigEndian.PutUint32(k, uint32(i))
			rs := hrw.NewCalc(k)
			ip := rs.Get()[2]
			rs.Close()
			m[ip] = m[ip] + 1
		}
		for k, v := range m {
			fmt.Fprintf(os.Stderr, "%v\t%v\n", k, v)
		}
	}

	t0 = time.Now()

	for i := 0; i < loop; i++ {
		binary.BigEndian.PutUint32(k, uint32(i))
		rs := hrw.NewCalc(k)
		rs.Close()
	}

	du = time.Now().Sub(t0)
	fmt.Fprintf(os.Stderr, "%v %v\n", du, float64(loop)*float64(time.Second.Nanoseconds())/float64(du.Nanoseconds()))

}
