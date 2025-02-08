package xnet

import (
	"context"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ccxp/xgo/xlog"
)

type AsyncResolver func() (res interface{}, e error)

/*************************************/

type asyncResolverResultData struct {
	Res interface{}
	Err error
	Ts  time.Time
}

type AsyncResolverResult struct {
	res  atomic.Pointer[asyncResolverResultData]
	last atomic.Pointer[asyncResolverResultData]
}

// 取得解析结果.
//
// 注意成功一次后，会一直只返回成功结果, t是上次成功的时间, unixnano.
// e只会在从来没有成功时返回
func (r *AsyncResolverResult) Get() (res interface{}, t time.Time, e error) {
	v := r.res.Load()
	if v == nil {
		v = r.last.Load()
	}
	return v.Res, v.Ts, v.Err
}

// 取得上次解析结果.
func (r *AsyncResolverResult) LastResult() (res interface{}, t time.Time, e error) {
	v := r.last.Load()
	return v.Res, v.Ts, v.Err
}

/*************************************/

type asyncResolverHandler struct {
	Resolver AsyncResolver
	Res      *AsyncResolverResult
}

func (h *asyncResolverHandler) Resolve() {
	res, e := h.Resolver()

	d := &asyncResolverResultData{
		Res: res,
		Err: e,
		Ts:  time.Now(),
	}

	h.Res.last.Store(d)
	if e == nil {
		h.Res.res.Store(d)
	}
}

/*************************************/

type asyncResolver struct {
	handlers map[interface{}]*asyncResolverHandler
	mu       sync.RWMutex
	once     sync.Once
}

var innerAsyncResolver = asyncResolver{
	handlers: make(map[interface{}]*asyncResolverHandler),
}

func (ro *asyncResolver) init() {
	go ro.loop()
}

func (ro *asyncResolver) loop() {
	ticker := time.NewTicker(time.Second * 20)
	defer ticker.Stop()

	handlers := make(map[interface{}]*asyncResolverHandler)

	for {
		<-ticker.C

		ro.mu.RLock()
		for k, h := range ro.handlers {
			handlers[k] = h
		}
		ro.mu.RUnlock()

		for _, h := range handlers {
			h.Resolve()
		}
	}
}

func (ro *asyncResolver) register(key interface{}, resolver AsyncResolver) *AsyncResolverResult {
	ro.once.Do(ro.init)

	var ok bool
	var h *asyncResolverHandler
	ro.mu.RLock()
	if h, ok = ro.handlers[key]; ok {
		ro.mu.RUnlock()
		return h.Res
	}
	ro.mu.RUnlock()

	// 读一次
	res, e := resolver()
	t := time.Now()

	ro.mu.Lock()
	defer ro.mu.Unlock()

	if h, ok = ro.handlers[key]; ok {
		return h.Res
	}

	d := &asyncResolverResultData{
		Res: res,
		Err: e,
		Ts:  t,
	}

	r := &AsyncResolverResult{}
	r.last.Store(d)
	if e == nil {
		r.res.Store(d)
	}

	h = &asyncResolverHandler{
		Resolver: resolver,
		Res:      r,
	}

	ro.handlers[key] = h
	xlog.Debugf("register AsyncResolver %v", key)
	return r
}

// 注意异步地址解析器, 20s解析一次.
//
// 各种地址解析会因为某些原因失败，比如dns服务器故障会引起地址解析超时，
// 如果同步解析会引起服务调用错误，为减少这种解析异常，可使用异步解析，在解析失败时不更新地址信息。
//
// 注意同样的key只会注册一次.
func RegisterAsyncResolver(key interface{}, fn AsyncResolver) *AsyncResolverResult {
	return innerAsyncResolver.register(key, fn)
}

/*************************************/

type asyncSingleHostBalancerKey struct {
	Host string
}

func (k asyncSingleHostBalancerKey) String() string {
	return "Host:" + k.Host
}

type asyncSingleHostBalancerData struct {
	addrs []string
	t     time.Time
}

// 使用 RegisterAsyncResolver 做的异步地址解析器.
type AsyncSingleHostBalancer struct {
	Host string
	Port int

	// 直接写进ConnInfo
	Network, Scheme string

	// 设置ConnInfo其它数据
	SetConnInfo func(connInfo *ConnInfo)

	port string

	res  *AsyncResolverResult
	d    atomic.Pointer[asyncSingleHostBalancerData]
	once sync.Once
}

func (b *AsyncSingleHostBalancer) GetAddr() (addrs []string, updateTime time.Time, e error) {

	b.once.Do(b.init)

	res, t, e := b.res.Get()
	if e != nil {
		return nil, t, e
	}

	old := b.d.Load()
	if old != nil && t.Equal(old.t) {
		return old.addrs, t, nil
	}

	newaddrs := res.([]string)
	addrs = make([]string, len(newaddrs))
	for i, a := range newaddrs {
		addrs[i] = net.JoinHostPort(a, b.port)
	}

	b.d.Store(&asyncSingleHostBalancerData{
		addrs: addrs,
		t:     t,
	})

	return addrs, t, nil
}

func (b *AsyncSingleHostBalancer) getEndpoint(ctx context.Context, req Request, addrUsed []string) (connInfo ConnInfo, err error) {

	addrs, _, e := b.GetAddr()
	if e != nil {
		err = e
		return
	}

	// 随机选里面个
	if len(addrs) > 0 {
		var usedMap map[string]bool
		if len(addrUsed) > 5 {
			usedMap := make(map[string]bool, len(addrUsed))
			for _, a := range addrUsed {
				usedMap[a] = true
			}
		}

		cnt := len(addrs)
		offset := 0
		if cnt > 1 {
			offset = rand.Intn(len(addrs))
		}

		for i := 0; i < cnt; i++ {
			a := addrs[(i+offset)%cnt]
			if len(addrUsed) > 5 {
				if _, ok := usedMap[a]; ok {
					continue
				}
			} else {
				used := false
				for _, au := range addrUsed {
					if a == au {
						used = true
						break
					}
				}
				if used {
					continue
				}
			}

			connInfo = ConnInfo{
				Addr:    a,
				Host:    b.Host,
				Network: b.Network,
				Scheme:  b.Scheme,
			}
			if b.SetConnInfo != nil {
				b.SetConnInfo(&connInfo)
			}
			return connInfo, nil
		}
	}
	return ConnInfo{}, ErrGetEndpointFail

}

func (b *AsyncSingleHostBalancer) init() {
	b.port = strconv.Itoa(b.Port)
	key := asyncSingleHostBalancerKey{
		Host: b.Host,
	}
	b.res = RegisterAsyncResolver(key, b.resolve)
}

func (b *AsyncSingleHostBalancer) resolve() (res interface{}, e error) {
	addrs, e := net.LookupHost(b.Host)
	return addrs, e
}

func (b *AsyncSingleHostBalancer) Balancer() *Balancer {
	return &Balancer{
		GetEndpoint: b.getEndpoint,
	}

}
