package xfile

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"testing"
	"time"
)

func print(v interface{}) {
	b, e := json.MarshalIndent(v, "", "\t")
	fmt.Fprintf(os.Stderr, "%s %v\n", b, e)
}

func printLn(f string, v ...interface{}) {
	fmt.Fprintf(os.Stderr, f, v...)
}

func TestCall1(t *testing.T) {
	f, _ := os.OpenFile("/tmp/1.dat", os.O_WRONLY|os.O_CREATE, 0755)
	w, _ := NewWriter(f, NewBufioWriterPool(100))
	defer w.Close()

	w.SetPreAlloc(1024)
	for i := 0; i < 10; i++ {
		p, n, e := w.Write([]byte(fmt.Sprintf("aaaaa%d", i)))
		printLn("%v %v %v\n", p, n, e)
	}

	w.Flush()
	f.Write([]byte("adsfja;sdkfja;slkjf;aslekjf;aksejf;raksejf;"))
}

func TestCall2(t *testing.T) {
	f, _ := os.Open("/tmp/1.dat")
	r, _ := NewReader(f, NewBufioReaderPool(100), NewBufferPool(4))
	defer r.Close()

	for {
		b, e := r.Next()
		printLn("%v %s\n", e, b)
		if e != nil {
			break
		}
	}

	r.Seek(38, io.SeekStart)
	b, e := r.Read()
	printLn("%v %s\n", e, b)
}

func TestFileLoader(t *testing.T) {
	fl := FileLoader{
		FilePath:              "/tmp/1.dat",
		CheckModifiedDuration: time.Second * 2,

		Read: func(path string) error {
			fmt.Printf("reload...\n")
			return nil
		},
	}

	for i := 0; i < 5; i++ {
		go func() {
			ticker := time.NewTicker(time.Second / 2)
			defer ticker.Stop()

			for {
				fl.LoadIfModified()
				<-ticker.C
			}
		}()
	}

	time.Sleep(time.Hour)
}
