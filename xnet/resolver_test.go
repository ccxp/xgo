package xnet

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ccxp/xgo/xlog"
)

func TestAsyncSingleHostBalancer(t *testing.T) {

	xlog.SetLevel(xlog.Ldebug)

	a := AsyncSingleHostBalancer{
		Host: "qq.com",
		Port: 80,
	}

	// b := a.Build()

	/*ctx := context.Background()
	var req Request
	var addrUsed []string*/

	for {
		// addrs, e := b.GetEndpoint(ctx, req, addrUsed)
		r, _, e := a.GetAddr()
		fmt.Fprintf(os.Stderr, "GetEndpoint %+v, %v\n", r, e)

		time.Sleep(time.Second * 5)
	}

}
