package xfilemq

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ccxp/xgo/xlog"
	"github.com/ccxp/xgo/xutil/xfile"
)

func TestTaskPool1(te *testing.T) {

	logger := xlog.NewColorLogger(os.Stderr, xlog.Ldebug, xlog.LstdFlags|xlog.Lgoid)

	xlog.SetLogger(logger)

	var bwp = xfile.NewBufioWriterPool(1 << 20)
	var brp = xfile.NewBufioReaderPool(1 << 20)
	var bp = xfile.NewBufferPool(1 << 20)

	res, _ := ListDir("/tmp/retrylog", 0, 0)
	fmt.Printf("%v\n", res)

	l := &FileMQ{
		DataPath:            "/tmp/retrylog",
		FilePrefix:          "aaa",
		FileSplitDuration:   time.Second * 5,
		FileReserveDuration: time.Minute,
		LogStatDuration:     time.Second,

		BufioReaderPool: brp,
		BufferPool:      bp,
		BufioWriterPool: bwp,
	}
	l.Init()
	defer l.Close()

	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		k := 0
		for {
			<-ticker.C
			l.Save([]byte(fmt.Sprintf("%v", k)))
			fmt.Printf("add %v\n", k)
			k++
		}
	}()

	ticker := time.NewTicker(time.Second / 2)
	defer ticker.Stop()

	for {
		<-ticker.C

		b := l.Next()
		if len(b) > 0 {
			l.MarkSucc()
			fmt.Printf("read %s\n", b)
		}
	}

}
