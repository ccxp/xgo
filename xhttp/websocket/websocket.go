package websocket

// This file implements a protocol of hybi draft.
// http://tools.ietf.org/html/draft-ietf-hybi-thewebsocketprotocol-17

import (
	"bufio"
	//"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	closeStatusProtocolError     = 1002
	maxControlFramePayloadLength = 125
)

const (
	ProtocolVersionHybi13    = 13
	ProtocolVersionHybi      = ProtocolVersionHybi13
	SupportedProtocolVersion = "13"
)

// ProtocolError represents WebSocket protocol errors.
type ProtocolError struct {
	ErrorString string
}

func (err *ProtocolError) Error() string { return err.ErrorString }

var (
	ErrBadStatus             = &ProtocolError{"bad status"}
	ErrBadUpgrade            = &ProtocolError{"missing or bad upgrade"}
	ErrBadWebSocketProtocol  = &ProtocolError{"missing or bad WebSocket-Protocol"}
	ErrBadWebSocketVersion   = &ProtocolError{"missing or bad WebSocket Version"}
	ErrChallengeResponse     = &ProtocolError{"mismatch challenge/response"}
	ErrBadFrame              = &ProtocolError{"bad frame"}
	ErrNotWebSocket          = &ProtocolError{"not websocket protocol"}
	ErrBadRequestMethod      = &ProtocolError{"bad method"}
	ErrNotSupported          = &ProtocolError{"not supported"}
	ErrBadMaskingKey         = &ProtocolError{"bad masking key"}
	ErrUnsupportedExtensions = &ProtocolError{"unsupported extensions"}
)

const (
	ContinuationFrame byte = 0
	TextFrame              = 1
	BinaryFrame            = 2
	CloseFrame             = 8
	PingFrame              = 9
	PongFrame              = 10
	UnknownFrame           = 255
)

type FrameReader struct {
	Fin        bool
	Rsv        [3]bool
	OpCode     byte
	Length     int64
	MaskingKey []byte

	// 从Conn读取数据时，读取到的Header完整数据
	Header []byte
	// 调用Read接口时，是否要unmask
	unmaskPayload bool

	reader io.Reader
	pos    int
}

// read payload
func (frame *FrameReader) Read(msg []byte) (n int, err error) {
	n, err = frame.reader.Read(msg)
	if frame.MaskingKey != nil && frame.unmaskPayload {
		for i := 0; i < n; i++ {
			msg[i] = msg[i] ^ frame.MaskingKey[frame.pos%4]
			frame.pos++
		}
	}
	return n, err
}

// NewFrameReader reads a frame header from the connection, and creates new reader for the frame.
// See Section 5.2 Base Framing protocol for detail.
// http://tools.ietf.org/html/draft-ietf-hybi-thewebsocketprotocol-17#section-5.2
//
// checkMask: 检查mask是否存在，对于server端，来自client端的frame应该都有mask.
func ReadFrame(br *bufio.Reader, unmaskPayload, checkMask bool) (frame *FrameReader, err error) {
	frame = &FrameReader{unmaskPayload: unmaskPayload}

	header := make([]byte, 0, 8)
	var b byte

	// First byte. FIN/RSV1/RSV2/RSV3/OpCode(4bits)
	b, err = br.ReadByte()
	if err != nil {
		return
	}
	header = append(header, b)
	frame.Fin = ((header[0] >> 7) & 1) != 0
	for i := 0; i < 3; i++ {
		j := uint(6 - i)
		frame.Rsv[i] = ((header[0] >> j) & 1) != 0
	}
	frame.OpCode = header[0] & 0x0f

	// Second byte. Mask/Payload len(7bits)
	b, err = br.ReadByte()
	if err != nil {
		return
	}
	header = append(header, b)
	mask := (b & 0x80) != 0
	b &= 0x7f
	lengthFields := 0
	switch {
	case b <= 125: // Payload length 7bits.
		frame.Length = int64(b)
	case b == 126: // Payload length 7+16bits
		lengthFields = 2
	case b == 127: // Payload length 7+64bits
		lengthFields = 8
	}
	for i := 0; i < lengthFields; i++ {
		b, err = br.ReadByte()
		if err != nil {
			return
		}
		if lengthFields == 8 && i == 0 { // MSB must be zero when 7+64 bits
			b &= 0x7f
		}
		header = append(header, b)
		frame.Length = frame.Length*256 + int64(b)
	}
	if mask {
		// Masking key. 4 bytes.
		for i := 0; i < 4; i++ {
			b, err = br.ReadByte()
			if err != nil {
				return
			}
			header = append(header, b)
			frame.MaskingKey = append(frame.MaskingKey, b)
		}
	} else if checkMask {
		return nil, ErrBadMaskingKey
	}
	frame.Header = header
	frame.reader = io.LimitReader(br, frame.Length)

	return
}

type MessageReader struct {
	OpCode byte // 第一个分片上的opcode，消息类型

	br        *bufio.Reader
	checkMask bool
	frame     *FrameReader
	err       error
}

// 读一个message，注意message是分片的。
//
// 分片时规则如下：
//
//	FIN=0，opcode=0x1，表示发送的是文本类型，且消息还没发送完成，还有后续的数据帧。
//	FIN=0，opcode=0x0，表示消息还没发送完成，还有后续的数据帧，当前的数据帧需要接在上一条数据帧之后。
//	FIN=1，opcode=0x0，表示消息已经发送完成，没有后续的数据帧，当前的数据帧需要接在上一条数据帧之后。服务端可以将关联的数据帧组装成完整的消息。
//
func ReadMessage(br *bufio.Reader, checkMask bool) (m *MessageReader, err error) {
	m = &MessageReader{
		br:        br,
		checkMask: checkMask,
	}
	var opcode byte
	_, opcode, err = m.PeekFrame()
	if err != nil {
		return nil, err
	}
	if opcode == ContinuationFrame {
		return nil, ErrBadFrame
	}

	m.frame, m.err = ReadFrame(br, true, checkMask)
	if m.err != nil {
		return nil, m.err
	}
	m.OpCode = m.frame.OpCode
	return
}

func (m *MessageReader) PeekFrame() (fin bool, opcode byte, err error) {
	var b []byte

	b, err = m.br.Peek(1)
	if err != nil {
		return
	}
	fin = ((b[0] >> 7) & 1) != 0
	opcode = b[0] & 0x0f
	return
}

func (m *MessageReader) Read(msg []byte) (n int, err error) {
	if m.err != nil {
		return 0, m.err
	}

	n, err = m.frame.Read(msg)
	for err == io.EOF {
		if m.frame.Fin {
			m.err = err
			return
		}

		var opcode byte
		_, opcode, m.err = m.PeekFrame()
		if m.err != nil {
			return 0, m.err
		}
		if opcode != ContinuationFrame {
			return 0, ErrBadFrame
		}

		m.frame, m.err = ReadFrame(m.br, true, m.checkMask)
		if m.err != nil {
			return 0, m.err
		}
		n, err = m.frame.Read(msg)
	}

	return n, err
}

// A HybiFrameWriter is a writer for hybi frame.
type FrameWriter struct {
	Fin    bool
	Rsv    [3]bool
	OpCode byte
	// 只能是0或者4个字节，server返回的数据按协议不需要有mask
	MaskingKey []byte

	Writer *bufio.Writer
}

func (frame *FrameWriter) Write(msg []byte) (n int, err error) {
	if len(frame.MaskingKey) != 0 && len(frame.MaskingKey) != 4 {
		return 0, ErrBadMaskingKey
	}
	lengthFields := 0
	length := len(msg)
	switch {
	case length <= 125:
	case length < 65536:
		lengthFields = 2
	default:
		lengthFields = 8
	}

	header := make([]byte, 0, 2+lengthFields+len(frame.MaskingKey))
	var b byte
	if frame.Fin {
		b |= 0x80
	}
	for i := 0; i < 3; i++ {
		if frame.Rsv[i] {
			j := uint(6 - i)
			b |= 1 << j
		}
	}
	b |= frame.OpCode
	header = append(header, b)
	if frame.MaskingKey != nil {
		b = 0x80
	} else {
		b = 0
	}
	switch {
	case length <= 125:
		b |= byte(length)
	case length < 65536:
		b |= 126
	default:
		b |= 127
	}
	header = append(header, b)
	for i := 0; i < lengthFields; i++ {
		j := uint((lengthFields - i - 1) * 8)
		b = byte((length >> j) & 0xff)
		header = append(header, b)
	}
	if len(frame.MaskingKey) > 0 {
		header = append(header, frame.MaskingKey...)
		frame.Writer.Write(header)
		for i := range msg {
			b = msg[i] ^ frame.MaskingKey[i%4]
			err = frame.Writer.WriteByte(b)
			if err != nil {
				return 0, err
			}
		}
		err = frame.Writer.Flush()
		return length, err
	}
	frame.Writer.Write(header)
	frame.Writer.Write(msg)
	err = frame.Writer.Flush()
	return length, err
}

type MessageWriter struct {
	frame FrameWriter
	buf   []byte
	pos   int
	fin   bool
	err   error
}

// 生成一个新的MessageWriter.
//
// frameType: 写入到frame的opcode.
// needMaskingKey: 需不需要maskkey，client端需要，srv端不需要。
func NewMessageWriter(bw *bufio.Writer, frameType byte, needMaskingKey bool) (message *MessageWriter, err error) {
	message = &MessageWriter{
		frame: FrameWriter{
			OpCode: frameType,
			Writer: bw,
		},
	}

	if needMaskingKey {
		message.frame.MaskingKey, err = generateMaskingKey()
		if err != nil {
			return nil, err
		}
	}
	return message, nil
}

// 设置buffer，如果没有设置，则在Write时生成一个32k的buffer。buffer大小影响分片的大小，也就是默认最大分片是32k
func (message *MessageWriter) SetBuffer(buf []byte) {
	message.buf = buf
}

// 设置分片的Rsv字段，注意Rsv只有三个字。
func (message *MessageWriter) SetRsv(i int, flag bool) {
	if i >= 0 && i < 3 {
		message.frame.Rsv[i] = flag
	}
}

// 写Message，写满buffer则生成一个分片，最后需要调用Close，Close时不管有没有数据都会生成一个fin分片。
func (message *MessageWriter) Write(msg []byte) (n int, err error) {
	if message.buf == nil {
		message.buf = make([]byte, 32<<10)
	}

	idx := 0
	for message.pos+len(msg)-idx >= len(message.buf) {
		n = copy(message.buf[message.pos:], msg[idx:])
		idx += n
		message.pos += n
		err = message.flush(false)
		if err != nil {
			return 0, err
		}
	}
	message.pos += copy(message.buf[message.pos:], msg[idx:])
	return len(msg), nil
}

func (message *MessageWriter) Flush() (err error) {
	return message.flush(false)
}

func (message *MessageWriter) Close() (err error) {
	return message.flush(true)
}

func (message *MessageWriter) flush(fin bool) (err error) {
	if message.err != nil {
		return message.err
	}
	if message.fin {
		if message.pos == 0 {
			return nil
		}
		return io.ErrClosedPipe
	}
	if message.pos == 0 && !fin {
		return nil
	}
	if fin && message.buf == nil {
		message.buf = make([]byte, 0)
	}
	message.fin = fin
	message.frame.Fin = fin
	_, err = message.frame.Write(message.buf[0:message.pos])
	message.pos = 0
	if err == nil {
		message.frame.OpCode = ContinuationFrame
	} else {
		message.err = err
	}
	return
}

type config struct {
	// A WebSocket server address.
	Location *url.URL

	// A Websocket client origin.
	Origin *url.URL

	// WebSocket subprotocols.
	Protocol []string

	// WebSocket protocol version.
	Version int
}

type Conn struct {
	br *bufio.Reader
	bw *bufio.Writer

	conn net.Conn
	rwc  io.ReadWriteCloser

	isServer bool

	config config
	accept []byte

	isClose bool

	// ReadPingPong=true时，不自动处理CloseFrame、PingFrame、PongFrame，
	// ReadMessage会返回ping/pong信息。
	ReadPingPong bool
}

func (c *Conn) Close() error {
	c.isClose = true
	if c.conn != nil {
		return c.conn.Close()
	}
	return c.rwc.Close()
}

func (c *Conn) IsClose() bool {
	return c.isClose
}

// 只限于用xhttp.Transport时才支持
func (c *Conn) SetDeadline(t time.Time) error {
	if c.conn != nil {
		return c.conn.SetDeadline(t)
	}
	return nil
}

// 只限于用xhttp.Transport时才支持
func (c *Conn) SetReadDeadline(t time.Time) error {
	if c.conn != nil {
		return c.conn.SetReadDeadline(t)
	}
	return nil
}

// 只限于用xhttp.Transport时才支持
func (c *Conn) SetWriteDeadline(t time.Time) error {
	if c.conn != nil {
		return c.conn.SetWriteDeadline(t)
	}
	return nil
}

func (c *Conn) NewMessage(frameType byte) (message *MessageWriter, err error) {
	return NewMessageWriter(c.bw, frameType, !c.isServer)
}

// 读取message.
//
// 注意只能在上一个message读取eof后再读下一个message。
// 自动处理CloseFrame、PingFrame、PongFrame。
// 读到CloseFrame时关闭连接，返回eof
func (c *Conn) ReadMessage() (m *MessageReader, err error) {

	for {
		m, err = ReadMessage(c.br, c.isServer)
		if err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				c.WriteClose(closeStatusProtocolError)
			}
			return nil, err
		}

		switch m.OpCode {
		case ContinuationFrame:
			return nil, ErrBadFrame
		case TextFrame, BinaryFrame:
			return m, nil
		case CloseFrame:
			c.Close()
			return nil, io.EOF
		case PingFrame, PongFrame:
			if c.ReadPingPong {
				return m, nil
			}

			b := make([]byte, maxControlFramePayloadLength)
			n, err := io.ReadFull(m, b)
			if err != nil {
				if err != io.EOF && err != io.ErrUnexpectedEOF {
					c.WriteClose(closeStatusProtocolError)
				}
				return nil, err
			}
			if m.OpCode == PingFrame {
				if _, err := c.WritePong(b[:n]); err != nil {
					if err != io.EOF && err != io.ErrUnexpectedEOF {
						c.WriteClose(closeStatusProtocolError)
					}
					return nil, err
				}
			}
			continue
		default:
			io.Copy(ioutil.Discard, m)
			c.WriteClose(closeStatusProtocolError)
			return nil, ErrBadFrame
		}
	}
}

func (c *Conn) WriteClose(status int) (err error) {

	m, e := NewMessageWriter(c.bw, CloseFrame, !c.isServer)
	if e != nil {
		return e
	}

	msg := make([]byte, 2)
	binary.BigEndian.PutUint16(msg, uint16(status))
	_, err = m.Write(msg)
	if err == nil {
		m.Close()
	}
	err2 := c.Close()
	if err2 != nil && err == nil {
		err = err2
	}
	return err
}

func (c *Conn) WritePong(msg []byte) (n int, err error) {
	m, e := NewMessageWriter(c.bw, PongFrame, !c.isServer)
	if e != nil {
		return 0, e
	}

	n, err = m.Write(msg)
	err2 := m.Close()
	if err2 != nil && err == nil {
		err = err2
	}
	return n, err
}

func (c *Conn) WritePing(msg []byte) (n int, err error) {
	m, e := NewMessageWriter(c.bw, PingFrame, !c.isServer)
	if e != nil {
		return 0, e
	}
	n, err = m.Write(msg)
	err2 := m.Close()
	if err2 != nil && err == nil {
		err = err2
	}
	return n, err
}

// 直接从连接中读出原始数据
func (c *Conn) Read(b []byte) (int, error) {
	return c.br.Read(b)
}

// 直接向连接中写入原始数据，后面需要调用Flush。
func (c *Conn) Write(b []byte) (int, error) {
	return c.bw.Write(b)
}

// 直接向连接中写入原始数据后需要调用Flush
func (c *Conn) Flush() error {
	return c.bw.Flush()
}

func NewServerConn(w http.ResponseWriter, r *http.Request) (c *Conn, err error) {
	rwc, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return nil, err
	}

	c = &Conn{
		br:       buf.Reader,
		bw:       buf.Writer,
		conn:     rwc,
		isServer: true,
	}

	code, err := c.serverReadHandshake(buf.Reader, r)
	if err == ErrBadWebSocketVersion {
		fmt.Fprintf(buf, "HTTP/1.1 %03d %s\r\n", code, http.StatusText(code))
		fmt.Fprintf(buf, "Sec-WebSocket-Version: %s\r\n", SupportedProtocolVersion)
		buf.WriteString("\r\n")
		buf.WriteString(err.Error())
		buf.Flush()
		return
	}
	if err != nil {
		fmt.Fprintf(buf, "HTTP/1.1 %03d %s\r\n", code, http.StatusText(code))
		buf.WriteString("\r\n")
		buf.WriteString(err.Error())
		buf.Flush()
		return
	}
	err = c.serverAcceptHandshake(buf.Writer)
	if err != nil {
		code = http.StatusBadRequest
		fmt.Fprintf(buf, "HTTP/1.1 %03d %s\r\n", code, http.StatusText(code))
		buf.WriteString("\r\n")
		buf.Flush()
		return
	}

	rwc.SetDeadline(time.Time{})

	return c, nil
}

func (c *Conn) serverReadHandshake(buf *bufio.Reader, req *http.Request) (code int, err error) {
	if req.Method != "GET" {
		return http.StatusMethodNotAllowed, ErrBadRequestMethod
	}

	// xlog.Debugf("%v", req.Header)
	// HTTP version can be safely ignored.
	if strings.ToLower(req.Header.Get("Upgrade")) != "websocket" ||
		!strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade") {
		return http.StatusBadRequest, ErrNotWebSocket
	}

	key := req.Header.Get("Sec-Websocket-Key")
	if key == "" {
		return http.StatusBadRequest, ErrChallengeResponse
	}
	version := req.Header.Get("Sec-Websocket-Version")
	switch version {
	case SupportedProtocolVersion:
		c.config.Version = ProtocolVersionHybi13
	default:
		return http.StatusBadRequest, ErrBadWebSocketVersion
	}
	var scheme string
	if req.TLS != nil {
		scheme = "wss"
	} else {
		scheme = "ws"
	}
	c.config.Location, err = url.ParseRequestURI(scheme + "://" + req.Host + req.URL.RequestURI())
	if err != nil {
		return http.StatusBadRequest, err
	}
	protocol := strings.TrimSpace(req.Header.Get("Sec-Websocket-Protocol"))
	if protocol != "" {
		protocols := strings.Split(protocol, ",")
		for i := 0; i < len(protocols); i++ {
			c.config.Protocol = append(c.config.Protocol, strings.TrimSpace(protocols[i]))
		}
	}
	c.accept, err = getNonceAccept([]byte(key))
	if err != nil {
		return http.StatusInternalServerError, err
	}
	return http.StatusSwitchingProtocols, nil
}

func (c *Conn) serverAcceptHandshake(buf *bufio.Writer) (err error) {
	if len(c.config.Protocol) > 0 {
		if len(c.config.Protocol) != 1 {
			// You need choose a Protocol in Handshake func in Server.
			return ErrBadWebSocketProtocol
		}
	}
	buf.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	buf.WriteString("Upgrade: websocket\r\n")
	buf.WriteString("Connection: Upgrade\r\n")
	buf.WriteString("Sec-WebSocket-Accept: " + string(c.accept) + "\r\n")
	if len(c.config.Protocol) > 0 {
		buf.WriteString("Sec-WebSocket-Protocol: " + c.config.Protocol[0] + "\r\n")
	}
	buf.WriteString("\r\n")
	c.accept = nil
	return buf.Flush()
}

// generateMaskingKey generates a masking key for a frame.
func generateMaskingKey() (maskingKey []byte, err error) {
	maskingKey = make([]byte, 4)
	if _, err = io.ReadFull(rand.Reader, maskingKey); err != nil {
		return
	}
	return
}

// getNonceAccept computes the base64-encoded SHA-1 of the concatenation of
// the nonce ("Sec-WebSocket-Key" value) with the websocket GUID string.
func getNonceAccept(nonce []byte) (expected []byte, err error) {
	h := sha1.New()
	if _, err = h.Write(nonce); err != nil {
		return
	}
	if _, err = h.Write([]byte(websocketGUID)); err != nil {
		return
	}
	expected = make([]byte, 28)
	base64.StdEncoding.Encode(expected, h.Sum(nil))
	return
}

// 生成websocket请求。
//
// origin是必须的。
func NewRequest(url, origin string) (req *http.Request, err error) {

	req, err = http.NewRequest("GET", url, nil)
	if err != nil {
		return
	}

	if strings.HasPrefix(url, "wss://") {
		req.URL.Scheme = "https"
	}

	req.Header.Set("Origin", strings.ToLower(origin))

	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")

	nonce := generateNonce()
	req.Header.Set("Sec-WebSocket-Key", string(nonce))
	req.Header.Set("Sec-WebSocket-Version", SupportedProtocolVersion)

	return
}

func NewClientConn(req *http.Request, resp *http.Response) (c *Conn, err error) {

	if resp.StatusCode != 101 {
		return nil, ErrBadStatus
	}
	if strings.ToLower(resp.Header.Get("Upgrade")) != "websocket" ||
		strings.ToLower(resp.Header.Get("Connection")) != "upgrade" {
		return nil, ErrBadUpgrade
	}
	expectedAccept, err := getNonceAccept([]byte(req.Header.Get("Sec-WebSocket-Key")))
	if err != nil {
		return nil, err
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != string(expectedAccept) {
		return nil, ErrChallengeResponse
	}
	if resp.Header.Get("Sec-WebSocket-Extensions") != "" {
		return nil, ErrUnsupportedExtensions
	}

	if conn, ok := resp.Body.(net.Conn); ok {
		c = &Conn{
			br:   bufio.NewReader(conn),
			bw:   bufio.NewWriter(conn),
			conn: conn,
		}
	} else if rwc, ok := resp.Body.(io.ReadWriteCloser); ok {
		c = &Conn{
			br:  bufio.NewReader(rwc),
			bw:  bufio.NewWriter(rwc),
			rwc: rwc,
		}
	} else {
		return nil, ErrNotSupported
	}
	return

}

// generateNonce generates a nonce consisting of a randomly selected 16-byte
// value that has been base64-encoded.
func generateNonce() (nonce []byte) {
	key := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		panic(err)
	}
	nonce = make([]byte, 24)
	base64.StdEncoding.Encode(nonce, key)
	return
}
