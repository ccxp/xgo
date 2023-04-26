package xlog

import (
	"fmt"
	"log"
	"os"
	"testing"
)

func TestLogger(t *testing.T) {

	if false {
		w := &HourSplitFileWriter{
			OutputDir:  "/tmp",
			NamePrefix: "11_",
			NameSuffix: ".log",

			MaxSizePerFile: 1024 * 1024 * 1024,

			UseBufio: true,

			DupWriter: os.Stdout,
		}

		e := w.Init()
		fmt.Printf("Init %v\n", e)

		logger := NewColorLogger(w, Ldebug, Ldate|Ltime|Lshortfile|Lcolor|Lgoid)
		SetLogger(logger)

		Errorf("aa %v", 1)
		Warnf("bb %v", 1)
		Debugf("dd %v", 1)

		Output(Lerror, 0, "xxxx")

		Error(1, 2)
		Error(2, 3)

		fmt.Print(1, 2)
		fmt.Println(1, 2)

		w.Close()
	}

	if false {
		Errorf("aa %v", 1)
		Warnf("bb %v", 1)
		Debugf("dd %v", 1)

		Output(Lerror, 0, "xxxx")
	}

	if false {
		w := &HourSplitFileWriter{
			OutputDir:  "/tmp",
			NamePrefix: "11_",
			NameSuffix: ".log",

			MaxSizePerFile: 1024 * 1024 * 1024,

			UseBufio: true,

			DupWriter: os.Stdout,
		}

		e := w.Init()
		fmt.Printf("Init %v\n", e)

		log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
		log.SetOutput(w)

		log.Printf("aa %v", 2)

		w.Close()
	}

	if true {

		SetLevel(Lwarn)
		l := Log{
			// Logger:     GetLogger(),
			CallerDept: 1,
		}

		l.Errorf("aa %v", 1)
		l.Warnf("bb %v", 1)
		l.Debugf("dd %v", 1)

		l.Output(Lerror, 0, "xxxx")

		l.Error(1, 2)
		l.Error(2, 3)

	}

}
