package cli

import (
	"fmt"
	"io"
	"time"
)

type Logger struct {
	output io.Writer
	debug  bool
	info   bool
	clean  bool
}

func NewLogger(w io.Writer, level string, clean bool) *Logger {
	return &Logger{output: w, debug: level == "DEBUG", info: level == "DEBUG" || level == "INFO", clean: clean}
}

func (l *Logger) Info(format string, args ...any) {
	if l.info {
		l.print("INFO", format, args...)
	}
}

func (l *Logger) Debug(format string, args ...any) {
	if l.debug {
		l.print("DEBUG", format, args...)
	}
}

func (l *Logger) print(level, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	if l.clean {
		fmt.Fprintln(l.output, message)
	} else {
		fmt.Fprintf(l.output, "%s %s: %s\n", time.Now().Format("2006-01-02 15:04:05,000"), level, message)
	}
}
