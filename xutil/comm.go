package xutil

import (
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
