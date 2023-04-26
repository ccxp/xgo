package xlog_test

import (
	"fmt"
	"github.com/ccxp/xgo/xlog"
	"log"
)

func ExampleSetLogger() {

	output := &xlog.HourSplitFileWriter{
		OutputDir:  "/tmp",
		NamePrefix: "test_",
		NameSuffix: ".log",

		MaxSizePerFile: 1024 * 1024 * 1024,

		UseBufio: true,

		// Set DupWriter if you want to output to other writer.
		// DupWriter: os.Stderr,
	}

	e := output.Init()
	log.Printf("output Init %v\n", e)
	defer output.Close() // if use UseBufio, flush on exit

	logger := xlog.NewColorLogger(output, xlog.Ldebug, xlog.Ldate|xlog.Ltime|xlog.Lshortfile|xlog.Lcolor|xlog.Lgoid)
	xlog.SetLogger(logger) // set default logger to colorLogger

	// write log
	xlog.Errorf("aa %v", 1)
	xlog.Warnf("bb %v", 1)
	xlog.Debugf("dd %v", 1)

	xlog.Output(xlog.Lerror, 0, "xxxx")

	// also set output to log package.
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	log.SetOutput(output)

	log.Printf("aa %v", 2)

	fmt.Printf("end\n")
	// Output: end

}
