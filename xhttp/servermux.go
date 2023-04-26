package xhttp

import (
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/ccxp/xgo/xlog"
)

// HTTP request multiplexer that match prefix.
// The same as http.ServeMux.
type ServePrefixMux struct {
	Tracer *ServerTracer

	NotFoundHandler http.Handler

	// when a handler for path + "/" was already registered, but
	// not for path itself. If RedirectToPathSlash is true, redirect to u.Path + "/".
	RedirectToPathSlash bool

	m     map[string]*prefixMuxEntry
	mm    map[string]map[string]*prefixMuxEntry
	hosts bool // whether any patterns contain hostnames

	patterns []string
}

type prefixMuxEntry struct {
	h       http.Handler
	pattern string
}

// Does path match pattern?
func prefixPathMatch(pattern, path string) bool {
	if len(pattern) == 0 {
		// should not happen
		return false
	}
	n := len(pattern)
	if pattern[n-1] != '/' {
		return pattern == path
	}
	return len(path) >= n && path[0:n] == pattern
}

// cleanPath returns the canonical path for p, eliminating . and .. elements.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	// path.Clean removes trailing slash except for root;
	// put the trailing slash back if necessary.
	if p[len(p)-1] == '/' && np != "/" {
		// Fast path for common case of p being the string we want:
		if len(p) == len(np)+1 && strings.HasPrefix(p, np) {
			np = p
		} else {
			np += "/"
		}
	}
	return np
}

const maxSlashPos = 4

// find slash post, max retun pos is maxSlashPos
func findSlash(s string) []int {
	pos := make([]int, 0, maxSlashPos)
	cnt := len(s)
	for i := 0; i < cnt && len(pos) < maxSlashPos; i++ {
		if s[i] == '/' {
			pos = append(pos, i)
		}
	}
	return pos
}

// stripHostPort returns h without any trailing ":<port>".
func stripHostPort(h string) string {
	// If no port on host, return unchanged
	if strings.IndexByte(h, ':') == -1 {
		return h
	}
	host, _, err := net.SplitHostPort(h)
	if err != nil {
		return h // on error, return unchanged
	}
	return host
}

// Find a handler on a handler map given a path string.
// Most-specific (longest) pattern wins.
func (mux *ServePrefixMux) match(path string) (h http.Handler, pattern string) {
	// Check for exact match first.
	v, ok := mux.m[path]
	if ok {
		return v.h, v.pattern
	}

	pos := findSlash(path)
	for i := len(pos) - 1; i >= 0; i-- {
		tmpPath := path[0 : pos[i]+1]
		if m, ok := mux.mm[tmpPath]; ok {

			// Check for longest valid match.
			var n = 0
			for k, v := range m {
				if !prefixPathMatch(k, path) {
					continue
				}
				if h == nil || len(k) > n {
					n = len(k)
					h = v.h
					pattern = v.pattern
				}
			}
			if h != nil {
				return
			}
		}
	}

	return
}

// redirectToPathSlash determines if the given path needs appending "/" to it.
// This occurs when a handler for path + "/" was already registered, but
// not for path itself. If the path needs appending to, it creates a new
// URL, setting the path to u.Path + "/" and returning true to indicate so.
func (mux *ServePrefixMux) redirectToPathSlash(host, path string, u *url.URL) (*url.URL, bool) {
	if !mux.RedirectToPathSlash {
		return u, false
	}
	shouldRedirect := mux.shouldRedirect(host, path)
	if !shouldRedirect {
		return u, false
	}
	path = path + "/"
	u = &url.URL{Path: path, RawQuery: u.RawQuery}
	return u, true
}

// shouldRedirect reports whether the given path and host should be redirected to
// path+"/". This should happen if a handler is registered for path+"/" but
// not path -- see comments at ServePrefixMux.
func (mux *ServePrefixMux) shouldRedirect(host, path string) bool {
	p := []string{path, host + path}

	for _, c := range p {
		if _, exist := mux.m[c]; exist {
			return false
		}
	}

	n := len(path)
	if n == 0 {
		return false
	}
	for _, c := range p {
		if _, exist := mux.m[c+"/"]; exist {
			return path[n-1] != '/'
		}
	}

	return false
}

// Handler returns the handler to use for the given request,
// consulting r.Method, r.Host, and r.URL.Path. It always returns
// a non-nil handler. If the path is not in its canonical form, the
// handler will be an internally-generated handler that redirects
// to the canonical path. If the host contains a port, it is ignored
// when matching handlers.
//
// The path and host are used unchanged for CONNECT requests.
//
// Handler also returns the registered pattern that matches the
// request or, in the case of internally-generated redirects,
// the pattern that will match after following the redirect.
//
// If there is no registered handler that applies to the request,
// Handler returns a ``page not found'' handler and an empty pattern.
func (mux *ServePrefixMux) Handler(r *http.Request) (h http.Handler, pattern string) {

	// CONNECT requests are not canonicalized.
	if r.Method == "CONNECT" {
		// If r.URL.Path is /tree and its handler is not registered,
		// the /tree -> /tree/ redirect applies to CONNECT requests
		// but the path canonicalization does not.
		if u, ok := mux.redirectToPathSlash(r.URL.Host, r.URL.Path, r.URL); ok {
			return http.RedirectHandler(u.String(), http.StatusMovedPermanently), u.Path
		}

		return mux.handler(r.Host, r.URL.Path)
	}

	// All other requests have any port stripped and path cleaned
	// before passing to mux.handler.
	host := stripHostPort(r.Host)
	path := cleanPath(r.URL.Path)

	// If the given path is /tree and its handler is not registered,
	// redirect for /tree/.
	if u, ok := mux.redirectToPathSlash(host, path, r.URL); ok {
		return http.RedirectHandler(u.String(), http.StatusMovedPermanently), u.Path
	}

	if path != r.URL.Path {
		_, pattern = mux.handler(host, path)
		url := *r.URL
		url.Path = path
		return http.RedirectHandler(url.String(), http.StatusMovedPermanently), pattern
	}

	return mux.handler(host, r.URL.Path)
}

// handler is the main implementation of Handler.
// The path is known to be in canonical form, except for CONNECT methods.
func (mux *ServePrefixMux) handler(host, path string) (h http.Handler, pattern string) {

	// Host-specific pattern takes precedence over generic ones
	if mux.hosts {
		h, pattern = mux.match(host + path)
	}
	if h == nil {
		h, pattern = mux.match(path)
	}
	if h == nil {
		h, pattern = mux.NotFoundHandler, ""
	}
	return
}

// ServeHTTP dispatches the request to the handler whose
// pattern most closely matches the request URL.
func (mux *ServePrefixMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.RequestURI == "*" {
		if r.ProtoAtLeast(1, 1) {
			w.Header().Set("Connection", "close")
		}
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	h, p := mux.Handler(r)

	ww := NewResponseWriter(w, r)
	ww.ValCtxSet(PatternKey, p)

	h.ServeHTTP(ww, r)
	ww.CloseZip()
}

// Handle registers the handler for the given pattern.
// If a handler already exists for pattern, Handle xlog.Errors.
func (mux *ServePrefixMux) Handle(pattern string, handler http.Handler) {

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
		mux.m = make(map[string]*prefixMuxEntry)
	}
	if mux.mm == nil {
		mux.mm = make(map[string]map[string]*prefixMuxEntry)
	}

	entry := &prefixMuxEntry{h: handler, pattern: pattern}
	mux.m[pattern] = entry

	if pattern[len(pattern)-1] == '/' {
		pos := findSlash(pattern)
		tmpPattern := pattern[0 : pos[len(pos)-1]+1]
		m := mux.mm[tmpPattern]
		if m == nil {
			m = make(map[string]*prefixMuxEntry)
		}
		m[pattern] = entry
		mux.mm[tmpPattern] = m
	}

	if mux.patterns == nil {
		mux.patterns = make([]string, 0, 1)
	}
	mux.patterns = append(mux.patterns, pattern)

	if pattern[0] != '/' {
		mux.hosts = true
	}

	if mux.NotFoundHandler == nil {
		mux.NotFoundHandler = &NotFoundHandler{
			Tracer: mux.Tracer,
		}
	}
}

// HandleFunc registers the handler function for the given pattern.
func (mux *ServePrefixMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	if handler == nil {
		xlog.Error("http: nil handler")
		return
	}
	mux.Handle(pattern, http.HandlerFunc(handler))
}

func (mux *ServePrefixMux) Patterns() []string {
	return mux.patterns
}

// NotFoundHandler returns a simple request handler
// that replies to each request with a ``403 forbidden'' reply.
type NotFoundHandler struct {
	Tracer *ServerTracer
}

func (h *NotFoundHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
	if h.Tracer != nil && h.Tracer.StateHandlerNotFound != nil {
		if ww, ok := w.(*ResponseWriter); ok {
			h.Tracer.StateHandlerNotFound(w, r, time.Now().Sub(ww.TimeStamp))
		} else {
			h.Tracer.StateHandlerNotFound(w, r, 0)
		}
	}
}
