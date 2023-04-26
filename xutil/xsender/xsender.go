package xsender

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ccxp/xgo/xlog"
	"github.com/ccxp/xgo/xutil/xfile"
	"github.com/ccxp/xgo/xutil/xfilemq"
	"github.com/pierrec/lz4"
)

// DataSender用的压缩参数
const (
	CompressNone = iota
	CompressLZ4
)

// send类型
/*const (
	senderApp = iota
	senderSnap
	senderMsg
)*/

const (
	checksumHeader byte = 0xef
	checksumBody        = 0xf1
	checksumTail        = 1 << 7
)

var (
	errBadStatus        = errors.New("bad status")
	errBadUpgrade       = errors.New("missing or bad upgrade")
	errConnNotSupported = errors.New("conn not supported")
	errDataChecksum     = errors.New("data checksum err")
	errNodeClosed       = errors.New("node is closed")
)

const (
	offsetCheckSumHeader        = 0
	offsetMsgType               = 1
	offsetCompressType          = 2
	offsetDataLen               = 3
	offsetDataLenBeforeCompress = 7
	offsetCheckSumBody          = 11
)

const (
	dataMsg      = 0
	heartbeatMsg = 1
)

/*****************/

/*****************/

type httpRoundTriperSendConn struct {
	sect int
	addr string

	w    *bufio.Writer
	conn net.Conn
	rwc  io.ReadWriteCloser

	checksum     byte
	useHeartbeat bool
	// readCloseCh  chan struct{}
}

func (c *httpRoundTriperSendConn) Close() {

	if c.rwc != nil {
		c.rwc.Close()
	} else {
		c.conn.Close()
	}
}

type httpRoundTriperRecvConn struct {
	conn net.Conn
}

func (c *httpRoundTriperRecvConn) Close() {
	c.conn.Close()
}

type httpRoundTriperSenderKey struct {
	Sect int
	Addr string
}

type retryData struct {
	b   []byte
	res chan bool
}
type httpRoundTriperSender struct {
	ch       chan []byte
	retrych  chan retryData
	retryLog *xfilemq.FileMQ
}

// 异步发raft消息的队列，在一个Node节点内部使用.
type DataSender struct {

	// 取得目标地址列表
	GetAddrs func(sect int) []string
	// 检查addr能不能同步数据
	IsAddrOK func(addr string) (ok bool, retry bool)
	// 生成建立连接的请求
	NewRequest func(sect int, addr string) (*http.Request, error)
	// 建立http连接的RoundTripper
	RoundTripper func(sect int, addr string, req *http.Request) (*http.Response, error)

	// svr端处理数据接口
	Step func(sect int, b []byte) error

	// client端写超时时间
	WriteTimeoutDuration time.Duration

	// svr端读超时时间
	ReadTimeoutDuration time.Duration

	// 读写回包的超时时间
	ReplyTimeoutDuration time.Duration

	// 心跳包发送间隔
	HeartbeatDuration time.Duration

	// 一个来源节点发送消息的缓冲大小 (msg个数)
	SendPoolSize int

	CompressType int

	// 错误回调
	TraceSendFail      func(sect int, addr string, e error)
	TraceStepFail      func(sect int, addr string, e error)
	TraceAddrNotOK     func(sect int, addr string, retry bool)
	TraceAddrQueueFull func(sect int, addr string)

	NoRetry              bool // 不重试，测试队列发送积压时也会丢弃
	RetryBinLogPath      string
	RetryTimeoutDuration time.Duration

	// 带缓存的写读io用
	RetryBinLogBufioReaderPool xfile.BufioReaderPool
	RetryBinLogBufferPool      xfile.BufferPool
	RetryBinLogBufioWriterPool xfile.BufioWriterPool

	// toAddr : sender
	senders   map[httpRoundTriperSenderKey]*httpRoundTriperSender
	sendersmu sync.RWMutex

	bufioWriterPool sync.Pool
	bufioReaderPool sync.Pool

	localAddr string

	closed  bool
	closeCh chan struct{}
	wg      sync.WaitGroup
}

func (h *DataSender) Init(localAddr string) error {

	h.closeCh = make(chan struct{})

	h.localAddr = localAddr
	xlog.Warnf("local addr %v", localAddr)
	h.senders = make(map[httpRoundTriperSenderKey]*httpRoundTriperSender)

	return nil
}

func (h *DataSender) Close() {

	if !h.closed {
		h.closed = true
		close(h.closeCh)

		h.wg.Wait()
	}
}

func (h *DataSender) getCompressType(t int) int {
	if t != CompressLZ4 {
		return CompressNone
	}
	return t
}

// 发消息入队
func (h *DataSender) SendEnqueue(sect int, b []byte) {

	addrs := h.GetAddrs(sect)
	for _, addr := range addrs {
		if addr == h.localAddr {
			continue
		}

		h.sendEnqueue(sect, b, addr)
	}
}

func (h *DataSender) sendEnqueue(sect int, b []byte, addr string) {

	// 这个函数是在ready里面，所以是串行的，里面处理不需要锁

	sender := h.getSender(sect, addr)
	if ok, retry := h.IsAddrOK(addr); !ok {

		retry = retry && !h.NoRetry

		if h.TraceAddrNotOK != nil {
			h.TraceAddrNotOK(sect, addr, retry)
		}

		if retry {
			xlog.Warnf("addr %v is not ok, save to retrylog", addr)
			sender.retryLog.Save(b)
		} else {
			xlog.Errorf("addr %v is not ok, discard", addr)
		}
		return
	}

	select {
	case sender.ch <- b:
	default:

		if h.TraceAddrQueueFull != nil {
			h.TraceAddrQueueFull(sect, addr)
		}

		if !h.NoRetry {
			xlog.Warnf("addr %v sender queue full, save to retrylog", addr)
			sender.retryLog.Save(b)
		} else {
			xlog.Errorf("addr %v sender queue full, discard", addr)
		}
	}
}

func (h *DataSender) getSender(sect int, addr string) *httpRoundTriperSender {
	k := httpRoundTriperSenderKey{sect, addr}

	h.sendersmu.RLock()
	res, ok := h.senders[k]
	h.sendersmu.RUnlock()
	if ok {
		return res
	}

	h.sendersmu.Lock()
	defer h.sendersmu.Unlock()

	res, ok = h.senders[k]
	if ok {
		return res
	}

	res = &httpRoundTriperSender{
		ch: make(chan []byte, h.SendPoolSize),
	}

	if !h.NoRetry {
		res.retrych = make(chan retryData)
		res.retryLog = &xfilemq.FileMQ{
			DataPath:            h.RetryBinLogPath,
			FilePrefix:          fmt.Sprintf("%v_%v", strings.ReplaceAll(addr, ":", "_"), sect),
			FileSplitDuration:   time.Minute,
			FileReserveDuration: h.RetryTimeoutDuration,
			LogStatDuration:     time.Second,

			// 带缓存的写读io用
			BufioReaderPool: h.RetryBinLogBufioReaderPool,
			BufferPool:      h.RetryBinLogBufferPool,
			BufioWriterPool: h.RetryBinLogBufioWriterPool,
		}

		res.retryLog.Init()
		go h.loopRetry(res)
	}

	go h.loopSender(sect, addr, res)

	h.senders[k] = res
	return res
}

func (h *DataSender) loopSender(sect int, addr string, sender *httpRoundTriperSender) {

	h.wg.Add(1)
	defer h.wg.Done()

	/*
		数据格式是  header、 data、tailMagic

		header: 12 byte

			1byte: checksumHeader
			1byte: msgType
			1byte: compressType
			4byte: data len, BigEndian
			4byte: data len before compress, BigEndian, is compressType > 0
			1byte: checksumBody

		data

		tailMagic: 1byte


		const (
			offsetCheckSumHeader        = 0
			offsetMsgType               = 1
			offsetCompressType          = 2
			offsetDataLen               = 3
			offsetDataLenBeforeCompress = 7
			offsetCheckSumBody          = 11
		)
	*/
	var body []byte
	header := make([]byte, 12)
	header[offsetCheckSumHeader] = checksumHeader
	header[offsetCheckSumBody] = checksumBody

	compressType := h.getCompressType(h.CompressType)

	var c *httpRoundTriperSendConn
	var e error
	var rd retryData

	var heartbeatTicker *time.Ticker
	if h.HeartbeatDuration > 0 {
		heartbeatTicker = time.NewTicker(h.HeartbeatDuration)
		defer heartbeatTicker.Stop()
	} else {
		heartbeatTicker = &time.Ticker{}
	}

	var lastSendTime, curr time.Time
	for {
		select {
		case body = <-sender.ch:
			lastSendTime = time.Now()
			c, e = h.send(compressType, sect, addr, body, header, c, dataMsg)
			if e != nil {
				xlog.Errorf("send to %v %v fail: %v", sect, addr, e)
				if h.TraceSendFail != nil {
					h.TraceSendFail(sect, addr, e)
				}
				if !h.NoRetry {
					sender.retryLog.Save(body)
				}
				h.sleep(time.Millisecond * 50)
			}
		case rd = <-sender.retrych:
			lastSendTime = time.Now()
			c, e = h.send(compressType, sect, addr, rd.b, header, c, dataMsg)
			if e != nil {
				rd.res <- false

				xlog.Errorf("retry send to %v %v fail: %v", sect, addr, e)
				if h.TraceSendFail != nil {
					h.TraceSendFail(sect, addr, e)
				}

				h.sleep(time.Millisecond * 50)
			} else {
				rd.res <- true
			}

		case <-heartbeatTicker.C:
			curr = time.Now()
			if int64(curr.Sub(lastSendTime)) > int64(h.HeartbeatDuration)/2 {
				lastSendTime = curr
				c, e = h.send(CompressNone, sect, addr, nil, header, c, heartbeatMsg)
				if e != nil {
					xlog.Errorf("send heartbeat to %v %v fail: %v", sect, addr, e)
					if h.TraceSendFail != nil {
						h.TraceSendFail(sect, addr, e)
					}
				}
			}

		case <-h.closeCh:
			return
		}

	}
}

func (h *DataSender) sleep(du time.Duration) {
	t := time.NewTimer(du)
	defer t.Stop()
	select {
	case <-t.C:
	case <-h.closeCh:
	}
}

func (h *DataSender) loopRetry(sender *httpRoundTriperSender) {

	h.wg.Add(1)
	defer func() {
		sender.retryLog.Close()
		h.wg.Done()
	}()

	var b []byte
	res := make(chan bool)
	var ok bool
	for !h.closed {
		b = sender.retryLog.Next()
		if b == nil {
			h.sleep(time.Second * 2)
			continue
		}

		if len(b) == 0 {
			sender.retryLog.MarkSucc()
			continue
		}

		sender.retrych <- retryData{b, res}
		ok = <-res
		if ok {
			sender.retryLog.MarkSucc()
		} else {
			h.sleep(time.Second * 2)
		}

	}
}

func (h *DataSender) writeto(header, body []byte, c *httpRoundTriperSendConn) (e error) {

	if c.conn != nil && h.WriteTimeoutDuration > 0 {
		e = c.conn.SetWriteDeadline(time.Now().Add(h.WriteTimeoutDuration))
		if e != nil {
			return e
		}
	}

	_, e = c.w.Write(header)
	if e != nil {
		return e
	}

	if len(body) > 0 {
		_, e = c.w.Write(body)
		if e != nil {
			return e
		}
	}

	checksum := make([]byte, 1)
	c.checksum++
	if c.checksum >= checksumTail {
		c.checksum = 1
	}
	checksum[0] = checksumTail | c.checksum
	_, e = c.w.Write(checksum)
	if e != nil {
		return e
	}

	e = c.w.Flush()
	if e != nil {
		return e
	}
	if c.conn != nil && h.WriteTimeoutDuration > 0 {
		c.conn.SetWriteDeadline(time.Time{})
	}

	// 读结果
	if c.conn != nil {
		if h.ReplyTimeoutDuration > 0 {
			e = c.conn.SetReadDeadline(time.Now().Add(h.ReplyTimeoutDuration))
			if e != nil {
				return e
			}
		}

		n, e := c.conn.Read(checksum)
		if e != nil {
			return e
		}
		if n != 1 || checksum[0] != c.checksum {
			return fmt.Errorf("read checksum fail: n:%v, checksum %v, res %v", n, c.checksum, checksum)
		}

		if h.ReplyTimeoutDuration > 0 {
			c.conn.SetReadDeadline(time.Time{})
		}
	} else {
		n, e := c.rwc.Read(checksum)
		if e != nil {
			return e
		}
		if n != 1 || checksum[0] != c.checksum {
			return fmt.Errorf("read checksum fail: n:%v, checksum %v, res %v", n, c.checksum, checksum)
		}
	}

	return nil
}

func (h *DataSender) send(compressType, sect int, addr string, body, header []byte, c *httpRoundTriperSendConn, msgType byte) (*httpRoundTriperSendConn, error) {

	/*
		数据格式是  header、 data、tailMagic

		header: 12 byte

			1byte: checksumHeader
			1byte: msgType
			1byte: compressType
			4byte: data len, BigEndian
			4byte: data len before compress, BigEndian, is compressType > 0
			1byte: checksumBody

		data

		tailMagic: 1byte


		const (
			offsetCheckSumHeader        = 0
			offsetMsgType               = 1
			offsetCompressType          = 2
			offsetDataLen               = 3
			offsetDataLenBeforeCompress = 7
			offsetCheckSumBody          = 11
		)
	*/

	var d []byte

	header[offsetMsgType] = msgType

	if len(body) < 128 {
		compressType = CompressNone
	}

	if compressType > CompressNone {

		header[offsetCompressType] = byte(compressType)
		binary.BigEndian.PutUint32(header[offsetDataLenBeforeCompress:offsetDataLenBeforeCompress+4], uint32(len(body)))

		n := lz4.CompressBlockBound(len(body))
		d = make([]byte, n)
		n, e := lz4.CompressBlock(body, d, nil)
		if e != nil {
			xlog.Errorf("CompressBlock fail: %v", e)
			return c, e
		}
		d = d[0:n]
		binary.BigEndian.PutUint32(header[offsetDataLen:offsetDataLen+4], uint32(n))

		// xlog.Errorf("send %v %v %v %v", header[offsetMsgType], header[offsetCompressType], n, len(info.data))

	} else {
		header[offsetCompressType] = byte(CompressNone)
		binary.BigEndian.PutUint32(header[offsetDataLen:offsetDataLen+4], uint32(len(body)))
		binary.BigEndian.PutUint32(header[offsetDataLenBeforeCompress:offsetDataLenBeforeCompress+4], 0)
		d = body

		// xlog.Errorf("send %v %v %v %v", header[offsetMsgType], header[offsetCompressType], len(info.data), 0)
	}

	var e error
	if c != nil {

		if !c.useHeartbeat && msgType == heartbeatMsg {
			return c, nil
		}

		e = h.writeto(header, d, c)
		if e == nil {
			return c, nil
		}
		c.Close()
		h.bufioWriterPool.Put(c.w)
		c = nil
		// 失败要重连一次
		xlog.Errorf("send fail: %v, reconnect", e)
	}

	c, e = h.dial(sect, addr)
	if e != nil {
		return nil, e
	}
	if !c.useHeartbeat && msgType == heartbeatMsg {
		return c, nil
	}

	e = h.writeto(header, d, c)
	if e == nil {
		return c, nil
	}
	c.Close()
	h.bufioWriterPool.Put(c.w)
	c = nil
	return nil, e
}

func isProtocolSwitch(r *http.Response) bool {
	return r.StatusCode == http.StatusSwitchingProtocols &&
		r.Header.Get("Upgrade") == "wegodataconn" &&
		r.Header.Get("Connection") == "Upgrade"
}

// client发消息建立连接
func (h *DataSender) dial(sect int, addr string) (c *httpRoundTriperSendConn, e error) {

	req, e := h.NewRequest(sect, addr)
	if e != nil {
		xlog.Errorf("NewRequest to %v %v fail: %v", sect, addr, e)
		return
	}

	req.Header.Set("X-HttpSender-Sect", strconv.Itoa(sect))

	req.Header.Set("Upgrade", "wegodataconn")
	req.Header.Set("Connection", "Upgrade")

	resp, e := h.RoundTripper(sect, addr, req)
	if e != nil {
		xlog.Errorf("RoundTrip to %v %v fail: %v", sect, addr, e)
		return
	}

	if !isProtocolSwitch(resp) {
		resp.Body.Close()
		e = errBadUpgrade
		xlog.Errorf("dail to %v, fail: %v %v %v", addr, e, resp.StatusCode, resp.Header)
		return
	}

	useHeartbeat := resp.Header.Get("X-Sender-Heartbeat") == "1"
	/*if !useHeartbeat {
		xlog.Debugf("no heartbeat")
	} else {
		xlog.Debugf("use heartbeat")
	}*/

	tmpw := h.bufioWriterPool.Get()
	var bw *bufio.Writer
	if tmpw == nil {
		bw = bufio.NewWriterSize(nil, 1<<16)
	} else {
		bw = tmpw.(*bufio.Writer)
	}

	if conn, ok := resp.Body.(net.Conn); ok {
		bw.Reset(conn)
		c = &httpRoundTriperSendConn{
			addr:         addr,
			w:            bw,
			conn:         conn,
			useHeartbeat: useHeartbeat,
		}
		conn.SetDeadline(time.Time{})
		conn.SetReadDeadline(time.Time{})
		conn.SetWriteDeadline(time.Time{})

		xlog.Debugf("dial to %v, is conn", addr)

	} else if rwc, ok := resp.Body.(io.ReadWriteCloser); ok {
		bw.Reset(rwc)

		c = &httpRoundTriperSendConn{
			w:            bw,
			rwc:          rwc,
			useHeartbeat: useHeartbeat,
		}

		xlog.Debugf("dial to %v", addr)

	} else {
		h.bufioWriterPool.Put(bw)

		resp.Body.Close()
		e = errConnNotSupported
		xlog.Errorf("dail to %v %v fail: %v", sect, addr, e)
		return
	}

	return c, nil
}

// svr端用的读消息用的处理
func (h *DataSender) ServReader(w http.ResponseWriter, req *http.Request) {

	sect, _ := strconv.Atoi(req.Header.Get("X-HttpSender-Sect"))
	from := req.RemoteAddr

	// HTTP version can be safely ignored.
	if strings.ToLower(req.Header.Get("Upgrade")) != "wegodataconn" ||
		!strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade") {

		http.Error(w, "400 Status Bad Request", http.StatusBadRequest)
		return
	}

	conn, brw, e := w.(http.Hijacker).Hijack()
	if e != nil {
		http.Error(w, "500 Status Internal Server Error", http.StatusInternalServerError)
		return
	}

	brw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	brw.WriteString("Upgrade: wegodataconn\r\n")
	brw.WriteString("Connection: Upgrade\r\n")
	brw.WriteString("X-Sender-Heartbeat: 1\r\n")
	brw.WriteString("\r\n")
	e = brw.Flush()
	if e != nil {
		xlog.Errorf("ServReader from %v %v, handsake fail: %v", sect, from, e)
		return
	}

	xlog.Debugf("ServReader from %v %v, handsake succ", sect, from)

	c := &httpRoundTriperRecvConn{
		conn: conn,
	}
	conn.SetDeadline(time.Time{})
	conn.SetReadDeadline(time.Time{})
	conn.SetWriteDeadline(time.Time{})

	var bw *bufio.Reader
	tmpbw := h.bufioReaderPool.Get()
	if tmpbw == nil {
		bw = bufio.NewReaderSize(c.conn, 1<<16)
	} else {
		bw = tmpbw.(*bufio.Reader)
		bw.Reset(conn)
	}

	var cnt, heartbeatCnt int
	defer func() {
		c.Close()
		h.bufioReaderPool.Put(bw)
		if e != nil {
			xlog.Errorf("ServReader from %v, succ %v, heartbeat %v, fail: %v", req.RemoteAddr, cnt, heartbeatCnt, e)
			return
		}
	}()

	header := make([]byte, 12)

	var l, cl uint32
	var b1, b2, b []byte
	var n int
	var checksum = make([]byte, 1)

	/*
		const (
				offsetCheckSumHeader        = 0
				offsetMsgType               = 1
				offsetCompressType          = 2
				offsetDataLen               = 3
				offsetDataLenBeforeCompress = 7
				offsetCheckSumBody          = 11
			)
	*/

	for {
		if h.ReadTimeoutDuration > 0 {
			e = conn.SetReadDeadline(time.Now().Add(h.ReadTimeoutDuration))
			if e != nil {
				return
			}
		}

		_, e = io.ReadFull(bw, header)
		if e != nil {
			xlog.Errorf("read header from %v, fail: %v", req.RemoteAddr, e)
			return
		}

		if header[offsetCheckSumHeader] != checksumHeader || header[offsetCheckSumBody] != checksumBody {
			e = errDataChecksum
			return
		}

		l = binary.BigEndian.Uint32(header[offsetDataLen : offsetDataLen+4])
		cl = binary.BigEndian.Uint32(header[offsetDataLenBeforeCompress : offsetDataLenBeforeCompress+4])

		if l > 1<<31 || cl > 1<<31 { // 2G
			e = fmt.Errorf("data length %v %v too large", l, cl)
			return
		}

		b1 = make([]byte, l+1)

		_, e = io.ReadFull(bw, b1)
		if e != nil {
			xlog.Errorf("read body from %v fail: %v", req.RemoteAddr, e)
			return
		}
		if b1[l]&checksumTail != checksumTail {
			e = errDataChecksum
			return
		}
		checksum[0] = b1[l] & 127

		if h.ReadTimeoutDuration > 0 {
			e = conn.SetReadDeadline(time.Time{})
			if e != nil {
				return
			}
		}

		// 回包
		if h.ReplyTimeoutDuration > 0 {
			e = c.conn.SetWriteDeadline(time.Now().Add(h.ReplyTimeoutDuration))
		}
		_, e = c.conn.Write(checksum)
		if e != nil {
			return
		}
		if h.ReplyTimeoutDuration > 0 {
			c.conn.SetWriteDeadline(time.Time{})
		}

		if header[offsetMsgType] == heartbeatMsg {
			heartbeatCnt++
			// xlog.Debugf("recv heartbeat")
			continue
		}

		switch int(header[offsetCompressType]) {
		case CompressNone:
			b = b1[0:l]
		case CompressLZ4:

			b2 = make([]byte, cl)
			n, e = lz4.UncompressBlock(b1[0:l], b2)
			if e != nil {
				xlog.Errorf("UncompressBlock fail: %v", e)
				return
			}
			if n != int(cl) {
				e = fmt.Errorf("UncompressBlock fail, %v != %v", n, cl)
				xlog.Errorf("UncompressBlock fail: %v", e)
				return
			}

			b = b2

		default:
			e = fmt.Errorf("compress type %v error", header[offsetCompressType])
			return
		}

		e = h.Step(sect, b)
		if e != nil {
			xlog.Errorf("Step error %v", e)
			if h.TraceStepFail != nil {
				h.TraceStepFail(sect, from, e)
			}
		}

		cnt++
	}
}
