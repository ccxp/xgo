package xhttp

import (
	"github.com/ccxp/xgo/xnet"
	"net/http"
)

// rpc converter handler.
//
// convert http.Request to rpc request, and convert rpc response to http response.
type RPCConverterHandler struct {
	ConvertRequest  func(*http.Request) (xnet.Request, error)
	Transport       *xnet.Transport
	ConvertResponse func(xnet.Response, error, http.ResponseWriter)
}

func (h *RPCConverterHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	req, err := h.ConvertRequest(r)
	if err != nil {
		return
	}

	resp, err := h.Transport.RoundTrip(req)
	h.ConvertResponse(resp, err, w)
}
