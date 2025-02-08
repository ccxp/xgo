package xutil

import (
	"io"
	"os"
	"path/filepath"
)

func GetAppName() string {
	return appName
}

var appName string = ""

func init() {
	appName = filepath.Base(os.Args[0])
}

type morkReaderCloser struct {
	io.Reader
}

func (r *morkReaderCloser) Close() error {
	return nil
}

func NewReadCloserFromReader(r io.Reader) io.ReadCloser {
	return &morkReaderCloser{r}
}
