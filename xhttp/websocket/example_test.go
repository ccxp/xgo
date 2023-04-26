package websocket_test

import (
	"fmt"
	"github.com/ccxp/xgo/xhttp"
	"github.com/ccxp/xgo/xhttp/websocket"
	"github.com/ccxp/xgo/xlog"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"time"
)

type echoHandler struct {
}

func (h *echoHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	c, err := websocket.NewServerConn(w, r)
	if err != nil {
		fmt.Fprintf(os.Stderr, "NewServerConn %v\n", err)
		return
	}
	defer c.Close()

	for {
		// 初始化读message，里面会自动处理、忽略ping、pong、close frame。
		m, err := c.ReadMessage()
		if err != nil {
			fmt.Fprintf(os.Stderr, "ReadMessage %v\n", err)
			return
		}
		// m.OpCode 是数据类型

		// 读出所有分片的payload
		buf, err := ioutil.ReadAll(m)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ReadAll %v\n", err)
			return
		}

		fmt.Fprintf(os.Stderr, "read %s\n", buf)

		// 初始化写回复
		respMessage, err := c.NewMessage(m.OpCode)
		if err != nil {
			fmt.Fprintf(os.Stderr, "NewMessage %v\n", err)
			return
		}
		n, err := respMessage.Write(buf)
		if err == nil {
			respMessage.Close()
		}
		fmt.Fprintf(os.Stderr, "Write %v %v\n", n, err)
	}

}

// This example demonstrates a trivial echo server.
func Example_server() {

	xlog.SetLevel(xlog.Ldebug)
	http.Handle("/", new(echoHandler))
	log.Fatal(http.ListenAndServe("127.0.0.1:22222", nil))

	fmt.Printf("end\n")
	// Output: end
}

func Example_transport() {

	xlog.SetLevel(xlog.Ldebug)

	// 建立跟后端的连接
	req, err := websocket.NewRequest("ws://127.0.0.1:11111/", "http://aaaaa.com")
	if err != nil {
		fmt.Fprintf(os.Stderr, "NewRequest %v\n", err)
		return
	}

	// 使用xhttp.Transport或者http.Transport都可以
	transport := &xhttp.Transport{
		MaxTryCount:         3,
		DialTimeout:         500 * time.Millisecond,
		ResponseBodyTimeout: 0, // ResponseBodyTimeout must be 0
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "RoundTrip %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "%v %v\n", resp, err)
	defer resp.Body.Close()

	c, err := websocket.NewClientConn(req, resp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "NewClientConn %v\n", err)
		return
	}
	defer c.Close()

	// 用xhttp.Transport，返回的Conn支持SetDeadline等超时处理
	//c.SetDeadline(time.Minute)

	for i := 0; i < 100; i++ {
		// 写到后端
		m, err := c.NewMessage(websocket.TextFrame)
		//m.SetBuffer(make([]byte, 10))
		for j := 0; j < 3; j++ {
			_, err = m.Write([]byte(fmt.Sprintf("test %d %d: 1234567!", i, j)))
			if err != nil {
				fmt.Fprintf(os.Stderr, "Write %v\n", err)
				return
			}
		}
		m.Close()

		// 读后端返回
		respm, err := c.ReadMessage()
		if err != nil {
			fmt.Fprintf(os.Stderr, "ReadMessage %v\n", err)
			return
		}

		buf, err := ioutil.ReadAll(respm)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ReadAll %v\n", err)
			return
		}

		fmt.Fprintf(os.Stderr, "read resp %s\n", buf)
	}

	fmt.Printf("end\n")
	// Output: end
}

/*
func Example_proxy() {

	h := &websocket.WebSocketProxyHandler{
		Scheme: "http",
		Director: func(w http.ResponseWriter, r *http.Request, target *http.Request) error {
			target.URL.Host = "10.123.102.140:12346"
			target.URL.Path = "/"
			return nil
		},
		ReadTimeout:  time.Minute * 5,
		WriteTimeout: time.Second,
	}

	http.Handle("/proxy", h)
	log.Fatal(http.ListenAndServe("10.123.102.140:12347", nil))

	fmt.Printf("end\n")
	// Output: end
}
*/
