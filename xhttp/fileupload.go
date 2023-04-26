package xhttp

import (
	"github.com/ccxp/xgo/xlog"
	"mime/multipart"
	"net/http"
)

// file upload handler.
type FileUploadHandler struct {
	Save func(r *http.Request, key string, f multipart.File, fh *multipart.FileHeader) error
}

func (h *FileUploadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	r.ParseMultipartForm(defaultMaxMemory)
	if r.MultipartForm == nil || r.MultipartForm.File == nil {
		return
	}

	for k, fhs := range r.MultipartForm.File {
		for _, fh := range fhs {
			f, err := fh.Open()
			if err != nil {
				xlog.Errorf("open multipart file fail: %v", err)
				return
			}

			err = h.Save(r, k, f, fh)
			if err != nil {
				xlog.Errorf("Save multipart file fail: %v", err)
				return
			}
		}
	}

}
