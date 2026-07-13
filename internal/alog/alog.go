// Package alog is a tiny process-wide debug logger. It is disabled (writes to
// io.Discard) until Open is called, so tests and normal runs stay silent unless
// a log file is requested.
package alog

import (
	"io"
	"log"
	"os"
	"runtime/debug"
	"sync"
)

var (
	mu   sync.Mutex
	l    = log.New(io.Discard, "", log.LstdFlags|log.Lmicroseconds)
	file *os.File
	path string
)

// Open directs logging to the given file (truncating it). Returns the absolute
// path actually used. Safe to call once at startup.
func Open(p string) (string, error) {
	f, err := os.Create(p)
	if err != nil {
		return "", err
	}
	mu.Lock()
	defer mu.Unlock()
	file = f
	path = p
	l.SetOutput(f)
	return p, nil
}

// Path returns the current log file path (empty if logging is off).
func Path() string {
	mu.Lock()
	defer mu.Unlock()
	return path
}

// Close flushes and closes the log file.
func Close() {
	mu.Lock()
	defer mu.Unlock()
	if file != nil {
		_ = file.Close()
		file = nil
		l.SetOutput(io.Discard)
	}
}

// Printf writes a formatted log line.
func Printf(format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	l.Printf(format, a...)
}

// Raw writes a category-tagged raw payload (e.g. a stream-json line) verbatim.
func Raw(tag, payload string) {
	mu.Lock()
	defer mu.Unlock()
	l.Printf("%s %s", tag, payload)
}

// Recover swallows a panic in a background goroutine and logs it with its stack.
// Deferred at the top of every goroutine we start: Bubble Tea only recovers
// panics raised inside its own loop, so a panic anywhere else would kill the
// process without unwinding its terminal restore — leaving the tty in raw mode
// with the alt-screen still on, which is exactly how a terminal ends up spewing
// control characters at the shell prompt afterwards.
func Recover(where string) {
	if r := recover(); r != nil {
		Printf("panic in %s: %v\n%s", where, r, debug.Stack())
	}
}
