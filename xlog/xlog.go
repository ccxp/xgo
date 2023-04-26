// logger include log formatter.
//
// the write can be used for log package.
package xlog

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/petermattis/goid"
)

// log level
const (
	Loff = 1 + iota
	Lpanic
	Lerror
	Lwarn
	Ldebug
)

const (
	Ldate         = 1 << iota // the date in the local time zone: 2009/01/23
	Ltime                     // the time in the local time zone: 01:23:23
	Lmicroseconds             // microsecond resolution: 01:23:23.123123.  assumes Ltime.
	Llongfile                 // full file name and line number: /a/b/c/d.go:23
	Lshortfile                // final file name element and line number: d.go:23. overrides Llongfile
	LUTC                      // if Ldate or Ltime is set, use UTC rather than the local time zone
	Lcolor                    // set color to text.
	Lgoid                     // add goid to log
	Lpid                      // add pid to log

	LstdFlags = Ldate | Ltime | Lcolor | Lshortfile // initial values for the standard logger
)

var pid = os.Getpid()

type Logger interface {
	SetLevel(level int)
	GetLevel() int
	SetOutput(w io.Writer)

	Output(level, calldepth int, s string)
}

var defaultLogger = NewColorLogger(os.Stderr, Loff, LstdFlags)
var defaultLevel = Ldebug

// 设置默认logger
func SetLogger(logger Logger) {
	defaultLogger = logger
	if logger != nil {
		defaultLevel = logger.GetLevel()
	} else {
		defaultLevel = Loff
	}
}

// 取得默认logger
func GetLogger() Logger {
	return defaultLogger
}

func SetLevel(level int) {
	if defaultLogger != nil {
		defaultLogger.SetLevel(level)
	}
	defaultLevel = level
}

func GetLevel() int {
	if defaultLogger != nil {
		return defaultLevel
	}
	return Loff
}

func SetOutput(w io.Writer) {
	if defaultLogger != nil {
		defaultLogger.SetOutput(w)
	}
}

func Debugf(format string, v ...interface{}) {
	if defaultLevel < Ldebug {
		return
	}

	if defaultLogger != nil {
		defaultLogger.Output(Ldebug, 1, fmt.Sprintf(format, v...))
	}
}

func Warnf(format string, v ...interface{}) {

	if defaultLevel < Lwarn {
		return
	}

	if defaultLogger != nil {
		defaultLogger.Output(Lwarn, 1, fmt.Sprintf(format, v...))
	}
}

func Errorf(format string, v ...interface{}) {
	if defaultLevel < Lerror {
		return
	}

	if defaultLogger != nil {
		defaultLogger.Output(Lerror, 1, fmt.Sprintf(format, v...))
	}
}

func Panicf(format string, v ...interface{}) {

	s := fmt.Sprintf(format, v...)
	if defaultLevel >= Lpanic {
		if defaultLogger != nil {
			defaultLogger.Output(Lpanic, 1, s)
		}
	}
	panic(s)
}

// Format string using fmt.Sprint.
// Spaces are added between operands when neither is a string,
// there is no space between string and none string.
func Debug(v ...interface{}) {

	if defaultLevel < Ldebug {
		return
	}

	if defaultLogger != nil {
		defaultLogger.Output(Ldebug, 1, fmt.Sprint(v...))
	}
}

// Format string using fmt.Sprint.
// Spaces are added between operands when neither is a string,
// there is no space between string and none string.
func Warn(v ...interface{}) {

	if defaultLevel < Lwarn {
		return
	}

	if defaultLogger != nil {
		defaultLogger.Output(Lwarn, 1, fmt.Sprint(v...))
	}
}

// Format string using fmt.Sprint.
// Spaces are added between operands when neither is a string,
// there is no space between string and none string.
func Error(v ...interface{}) {

	if defaultLevel < Lerror {
		return
	}

	if defaultLogger != nil {
		defaultLogger.Output(Lerror, 1, fmt.Sprint(v...))
	}
}

// Format string using fmt.Sprint.
// Spaces are added between operands when neither is a string,
// there is no space between string and none string.
func Panic(v ...interface{}) {
	s := fmt.Sprint(v...)
	if defaultLevel >= Lpanic {
		if defaultLogger != nil {
			defaultLogger.Output(Lpanic, 1, s)
		}
	}
	panic(s)
}

func Output(level, calldepth int, s string) {
	if defaultLevel >= level {
		if defaultLogger != nil {
			defaultLogger.Output(level, calldepth+1, s)
		}
	}
	if level == Lpanic {
		panic(s)
	}
}

func Outputf(level, calldepth int, format string, v ...interface{}) {

	s := fmt.Sprintf(format, v...)
	if defaultLevel >= level {
		if defaultLogger != nil {
			defaultLogger.Output(level, calldepth+1, s)
		}
	}
	if level == Lpanic {
		panic(s)
	}
}

//---------------------------------HourSplitFileWriter

type HourSplitFileWriter struct {
	OutputDir  string
	NamePrefix string
	NameSuffix string

	MaxSizePerFile int64

	UseBufio  bool
	AutoFlush bool // work with UseBufio=true

	// 文件perm，如果为0，则以0640生成文件
	FileMode os.FileMode

	// If DupWriter is not nil, write to file and call DupWriter.Write.
	DupWriter io.Writer

	file      *os.File
	bufWriter *bufio.Writer
	writer    io.Writer

	fileSize int64
	fileTime int64

	inited   bool
	mu       sync.Mutex
	closed   bool
	closeCh  chan struct{}
	doneChan chan struct{}
}

func (w *HourSplitFileWriter) Init() (e error) {
	if w.inited {
		return nil
	}

	if !strings.HasSuffix(w.OutputDir, "/") {
		w.OutputDir += "/"
	}

	e = w.checkSplit()
	if e != nil {
		return
	}

	if w.UseBufio {
		if w.AutoFlush {
			w.closeCh = make(chan struct{})
			w.doneChan = make(chan struct{})
			go w.flushSched()
		}
	} else {
		w.AutoFlush = false
	}

	w.inited = true
	return nil
}

func (w *HourSplitFileWriter) flushSched() {
	ticker := time.NewTicker(time.Millisecond * 500)
	flg := true
	for flg {
		select {
		case <-ticker.C:
			w.mu.Lock()
			if w.bufWriter != nil {
				w.bufWriter.Flush()
			}
			w.checkSplit()
			w.mu.Unlock()

		case <-w.closeCh:
			w.mu.Lock()
			if w.bufWriter != nil {
				w.bufWriter.Flush()
			}
			if w.file != nil {
				w.file.Close()
			}
			w.mu.Unlock()
			flg = false
		}
	}
	ticker.Stop()

	w.doneChan <- struct{}{}
}

func (w *HourSplitFileWriter) checkSplit() (e error) {

	now := time.Now()
	t := now.Unix() / 3600 * 3600
	if t == w.fileTime {
		return
	}

	year, month, day := now.Date()
	hour, _, _ := now.Clock()

	w.fileTime = t
	fn := fmt.Sprintf("%v%v%d%02d%02d%02d%v", w.OutputDir, w.NamePrefix, year, month, day, hour, w.NameSuffix)

	if w.bufWriter != nil {
		w.bufWriter.Flush()
	}
	if w.file != nil {
		w.file.Close()
		w.file = nil
	}

	perm := w.FileMode
	if perm == 0 {
		perm = 0640
	}
	w.file, e = os.OpenFile(fn, os.O_WRONLY|os.O_APPEND|os.O_CREATE, perm)
	if e != nil {
		fmt.Printf("open %s fail: %v\n", fn, e)
		return
	}
	if w.UseBufio {
		if w.bufWriter != nil {
			w.bufWriter.Reset(w.file)
		} else {
			w.bufWriter = bufio.NewWriter(w.file)
		}
	}

	w.fileSize = fileSize(fn)
	return
}

func (w *HourSplitFileWriter) Write(b []byte) (n int, err error) {
	if w.closed {
		if w.DupWriter != nil {
			return w.DupWriter.Write(b)
		}

		return 0, ErrClosed
	}

	if w.AutoFlush {
		w.mu.Lock()
		defer w.mu.Unlock()
	}

	err = w.checkSplit()
	if err != nil {
		return
	}

	if w.file == nil {
		return
	}

	if w.MaxSizePerFile > 0 && w.fileSize+int64(len(b)) > w.MaxSizePerFile {
		return 0, ErrFileSizeExceedLimit
	}

	if w.UseBufio {
		n, err = w.bufWriter.Write(b)
	} else {
		n, err = w.file.Write(b)
	}
	if err == nil {
		w.fileSize += int64(n)
	}
	if w.DupWriter != nil {
		w.DupWriter.Write(b)
	}
	return
}

func fileSize(fn string) int64 {
	f, e := os.Stat(fn)
	if e != nil {
		return 0
	}

	return f.Size()
}

func (w *HourSplitFileWriter) Close() error {
	w.closed = true
	if w.AutoFlush {
		w.closeCh <- struct{}{}
		<-w.doneChan
	} else {
		if w.bufWriter != nil {
			w.bufWriter.Flush()
		}
		if w.file != nil {
			w.file.Close()
		}
	}
	if w.DupWriter != nil {
		if c, ok := w.DupWriter.(io.WriteCloser); ok {
			c.Close()
			w.DupWriter = nil
		}
	}
	return nil
}

var ErrClosed = errors.New("writer is closed")
var ErrFileSizeExceedLimit = errors.New("file size exceed limit")

//----------------------------------------------------------Logger

type colorLogger struct {
	out   io.Writer // destination for output
	level int
	flag  int

	mu sync.Mutex

	buf []byte // for accumulating text to write
}

func NewColorLogger(out io.Writer, level, flag int) Logger {
	return &colorLogger{out: out, level: level, flag: flag}
}

func (l *colorLogger) SetLevel(level int) {
	l.level = level
}

func (l *colorLogger) GetLevel() int {
	return l.level
}

func (l *colorLogger) SetOutput(w io.Writer) {
	l.mu.Lock()
	l.out = w
	l.mu.Unlock()
}

func (l *colorLogger) Output(level, calldepth int, s string) {

	var colorPrefix, tag []byte
	switch level {
	case Lpanic:
		colorPrefix = panicColorPrefix
		tag = panicTag
	case Lerror:
		colorPrefix = errorColorPrefix
		tag = errorTag
	case Lwarn:
		colorPrefix = warnColorPrefix
		tag = warnTag
	case Ldebug:
		colorPrefix = debugColorPrefix
		tag = debugTag
	}

	var file string
	var line int
	var ok bool

	if l.flag&(Lshortfile|Llongfile) != 0 {
		_, file, line, ok = runtime.Caller(calldepth + 1)
		if !ok {
			file = "???"
			line = 0
		} else {
			if l.flag&Lshortfile != 0 {
				short := file
				for i := len(file) - 1; i > 0; i-- {
					if file[i] == '/' {
						short = file[i+1:]
						break
					}
				}
				file = short
			}
		}
	}

	t := time.Now()
	if l.flag&LUTC != 0 {
		t = t.UTC()
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.buf = l.buf[:0]

	if l.flag&Ldate != 0 {
		year, month, day := t.Date()
		itoa(&l.buf, year, 4)
		l.buf = append(l.buf, '/')
		itoa(&l.buf, int(month), 2)
		l.buf = append(l.buf, '/')
		itoa(&l.buf, day, 2)
		l.buf = append(l.buf, ' ')
	}

	if l.flag&(Ltime|Lmicroseconds) != 0 {
		hour, min, sec := t.Clock()
		itoa(&l.buf, hour, 2)
		l.buf = append(l.buf, ':')
		itoa(&l.buf, min, 2)
		l.buf = append(l.buf, ':')
		itoa(&l.buf, sec, 2)
		if l.flag&Lmicroseconds != 0 {
			l.buf = append(l.buf, '.')
			itoa(&l.buf, t.Nanosecond()/1e3, 6)
		}
		l.buf = append(l.buf, ' ')
	}

	if l.flag&Lpid != 0 {
		l.buf = append(l.buf, '[')
		itoa(&l.buf, pid, -1)
		l.buf = append(l.buf, ']', ' ')
	}

	if l.flag&Lgoid != 0 {
		l.buf = append(l.buf, '[')
		itoa(&l.buf, int(goid.Get()), -1)
		l.buf = append(l.buf, ']', ' ')
	}

	if l.flag&(Lshortfile|Llongfile) != 0 {
		l.buf = append(l.buf, file...)
		l.buf = append(l.buf, ':')
		itoa(&l.buf, line, -1)
		l.buf = append(l.buf, ": "...)
	}

	if l.flag&Lcolor != 0 {
		l.buf = append(l.buf, colorPrefix...)
	}
	l.buf = append(l.buf, tag...)

	l.buf = append(l.buf, s...)

	if l.flag&Lcolor != 0 && colorPrefix != nil {
		l.buf = append(l.buf, colorSuffix...)
	}
	if l.buf[len(l.buf)-1] != '\n' {
		l.buf = append(l.buf, '\n')
	}

	l.out.Write(l.buf)
}

// Cheap integer to fixed-width decimal ASCII. Give a negative width to avoid zero-padding.
func itoa(buf *[]byte, i int, wid int) {
	// Assemble decimal in reverse order.
	var b [20]byte
	bp := len(b) - 1
	for i >= 10 || wid > 1 {
		wid--
		q := i / 10
		b[bp] = byte('0' + i - q*10)
		bp--
		i = q
	}
	// i < 10
	b[bp] = byte('0' + i)
	*buf = append(*buf, b[bp:]...)
}

var (
	panicColorPrefix = []byte("\033[1;31m")
	errorColorPrefix = []byte("\033[1;35m")
	warnColorPrefix  = []byte("\033[1;33m")
	debugColorPrefix = []byte("\033[32m")

	panicTag = []byte("[PANIC]")
	errorTag = []byte("[ERROR]")
	warnTag  = []byte("[WARN]")
	debugTag = []byte("[DEBUG]")

	colorSuffix = []byte("\033[0m")
)
