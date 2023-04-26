package xmq

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ccxp/xgo/xlog"
)

func TestCall1(t *testing.T) {

	xlog.SetLevel(xlog.Ldebug)

	q := &MQ{
		QueueSize:      1 << 20,
		RetryQueueSize: 1 << 20,
		BlockSize:      1 << 10,

		DataPath:   "/tmp/mq",
		FilePrefix: "q1",

		// SkipStorageError: true,
		// SyncWrite: true,

		Handler: func(item *QueueItem) HANDLE_RESULT {
			xlog.Warnf("dooo %s %v %v %v %v", item.Data1, item.UpdateTime,
				item.HandlerCount, item.Uuid1, item.Uuid2)
			return RESULT_OK
			return HANDLE_RESULT(rand.Intn(3))
		},
		WorkerCount:      5,
		Delay:            time.Second * 2,
		MaxRetry:         3,
		RetryWorkerCount: 5,
		RetryDelay:       time.Second * 5,
	}

	e := q.Init()
	if e != nil {
		xlog.Errorf("init fail: %v", e)
	}

	// return

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)

	go func() {
		<-sigs

		q.Close()
		os.Exit(0)
	}()

	for {

		s := fmt.Sprintf("%v", time.Now())
		item := &QueueItem{
			Data1: []byte(s),
		}
		e = q.Push(item)
		if e != nil {
			xlog.Errorf("Push fail: %v", e)
		}

		xlog.Debugf("add %v", s)

		time.Sleep(time.Second)
	}

}

func TestCall2(t *testing.T) {

	xlog.SetLevel(xlog.Ldebug)

	q := &MQ{
		QueueSize: 1 << 20,
		// RetryQueueSize: 1 << 20,
		BlockSize: 1 << 10,

		DataPath:   "/tmp/mq",
		FilePrefix: "q1",

		// Delay: time.Second,
	}

	e := q.Init()
	if e != nil {
		xlog.Errorf("init fail: %v", e)
	}

	// return

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)

	go func() {
		<-sigs

		q.Close()
		os.Exit(0)
	}()

	go func() {
		for {
			xlog.Warnf("size %v", q.Size(false))
			item, _ := q.Pop(false)
			if item != nil {
				xlog.Warnf("dooo %s %v %v", item.Data1, item.UpdateTime, item.HandlerCount)
				q.SetResult(false, item, RESULT_OK, context.Background())
			}
			time.Sleep(time.Millisecond * 100)
		}
	}()

	for {

		s := fmt.Sprintf("%v", time.Now())
		item := &QueueItem{
			Data1: []byte(s),
		}
		e = q.Push(item)
		if e != nil {
			xlog.Errorf("Push fail: %v", e)
		}

		xlog.Debugf("add %v", s)

		time.Sleep(time.Second)
	}

}

func TestCall3(t *testing.T) {

	xlog.SetLevel(xlog.Ldebug)

	q := &MQ{
		QueueSize:      1 << 20,
		RetryQueueSize: 1 << 20,
		BlockSize:      1 << 10,

		DataPath:   "/tmp/mq",
		FilePrefix: "q1",

		BatchHandler: func(items []*QueueItem, rets []HANDLE_RESULT) {
			for i, item := range items {
				xlog.Warnf("dooo [%d] %s %v %v", i, item.Data1, item.UpdateTime, item.HandlerCount)
				rets[i] = RESULT_OK
				//rets[i] = HANDLE_RESULT(rand.Intn(3))
			}
		},
		Delay:             time.Second,
		BatchMaxItem:      10,
		BatchWaitDuration: time.Second * 2,

		MaxRetry:         3,
		WorkerCount:      1,
		RetryWorkerCount: 1,
		RetryDelay:       time.Second * 5,
	}

	e := q.Init()
	if e != nil {
		xlog.Errorf("init fail: %v", e)
	}

	// return

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)

	go func() {
		<-sigs

		q.Close()
		os.Exit(0)
	}()

	for {

		s := fmt.Sprintf("%v", time.Now())
		item := &QueueItem{
			Data1: []byte(s),
		}
		e = q.Push(item)
		if e != nil {
			xlog.Errorf("Push fail: %v", e)
		}

		xlog.Debugf("add %v", s)

		time.Sleep(time.Second / 5)
	}

}

func TestCall4(t *testing.T) {

	xlog.SetLevel(xlog.Lerror)

	cnt := 10000
	doCnt := uint32(0)
	var ts1 time.Time
	q := &MQ{
		QueueSize:      1 << 30,
		RetryQueueSize: 1 << 20,
		BlockSize:      1 << 10,

		// DataPath:   "/tmp/mq",
		FilePrefix: "q1",

		/*BatchHandler: func(items []*QueueItem, rets []HANDLE_RESULT) {
			for i, _ := range items {
				// xlog.Warnf("dooo [%d] %s %v %v", i, item.Data1, item.UpdateTime, item.HandlerCount)
				rets[i] = RESULT_OK
				//rets[i] = HANDLE_RESULT(rand.Intn(3))

				j := atomic.AddUint32(&doCnt, 1)
				if j == 1 {
					ts1 = time.Now()
				} else if int(j) == cnt {
					fmt.Fprintf(os.Stderr, "XXX %v %v\n", time.Now().Sub(ts1), float64(cnt)*float64(time.Second.Nanoseconds())/float64(time.Now().Sub(ts1).Nanoseconds()))
				}
			}
		},*/
		Handler: func(item *QueueItem) HANDLE_RESULT {
			j := atomic.AddUint32(&doCnt, 1)
			if j == 1 {
				ts1 = time.Now()
			} else if int(j) == cnt {
				fmt.Fprintf(os.Stderr, "XXX %v %.3f\n", time.Now().Sub(ts1), float64(cnt)*float64(time.Second.Nanoseconds())/float64(time.Now().Sub(ts1).Nanoseconds()))
			}
			// fmt.Fprintf(os.Stderr, "%v %s\n", j, item.Data1)
			return RESULT_OK
		},
		// Delay:             time.Second * 2,
		BatchMaxItem:      100,
		BatchWaitDuration: time.Microsecond * 1000,

		MaxRetry:         3,
		WorkerCount:      10,
		RetryWorkerCount: 10,
		RetryDelay:       time.Second * 5,
	}

	e := q.Init()
	if e != nil {
		xlog.Errorf("init fail: %v", e)
	}

	// return

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)

	go func() {
		<-sigs

		q.Close()
		os.Exit(0)
	}()

	ts := time.Now()

	for i := 0; i < cnt; i++ {

		s := fmt.Sprintf("%v", i)
		item := &QueueItem{
			Data1: []byte(s),
		}
		e = q.Push(item)
		if e != nil {
			fmt.Fprintf(os.Stderr, "Push fail: %v\n", e)
		}
	}

	fmt.Fprintf(os.Stderr, "YYY %v %.3f\n", time.Now().Sub(ts), float64(cnt)*float64(time.Second.Nanoseconds())/float64(time.Now().Sub(ts).Nanoseconds()))

	time.Sleep(time.Minute * 10)

}
