package xnet

import (
	"context"
	"encoding/base64"
	"net"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ccxp/xgo/xutil/xctx"
	"golang.org/x/net/idna"
)

//---------------------------------------------Balancer

// ConnInfo 包含连接参数，Network + Addr 是连接池的map key.
type ConnInfo struct {
	// 连接参数，其中Network用于dial，Scheme为https时做tls handshake.
	Network, Scheme, Addr string

	// Host is used for https to handle tls handshake.
	Host string

	// 设置单个请求的读超时，> 0时生效，否则用回来Transport里设置的WriteRequestTimeout
	WriteRequestTimeout time.Duration
	// 设置单个请求的读超时，> 0时生效，否则用回来Transport里设置的ReadResponseTimeout
	ReadResponseTimeout time.Duration
}

type connectMethodKey struct {
	Network, Addr string
}

type connectMethod struct {
	connInfo *ConnInfo
	connKey  *connectMethodKey
	proxyURL *url.URL // nil for no proxy, else full proxy URL
	isProxy  bool
}

// scheme returns the first hop scheme: http, https, or socks5
func (cm *connectMethod) scheme() string {
	if cm.proxyURL != nil {
		return cm.proxyURL.Scheme
	}
	return cm.connInfo.Scheme
}

var portMap = map[string]string{
	"http":   "80",
	"https":  "443",
	"socks5": "1080",
}

// canonicalAddr returns url.Host but always with a ":port" suffix
func canonicalAddr(url *url.URL) string {
	addr := url.Hostname()
	if v, err := idnaASCII(addr); err == nil {
		addr = v
	}
	port := url.Port()
	if port == "" {
		port = portMap[url.Scheme]
	}
	return net.JoinHostPort(addr, port)
}

func hasPort(s string) bool { return strings.LastIndex(s, ":") > strings.LastIndex(s, "]") }

func idnaASCII(v string) (string, error) {
	// TODO: Consider removing this check after verifying performance is okay.
	// Right now punycode verification, length checks, context checks, and the
	// permissible character tests are all omitted. It also prevents the ToASCII
	// call from salvaging an invalid IDN, when possible. As a result it may be
	// possible to have two IDNs that appear identical to the user where the
	// ASCII-only version causes an error downstream whereas the non-ASCII
	// version does not.
	// Note that for correct ASCII IDNs ToASCII will only do considerably more
	// work, but it will not cause an allocation.
	if isASCII(v) {
		return v, nil
	}
	return idna.Lookup.ToASCII(v)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// addr returns the first hop "host:port" to which we need to TCP connect.
func (cm *connectMethod) addr() string {
	if cm.proxyURL != nil {
		return canonicalAddr(cm.proxyURL)
	}
	return cm.connInfo.Addr
}

// tlsHost returns the host name to match against the peer's
// TLS certificate.
func (cm *connectMethod) tlsHost() string {
	h := cm.connInfo.Addr
	if hasPort(h) {
		h = h[:strings.LastIndex(h, ":")]
	}
	return h
}

func (cm *connectMethod) Proxy() *url.URL {
	return cm.proxyURL
}

// See 2 (end of page 4) https://www.ietf.org/rfc/rfc2617.txt
// "To receive authorization, the client sends the userid and password,
// separated by a single colon (":") character, within a base64
// encoded string in the credentials."
// It is not meant to be urlencoded.
func basicAuth(username, password string) string {
	auth := username + ":" + password
	return base64.StdEncoding.EncodeToString([]byte(auth))
}

// proxyAuth returns the Proxy-Authorization header to set
// on requests, if applicable.
func (cm *connectMethod) proxyAuth() string {
	if cm.proxyURL == nil {
		return ""
	}
	if u := cm.proxyURL.User; u != nil {
		username := u.Username()
		password, _ := u.Password()
		return "Basic " + basicAuth(username, password)
	}
	return ""
}

type TraceGotConnInfo struct {
	// Conn is the connection that was obtained. It is owned by
	// the http.Transport and should not be read, written or
	// closed by users of ClientTrace.
	Conn net.Conn

	// Reused is whether this connection has been previously
	// used for another HTTP request.
	Reused bool

	// WasIdle is whether this connection was obtained from an
	// idle pool.
	WasIdle bool

	// IdleTime reports how long the connection was previously
	// idle, if WasIdle is true.
	IdleTime time.Duration

	TryCount int
	Err      error
	Duration time.Duration
}

type TraceResultInfo struct {
	TryCount int
	Err      error
	Duration time.Duration
	AddrUsed []string
}

type Balancer struct {
	// Get addr info, must not be nil.
	GetEndpoint func(ctx context.Context, req Request, addrUsed []string) (connInfo ConnInfo, err error)

	// GotConn is called after a connection is
	// obtained or connect to addr failed.
	// if connect to addr failed, err is not nil.
	StatConn func(req Request, connInfo *ConnInfo, gotConnInfo *TraceGotConnInfo)

	// StatRoundTrip is called after single roundtrip.
	// resp is nil if fail to read response.
	// transportInfo.Duration is duration ater get connection.
	StatRoundTrip func(req Request, resp Response, connInfo *ConnInfo, connState *ConnState, respInfo *TraceResultInfo)

	// PutIdleConn is called when the connection is returned to
	// the idle pool. If err is nil, the connection was
	// successfully returned to the idle pool. If err is non-nil,
	// it describes why not. PutIdleConn is not called if
	// connection reuse is disabled via Transport.DisableKeepAlives.
	// PutIdleConn is called before the caller's Response.Body.Close
	// call returns.
	// For HTTP/2, this hook is not currently used.
	PutIdleConn func(connInfo *ConnInfo, err error)
}

type Tracer struct {

	// 取连接信息失败时调用一次
	StatGetEndpointFail func(req Request, err error)

	// 连接后，成功或失败都调用一次
	StatConn func(req Request, connInfo *ConnInfo, gotConnInfo *TraceGotConnInfo)

	// 单次连接读写结果上报
	StatRoundTrip func(req Request, resp Response, connInfo *ConnInfo, connState *ConnState, respInfo *TraceResultInfo)

	// 单次RoundTrip（里面可能多次发送不能地址）的最终结果
	StatResult func(req Request, resp Response, respInfo *TraceResultInfo)

	// 以下是带xctx的版本，在开始和结束可以传递数据

	// RoundTrip进入后调用一次, xtcx可以存少量日志用数据
	StatRoundTripBegin func(req Request, ctxx *xctx.ValueCtx)
	// 单次RoundTrip（里面可能多次发送不能地址）的最终结果
	StatResultWithValue func(req Request, resp Response, respInfo *TraceResultInfo, ctxx *xctx.ValueCtx)
}

// 合并Balancer，其中GetEndpoint只取其中一个，以第一个为先。
func CombineBalancer(a *Balancer, b *Balancer) *Balancer {
	if a != nil && b == nil {
		return a
	}
	if a == nil && b != nil {
		return b
	}

	r := &Balancer{}
	if a.GetEndpoint != nil {
		r.GetEndpoint = a.GetEndpoint
	} else {
		r.GetEndpoint = b.GetEndpoint
	}

	if a.StatConn != nil || b.StatConn != nil {
		r.StatConn = func(req Request, connInfo *ConnInfo, gotConnInfo *TraceGotConnInfo) {
			if a.StatConn != nil {
				a.StatConn(req, connInfo, gotConnInfo)
			}
			if b.StatConn != nil {
				b.StatConn(req, connInfo, gotConnInfo)
			}
		}
	}

	if a.StatRoundTrip != nil || b.StatRoundTrip != nil {
		r.StatRoundTrip = func(req Request, resp Response, connInfo *ConnInfo, connState *ConnState, respInfo *TraceResultInfo) {
			if a.StatRoundTrip != nil {
				a.StatRoundTrip(req, resp, connInfo, connState, respInfo)
			}
			if b.StatRoundTrip != nil {
				b.StatRoundTrip(req, resp, connInfo, connState, respInfo)
			}
		}
	}

	if a.PutIdleConn != nil || b.PutIdleConn != nil {
		r.PutIdleConn = func(connInfo *ConnInfo, err error) {
			if a.PutIdleConn != nil {
				a.PutIdleConn(connInfo, err)
			}
			if b.PutIdleConn != nil {
				b.PutIdleConn(connInfo, err)
			}
		}
	}
	return r
}

func CombineTracer(a *Tracer, b *Tracer) *Tracer {
	if a != nil && b == nil {
		return a
	}
	if a == nil && b != nil {
		return b
	}

	r := &Tracer{}

	if a.StatGetEndpointFail != nil || b.StatGetEndpointFail != nil {
		r.StatGetEndpointFail = func(req Request, err error) {
			if a.StatGetEndpointFail != nil {
				a.StatGetEndpointFail(req, err)
			}
			if b.StatGetEndpointFail != nil {
				b.StatGetEndpointFail(req, err)
			}
		}
	}

	if a.StatConn != nil || b.StatConn != nil {
		r.StatConn = func(req Request, connInfo *ConnInfo, gotConnInfo *TraceGotConnInfo) {
			if a.StatConn != nil {
				a.StatConn(req, connInfo, gotConnInfo)
			}
			if b.StatConn != nil {
				b.StatConn(req, connInfo, gotConnInfo)
			}
		}
	}

	if a.StatRoundTrip != nil || b.StatRoundTrip != nil {
		r.StatRoundTrip = func(req Request, resp Response, connInfo *ConnInfo, connState *ConnState, respInfo *TraceResultInfo) {
			if a.StatRoundTrip != nil {
				a.StatRoundTrip(req, resp, connInfo, connState, respInfo)
			}
			if b.StatRoundTrip != nil {
				b.StatRoundTrip(req, resp, connInfo, connState, respInfo)
			}
		}
	}

	if a.StatResult != nil || b.StatResult != nil {
		r.StatResult = func(req Request, resp Response, respInfo *TraceResultInfo) {
			if a.StatResult != nil {
				a.StatResult(req, resp, respInfo)
			}
			if b.StatResult != nil {
				b.StatResult(req, resp, respInfo)
			}
		}
	}

	if a.StatRoundTripBegin != nil || b.StatRoundTripBegin != nil {
		r.StatRoundTripBegin = func(req Request, ctxx *xctx.ValueCtx) {
			if a.StatRoundTripBegin != nil {
				a.StatRoundTripBegin(req, ctxx)
			}
			if b.StatRoundTripBegin != nil {
				b.StatRoundTripBegin(req, ctxx)
			}
		}
	}

	if a.StatResultWithValue != nil || b.StatResultWithValue != nil {
		r.StatResultWithValue = func(req Request, resp Response, respInfo *TraceResultInfo, ctxx *xctx.ValueCtx) {
			if a.StatResultWithValue != nil {
				a.StatResultWithValue(req, resp, respInfo, ctxx)
			}
			if b.StatResultWithValue != nil {
				b.StatResultWithValue(req, resp, respInfo, ctxx)
			}
		}
	}

	return r
}
