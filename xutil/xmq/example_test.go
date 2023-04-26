package xmq_test

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ccxp/xgo/xlog"
	"github.com/ccxp/xgo/xutil/xmq"
)

func ExampleMQ() {

	xlog.SetLevel(xlog.Ldebug)

	q := &xmq.MQ{
		QueueSize:      1 << 20, // 队列占用内存
		RetryQueueSize: 1 << 20, // 重试队列占用内存
		BlockSize:      1 << 10, // 重试队列占用内存

		// DataPath:   "/tmp/mq", // 注意目录要存在
		FilePrefix: "q1", // 文件名前缀，可用队列名

		// 一次处理一个请求
		Handler: func(item *xmq.QueueItem) xmq.HANDLE_RESULT {
			xlog.Warnf("dooo %s %v %v %v %v", item.Data1, item.UpdateTime,
				item.HandlerCount, item.Uuid1, item.Uuid2)
			return xmq.RESULT_OK
		},
		WorkerCount: 5,
		// Delay:            time.Second * 2, // 可以设置延迟处理
		MaxRetry:         3,
		RetryWorkerCount: 5,
		RetryDelay:       time.Second * 5, // 重试延迟多久

		AutoClean: true,
	}

	e := q.Init()
	if e != nil {
		xlog.Errorf("init fail: %v", e)
	}

	// 一般服务注意要在退出时做Close处理.
	// net/http、xhttp、xhttp、whttp、svrkit有单独的处理函数RegisterOnShutdown，并且要在start Server下一行用Sleep(1s)保证Close完成.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)
	go func() {
		<-sigs

		q.Close()
		os.Exit(0)
	}()

	for {

		s := fmt.Sprintf("%v", time.Now())
		item := &xmq.QueueItem{
			Data1: []byte(s),
			// Data2: .. // Data1~5可自由使用
		}
		e = q.Push(item)
		if e != nil {
			xlog.Errorf("Push fail: %v", e)
		}

		xlog.Debugf("add %v", s)

		time.Sleep(time.Second)
	}

	fmt.Printf("end\n")
	// Output: end
}

func ExampleMQ_batch() {

	xlog.SetLevel(xlog.Ldebug)

	q := &xmq.MQ{
		QueueSize:      1 << 20, // 队列占用内存
		RetryQueueSize: 1 << 20, // 重试队列占用内存
		BlockSize:      1 << 10, // 重试队列占用内存

		DataPath:   "/tmp/mq", // 注意目录要存在
		FilePrefix: "q1",      // 文件名前缀，可用队列名

		//批量处理
		BatchHandler: func(items []*xmq.QueueItem, rets []xmq.HANDLE_RESULT) {
			for i, item := range items {
				xlog.Warnf("dooo [%d] %s %v %v", i, item.Data1, item.UpdateTime, item.HandlerCount)
				rets[i] = xmq.RESULT_OK
				//rets[i] = HANDLE_RESULT(rand.Intn(3))
			}
		},
		BatchMaxItem:      10,              // 一次最多处理多少个请求
		BatchWaitDuration: time.Second * 2, // 最大等待多久

		WorkerCount: 5,
		// Delay:            time.Second * 2, // 可以设置延迟处理
		MaxRetry:         3,
		RetryWorkerCount: 5,
		RetryDelay:       time.Second * 5, // 重试延迟多久

		AutoClean: true,
	}

	e := q.Init()
	if e != nil {
		xlog.Errorf("init fail: %v", e)
	}

	// 一般服务注意要在退出时做Close处理.
	// net/http、xhttp、xhttp、whttp、svrkit有单独的处理函数RegisterOnShutdown，并且要在start Server下一行用Sleep(1s)保证Close完成.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)
	go func() {
		<-sigs

		q.Close()
		os.Exit(0)
	}()

	for {

		s := fmt.Sprintf("%v", time.Now())
		item := &xmq.QueueItem{
			Data1: []byte(s),
			// Data2: .. // Data1~5可自由使用
		}
		e = q.Push(item)
		if e != nil {
			xlog.Errorf("Push fail: %v", e)
		}

		xlog.Debugf("add %v", s)

		time.Sleep(time.Second)
	}

	fmt.Printf("end\n")
	// Output: end
}
