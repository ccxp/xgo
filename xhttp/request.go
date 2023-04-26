package xhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
)

// request body part
type part struct {
	name         string
	data         []byte
	contentType  string
	fileName     string
	isAttachment bool
}

type RequestBuilder struct {
	Method string
	Url    string

	Form url.Values // query string

	ContentType string
	Body        []byte // for post

	Cookies []*http.Cookie // cookie to send
	Headers map[string]string

	// If SetExtra is not nil, call SetExtra in Make.
	SetExtra func(req *http.Request) error

	// for multipart
	partList []*part
}

// add url query value
func (r *RequestBuilder) AddForm(key, value string) {
	if r.Form == nil {
		r.Form = make(url.Values)
	}
	r.Form.Add(key, value)
}

// Encode values into URL query string value,
//
// from can be struct, *map[string]string or *map[string][]string.
//
// Struct field can be customized by the format string stored under the "query" key in the struct field's tag.
//
// Example:
//
//     type MyData struct {
//         Aa int      `query:"aa"`
//         Bb []string `query:"bb"`
//         Cc *int     `query:"cc"`
//         Dd []*int   `query:"dd"`
//     }
//
func (r *RequestBuilder) FormMarshal(dat interface{}) error {
	if r.Form == nil {
		r.Form = make(url.Values)
	}
	return (*Values)(&r.Form).Marshal(dat)
}

// serializes the given struct as JSON into the request body, and set Content-Type to "application/json".
func (r *RequestBuilder) MarshalJSON(v interface{}) error {
	return r.Marshal(json.Marshal, v, "application/json")
}

// serializes the given struct as JSON into the response body. and set Content-Type.
func (r *RequestBuilder) Marshal(marshaler func(interface{}) ([]byte, error), data interface{}, contentType string) error {

	b, err := marshaler(data)
	if err != nil {
		return err
	}

	r.Body = b
	r.ContentType = contentType
	return nil
}

// add file data
func (r *RequestBuilder) AddFile(name string, data []byte, contentType string, fileName string) {
	p := &part{
		name:        name,
		data:        data,
		contentType: contentType,
		fileName:    fileName,
	}

	if contentType == "" {
		p.contentType = "application/octet-stream"
	}

	if r.partList == nil {
		r.partList = make([]*part, 0, 1)
	}
	r.partList = append(r.partList, p)
}

// add multipart
func (r *RequestBuilder) AddPart(name string, data []byte, contentType string) {
	p := &part{
		name:        name,
		data:        data,
		contentType: contentType,
		fileName:    "",
	}

	if contentType == "" {
		p.contentType = "application/octet-stream"
	}

	if r.partList == nil {
		r.partList = make([]*part, 0, 1)
	}
	r.partList = append(r.partList, p)
}

// add attachment
func (r *RequestBuilder) AddAttachment(name, fileName string, data []byte, contentType string) {
	p := &part{
		name:         name,
		data:         data,
		contentType:  contentType,
		fileName:     fileName,
		isAttachment: true,
	}

	if contentType == "" {
		p.contentType = "application/octet-stream"
	}

	if r.partList == nil {
		r.partList = make([]*part, 0, 1)
	}
	r.partList = append(r.partList, p)
}

func (r *RequestBuilder) BuildWithContext(ctx context.Context) (*http.Request, error) {
	var req *http.Request
	var err error
	u := r.Url
	if r.Method == "" || r.Method == "GET" {
		if r.Form != nil {
			u += "?" + r.Form.Encode()
		}

		req, err = http.NewRequestWithContext(ctx, "GET", u, nil)
	} else {
		if len(r.Body) > 0 {
			// body exists, set query to url
			if r.Form != nil {
				u += "?" + r.Form.Encode()
			}

			body := bytes.NewReader(r.Body)
			req, err = http.NewRequestWithContext(ctx, r.Method, u, body)
			if err == nil && r.ContentType != "" {
				req.Header.Set("Content-Type", r.ContentType)
			}
		} else if len(r.partList) > 0 { // use multipart
			body := &bytes.Buffer{}
			w := multipart.NewWriter(body)

			if r.Form != nil {
				for k, vs := range r.Form { //add valuse to multipart
					for _, v := range vs {
						err = w.WriteField(k, v)
						if err != nil {
							return nil, err
						}
					}
				}
			}

			var contentDisposition string
			for _, p := range r.partList { // add part to multipart
				h := make(textproto.MIMEHeader)
				if p.isAttachment {
					contentDisposition = "attachment"
				} else {
					contentDisposition = "form-data"
				}

				if p.fileName != "" {
					h.Set("Content-Disposition", fmt.Sprintf(`%s; name="%s"; filename="%s"`, contentDisposition, escapeQuotes(p.name), escapeQuotes(p.fileName)))
				} else {
					h.Set("Content-Disposition", fmt.Sprintf(`%s; name="%s""`, contentDisposition, escapeQuotes(p.name)))
				}
				if p.contentType != "" {
					h.Set("Content-Type", p.contentType)
				}
				pw, err := w.CreatePart(h)
				if err != nil {
					return nil, err
				}
				pw.Write(p.data)
			}
			w.Close()

			req, err = http.NewRequestWithContext(ctx, r.Method, u, body)
			if err == nil {
				req.Header.Set("Content-Type", w.FormDataContentType())
			}
		} else {
			// no body, set query to body
			query := ""
			if r.Form != nil {
				query = r.Form.Encode()
			}

			body := strings.NewReader(query)
			req, err = http.NewRequestWithContext(ctx, r.Method, u, body)
			if err == nil {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
		}
	}

	if err != nil {
		return nil, err
	}

	// Set cookie
	for i := 0; i < len(r.Cookies); i++ {
		req.AddCookie(r.Cookies[i])
	}

	if r.Headers != nil {
		for k, v := range r.Headers {
			req.Header.Set(k, v)
		}
	}

	if r.SetExtra != nil {
		err = r.SetExtra(req)
		if err != nil {
			return nil, err
		}
	}
	return req, nil
}

func (r *RequestBuilder) Build() (*http.Request, error) {
	return r.BuildWithContext(context.Background())
}

var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"")

func escapeQuotes(s string) string {
	return quoteEscaper.Replace(s)
}
