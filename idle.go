package main

import (
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

func detectorListener(addr string) (net.Listener, bool, error) {
	pid, fds := os.Getenv("LISTEN_PID"), os.Getenv("LISTEN_FDS")
	if pid == "" && fds == "" {
		l, err := net.Listen("tcp4", addr)
		return l, false, err
	}
	if pid != strconv.Itoa(os.Getpid()) || fds != "1" {
		return nil, false, errors.New("invalid inherited listener")
	}
	file := os.NewFile(3, "sub2-fingerprint.socket")
	if file == nil {
		return nil, false, errors.New("missing inherited socket")
	}
	defer file.Close()
	l, err := net.FileListener(file)
	if err != nil {
		return nil, false, err
	}
	if l.Addr().Network() != "tcp" || l.Addr().String() != addr {
		l.Close()
		return nil, false, errors.New("unexpected inherited socket address")
	}
	return l, true, nil
}

type idleTracker struct {
	mu       sync.Mutex
	last     time.Time
	requests int
}

func newIdleTracker() *idleTracker { return &idleTracker{last: time.Now()} }
func (t *idleTracker) track(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.mu.Lock()
		t.requests++
		t.mu.Unlock()
		defer func() { t.mu.Lock(); t.requests--; t.last = time.Now(); t.mu.Unlock() }()
		handler.ServeHTTP(w, r)
	})
}
func (t *idleTracker) idle(now time.Time, grace time.Duration, busy bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if busy || t.requests != 0 {
		t.last = now
		return false
	}
	return now.Sub(t.last) >= grace
}
