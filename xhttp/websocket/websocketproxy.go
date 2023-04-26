package websocket

/*
import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ccxp/xgo/xhttp"
	"github.com/ccxp/xgo/xlog"
)

// 废弃，直接使用xhttp.ReverseProxyHandler
type WebSocketProxyHandler struct {
	// target scheme, http/https
	Scheme string

	// 设置到连接的timeout，只有用xhttp.Transport得到的返回才支持。
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// 用于修改target请求，如果返回error != nil，则不再进行后续处理。
	Director func(w http.ResponseWriter, r *http.Request, target *http.Request) error

	// RoundTrip is used to perform proxy requests.
	// If nil, use default transport.
	RoundTrip func(w http.ResponseWriter, r *http.Request, target *http.Request) (*http.Response, error)

	// BufferPool optionally specifies a buffer pool to
	// get byte slices for use by io.CopyBuffer when
	// copying HTTP response bodies.
	BufferPool xhttp.BufferPool
}

var defaultTransport = &xhttp.Transport{
	MaxTryCount: 3,
	DialTimeout: 1000 * time.Millisecond,
}

func defaultRoundTrip(w http.ResponseWriter, r *http.Request, target *http.Request) (*http.Response, error) {
	return http.DefaultTransport.RoundTrip(target)
}

func (p *WebSocketProxyHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {

	ctx := req.Context()
	outreq, err := NewRequest(req.URL.RequestURI(), req.Header.Get("Origin"))
	if err != nil {
		xlog.Errorf("NewRequest fail: %v", err)
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	outreq.URL.Scheme = req.URL.Scheme
	if p.Scheme != "" {
		outreq.URL.Scheme = p.Scheme
	}
	if outreq.URL.Scheme == "" {
		outreq.URL.Scheme = "ws"
	}
	outreq.URL.Host = req.Host
	outreq.Host = req.Host

	protocol := strings.TrimSpace(req.Header.Get("Sec-Websocket-Protocol"))
	if protocol != "" {
		outreq.Header.Set("Sec-Websocket-Protocol", protocol)
	}

	for k, vv := range req.Header {
		if _, ok := outreq.Header[k]; !ok {
			vv2 := make([]string, len(vv))
			copy(vv2, vv)
			outreq.Header[k] = vv2
		}
	}

	outreq = outreq.WithContext(ctx)

	err = p.Director(rw, req, outreq)
	if err != nil {
		return
	}

	roundTrip := p.RoundTrip
	if roundTrip == nil {
		roundTrip = defaultRoundTrip
	}
	res, err := roundTrip(rw, req, outreq)
	if err != nil {
		xlog.Errorf("roundTrip fail: %v", err)
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	defer res.Body.Close()

	connFront, err := NewServerConn(rw, req)
	if err != nil {
		xlog.Errorf("NewServerConn fail: %v", err)
		return
	}
	defer connFront.Close()

	connEnd, err := NewClientConn(outreq, res)
	if err != nil {
		xlog.Errorf("NewClientConn fail: %v", err)
		return
	}
	defer connEnd.Close()

	connFront.ReadPingPong = true
	connEnd.ReadPingPong = true

	done := make(chan struct{}, 2)
	go p.loop(connFront, connEnd, done)
	go p.loop(connEnd, connFront, done)
	<-done
	<-done
}

func minDuGtZero(a time.Duration, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	} else if a < b {
		return a
	} else if b <= 0 {
		return a
	}
	return b
}

func (p *WebSocketProxyHandler) loop(from, to *Conn, done chan struct{}) {
	var buf []byte
	if p.BufferPool != nil {
		buf = p.BufferPool.Get()
		defer p.BufferPool.Put(buf)
	} else {
		buf = make([]byte, 32<<10)
	}

	var fromaddr, toaddr string
	if from.conn != nil {
		fromaddr = from.conn.RemoteAddr().String()
	}
	if to.conn != nil {
		toaddr = to.conn.RemoteAddr().String()
	}

	defer func() {
		from.Close()
		to.Close()
		done <- struct{}{}
	}()

	var n int
	var err error
	var fm *MessageReader
	var tm *MessageWriter

	dr := minDuGtZero(p.ReadTimeout, p.WriteTimeout)
	dw := p.WriteTimeout
	for {
		if dr > 0 {
			from.SetReadDeadline(time.Now().Add(dr))
		}
		if dw > 0 {
			to.SetWriteDeadline(time.Now().Add(dw))
		}

		fm, err = from.ReadMessage()
		if err != nil {
			if err != io.EOF && !from.IsClose() {
				xlog.Errorf("ReadMessage from [%v] fail: %v", fromaddr, err)
			}
			xlog.Debugf("ReadMessage from [%v] fail: %v", fromaddr, err)
			return
		}

		tm, err = to.NewMessage(fm.frame.OpCode)
		if err != nil {
			xlog.Errorf("NewMessage to [%v] fail: %v", toaddr, err)
			return
		}

		for {
			n, err = fm.Read(buf)
			if err != nil {
				if err != io.EOF {
					xlog.Errorf("Read from [%v] fail: %v", fromaddr, err)
					return
				}
				xlog.Debugf("Read from [%v] fail: %v", fromaddr, err)
				err = tm.Close()
				if err != nil {
					xlog.Errorf("Close Write to [%v] fail: %v", toaddr, err)
					return
				}
				break
			}
			// xlog.Debugf("write websocket: %s", buf[0:n])
			n, err = tm.Write(buf[0:n])
			if err != nil {
				xlog.Errorf("Write to [%v] fail: %v", toaddr, err)
				return
			}
		}
	}
}
*/
