package xhttp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/ccxp/xgo/xlog"
)

type restFulNode struct {
	Child      map[string]*restFulNode
	ParamChild []*restFulNode
	parent     *restFulNode

	Name    string
	IsParam bool

	MaxChildDept int

	handler http.Handler
	pattern string
}

func (n *restFulNode) InsertChild(name string) *restFulNode {

	modifyDept := false
	r := &restFulNode{
		Child:      make(map[string]*restFulNode),
		ParamChild: make([]*restFulNode, 0),
		parent:     n,
		Name:       name,
	}

	if n.MaxChildDept == 0 {
		n.MaxChildDept = 1
		modifyDept = true
	}
	if len(name) > 0 && name[0] == ':' {
		r.IsParam = true
		r.Name = r.Name[1:]
		n.ParamChild = append(n.ParamChild, r)
	} else {
		if r, ok := n.Child[name]; ok {
			return r
		}

		n.Child[name] = r
	}

	// calc MaxChildDept
	if modifyDept {
		c := n
		p := c.parent
		for modifyDept && p != nil {
			modifyDept = false
			if p.MaxChildDept < c.MaxChildDept+1 {
				p.MaxChildDept = c.MaxChildDept + 1
				modifyDept = true
			}
			c = p
			p = c.parent
		}
	}
	return r
}

func (n *restFulNode) Handle(method, pattern string, handler http.Handler) {

	if !strings.HasPrefix(pattern, "/") {
		xlog.Error("http: invalid pattern")
		return
	}
	if handler == nil {
		xlog.Error("http: nil handler")
		return
	}

	c := n
	for _, seg := range strings.Split(pattern[1:], "/") { // at least one
		c = c.InsertChild(seg)
	}
	c.handler = handler
	c.pattern = method + " " + pattern
}

func (n *restFulNode) match(segs []string, buf []*restFulNode, idx int) {
	if idx >= len(segs) || n.MaxChildDept < len(segs)-1-idx {
		return
	}
	ok := false
	buf[idx], ok = n.Child[segs[idx]]
	if !ok {
		for _, c := range n.ParamChild {
			buf[idx] = c
			// match child
			buf[idx].match(segs, buf, idx+1)
			if buf[len(segs)-1] != nil {
				return
			}
			buf[idx] = nil
		}
	} else {
		// match child
		buf[idx].match(segs, buf, idx+1)
		if buf[len(segs)-1] != nil {
			return
		}

		// if fail, test paramChild.
		buf[idx] = nil
		for _, c := range n.ParamChild {
			buf[idx] = c
			// match child
			buf[idx].match(segs, buf, idx+1)
			if buf[len(segs)-1] != nil {
				return
			}
			buf[idx] = nil
		}
	}

	// test * child
	buf[idx], ok = n.Child["*"]
	if ok {
		buf[idx].match(segs, buf, idx+1)
	}
}

type Param struct {
	Name  string
	Value string
}

func (p Param) String() string {
	return fmt.Sprintf("{\"%v\":\"%v\"}", p.Name, p.Value)
}

func (p Param) MarshalJSON() ([]byte, error) {
	n, _ := json.Marshal(p.Name)
	v, _ := json.Marshal(p.Value)

	b := bytes.NewBufferString("{\"Name\":")
	b.Write(n)
	b.WriteString(",\"Value\":")
	b.Write(v)
	b.WriteByte('}')

	return b.Bytes(), nil
}

type Params struct {
	Params []Param
}

func (ps *Params) Get(name string) (string, bool) {
	for _, p := range ps.Params {
		if p.Name == name {
			return p.Value, true
		}
	}
	return "", false
}

func (ps Params) MarshalJSON() ([]byte, error) {
	b := bytes.NewBufferString("{\"Params\":[")
	for i, p := range ps.Params {
		if i > 0 {
			b.WriteByte(',')
		}
		n, _ := json.Marshal(p.Name)
		v, _ := json.Marshal(p.Value)

		b.WriteString("{\"Name\":")
		b.Write(n)
		b.WriteString(",\"Value\":")
		b.Write(v)
		b.WriteByte('}')
	}
	b.WriteString("]}")
	return b.Bytes(), nil
}

func (ps Params) String() string {
	b := strings.Builder{}
	b.WriteString("[")
	for i, p := range ps.Params {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("{\"")
		b.WriteString(p.Name)
		b.WriteString("\":\"")
		b.WriteString(p.Value)
		b.WriteString("\"}")
	}
	b.WriteString("]")
	return b.String()
}

type restBufferPool struct {
	nodePool *sync.Pool
}

func (p *restBufferPool) getNode(maxLen int) []*restFulNode {
	r := p.nodePool.Get()
	if r == nil {
		return make([]*restFulNode, maxLen)
	}

	out := r.([]*restFulNode)
	if maxLen > len(out) {
		return make([]*restFulNode, maxLen)
	}
	return out
}

func (p *restBufferPool) putNode(x []*restFulNode) {
	p.nodePool.Put(x)
}

type restFulRoot struct {
	Root     *restFulNode
	pool     *restBufferPool
	patterns map[string]int
}

func newRestFulRoot() *restFulRoot {
	return &restFulRoot{
		Root: &restFulNode{
			Child:      make(map[string]*restFulNode),
			ParamChild: make([]*restFulNode, 0),
		},
		pool: &restBufferPool{
			nodePool: &sync.Pool{},
		},
		patterns: make(map[string]int),
	}
}
func (r *restFulRoot) Handle(method, pattern string, handler http.Handler) {
	if _, ok := r.patterns[pattern]; ok {
		xlog.Error("http: multiple registrations for " + method + " " + pattern)
		return
	}
	r.patterns[pattern] = 1
	r.Root.Handle(method, pattern, handler)
}

func (r *restFulRoot) Handler(u string) (*Params, http.Handler, string) {

	if len(u) == 0 || u[0] != '/' {
		return nil, nil, ""
	}

	segs := strings.Split(u[1:], "/")
	cnt := len(segs)

	if cnt > r.Root.MaxChildDept {
		return nil, nil, ""
	}

	ns := r.pool.getNode(r.Root.MaxChildDept)
	defer r.pool.putNode(ns)

	ns[cnt-1] = nil
	r.Root.match(segs, ns, 0)
	if ns[cnt-1] == nil || ns[cnt-1].handler == nil {
		return nil, nil, ""
	}

	idx := 0

	for i := 0; i < cnt; i++ {
		if ns[i].IsParam {
			idx++
		}
	}

	ps := &Params{
		Params: make([]Param, idx),
	}

	idx = 0
	for i := 0; i < cnt; i++ {
		if ns[i].IsParam {
			ps.Params[idx].Name = ns[i].Name
			ps.Params[idx].Value = segs[i]
			idx++
		}
	}
	return ps, ns[cnt-1].handler, ns[cnt-1].pattern
}

// --------------------------------------------ServeRESTfulMux

// HTTP request multiplexer that match RESTful.
//
// pattern: /xx/:a
// url:     /xx/yy  match
//          /xx/yy/ not match
//
// pattern: /*/:a
// url:     /xx/yy  match
//          /yy/zz/ not match
//
type ServeRESTfulMux struct {
	Tracer          *ServerTracer
	NotFoundHandler http.Handler

	// when a handler for path + "/" was already registered, but
	// not for path itself. If RedirectToPathSlash is true, redirect to u.Path + "/".
	RedirectToPathSlash bool

	m map[string]*restFulRoot

	patterns []string
}

// Handler returns the handler to use for the given request,
// consulting r.Method, and r.URL.Path. It always returns
// a non-nil handler. If the path is not in its canonical form, the
// handler will be an internally-generated handler that redirects
// to the canonical path. If the host contains a port, it is ignored
// when matching handlers.
//
// Handler also returns the registered pattern that matches the
// request or, in the case of internally-generated redirects,
// the pattern that will match after following the redirect.
//
// If there is no registered handler that applies to the request,
// Handler returns a ``page not found'' handler and an empty pattern.
func (mux *ServeRESTfulMux) Handler(r *http.Request) (h http.Handler, pattern string, param *Params) {

	path := cleanPath(r.URL.Path)

	root, ok := mux.m[r.Method]
	if !ok {
		return nil, "", nil
	}

	param, h, pattern = root.Handler(path)
	if h == nil {
		// check if server-wide handler existrs
		param, h, pattern = root.Handler("*")
	}

	if h == nil && mux.RedirectToPathSlash && !strings.HasSuffix(path, "/") {
		param, h, pattern = root.Handler(path + "/")
		if h != nil {
			return http.RedirectHandler(r.URL.String(), http.StatusMovedPermanently), pattern, nil
		}
	}

	if h == nil {
		h, pattern = mux.NotFoundHandler, ""
	}
	return
}

// ServeHTTP dispatches the request to the handler whose
// pattern most closely matches the request URL.
func (mux *ServeRESTfulMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.RequestURI == "*" {
		if r.ProtoAtLeast(1, 1) {
			w.Header().Set("Connection", "close")
		}
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	h, p, param := mux.Handler(r)

	ww := NewResponseWriter(w, r)
	ww.ValCtxSet(ParamKey, param)
	ww.ValCtxSet(PatternKey, p)

	h.ServeHTTP(ww, r)
	ww.CloseZip()
}

// Handle registers the handler for the given pattern.
// If a handler already exists for pattern, Handle panics.
func (mux *ServeRESTfulMux) Handle(method, pattern string, handler http.Handler) {

	if pattern == "" {
		xlog.Error("http: invalid pattern")
		return
	}
	if handler == nil {
		xlog.Error("http: nil handler")
		return
	}
	if _, exist := mux.m[pattern]; exist {
		xlog.Error("http: multiple registrations for " + pattern)
		return
	}

	if mux.Tracer != nil {
		// mork handler
		if _, ok := handler.(*Handler); !ok {
			handler = &Handler{
				Handler: handler,
				Tracer:  mux.Tracer,
			}
		}
	}

	if mux.m == nil {
		mux.m = make(map[string]*restFulRoot)
	}
	root, ok := mux.m[method]
	if !ok {
		root = newRestFulRoot()
		mux.m[method] = root
	}
	root.Handle(method, pattern, handler)

	if mux.patterns == nil {
		mux.patterns = make([]string, 0, 1)
	}
	mux.patterns = append(mux.patterns, method+" "+pattern)

	if mux.NotFoundHandler == nil {
		mux.NotFoundHandler = &NotFoundHandler{
			Tracer: mux.Tracer,
		}
	}
}

// HandleFunc registers the handler function for the given pattern.
func (mux *ServeRESTfulMux) HandleFunc(method, pattern string, handler func(http.ResponseWriter, *http.Request)) {
	if handler == nil {
		xlog.Error("http: nil handler")
		return
	}
	mux.Handle(method, pattern, http.HandlerFunc(handler))
}

func (mux *ServeRESTfulMux) Patterns() []string {
	return mux.patterns
}
