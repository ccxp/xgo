package xlog

import (
	"fmt"
)

type Log struct {
	// 自定义logger，可为空，空则取默认logger
	Logger Logger

	// 取调用信息的深度，默认是0
	CallerDept int

	level *int
}

// 设置自己的LogLevel，不影响Log.Logger的设置
func (l *Log) SetLevel(level int) {

	l.level = &level
}

func (l *Log) getLogger() Logger {
	if l.Logger != nil {
		return l.Logger
	}
	return defaultLogger
}

func (l *Log) GetLevel() int {
	if l.level != nil {
		return *l.level
	}
	if l.Logger != nil {
		return l.Logger.GetLevel()
	}
	return defaultLogger.GetLevel()
}

func (l *Log) Debugf(format string, v ...interface{}) {
	if l.GetLevel() < Ldebug {
		return
	}

	l.getLogger().Output(Ldebug, 1+l.CallerDept, fmt.Sprintf(format, v...))
}

func (l *Log) Warnf(format string, v ...interface{}) {

	if l.GetLevel() < Lwarn {
		return
	}
	l.getLogger().Output(Lwarn, 1+l.CallerDept, fmt.Sprintf(format, v...))
}

func (l *Log) Errorf(format string, v ...interface{}) {
	if l.GetLevel() < Lerror {
		return
	}

	l.getLogger().Output(Lerror, 1+l.CallerDept, fmt.Sprintf(format, v...))
}

func (l *Log) Panicf(format string, v ...interface{}) {

	s := fmt.Sprintf(format, v...)
	if l.GetLevel() >= Lpanic {
		l.getLogger().Output(Lpanic, 1+l.CallerDept, s)
	}
	panic(s)
}

func (l *Log) Debug(v ...interface{}) {

	if l.GetLevel() < Ldebug {
		return
	}

	l.getLogger().Output(Ldebug, 1+l.CallerDept, fmt.Sprint(v...))
}

func (l *Log) Warn(v ...interface{}) {

	if l.GetLevel() < Lwarn {
		return
	}

	l.getLogger().Output(Lwarn, 1+l.CallerDept, fmt.Sprint(v...))
}

func (l *Log) Error(v ...interface{}) {

	if l.GetLevel() < Lerror {
		return
	}

	l.getLogger().Output(Lerror, 1+l.CallerDept, fmt.Sprint(v...))
}

func (l *Log) Panic(v ...interface{}) {
	s := fmt.Sprint(v...)
	if l.GetLevel() >= Lpanic {
		l.getLogger().Output(Lpanic, 1+l.CallerDept, s)
	}
	panic(s)
}

func (l *Log) Output(level, calldepth int, s string) {
	if l.GetLevel() >= level {
		l.getLogger().Output(level, calldepth+1+l.CallerDept, s)
	}
	if level == Lpanic {
		panic(s)
	}
}

func (l *Log) Outputf(level, calldepth int, format string, v ...interface{}) {

	s := fmt.Sprintf(format, v...)
	if l.GetLevel() >= level {
		l.getLogger().Output(level, calldepth+1+l.CallerDept, s)
	}
	if level == Lpanic {
		panic(s)
	}
}
