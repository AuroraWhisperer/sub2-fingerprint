//go:build windows

package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestLiveSocketPreventsIdleDisconnect(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := uint16(l.Addr().(*net.TCPAddr).Port)
	pid := uint32(os.Getpid())
	outbound, err := net.Dial("tcp4", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer outbound.Close()
	inbound, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer inbound.Close()
	state, err := portState(port)
	if err != nil || state.pid != pid || !state.connected {
		t.Fatal("did not identify owned connection", state, err)
	}
	finished := make(chan error, 1)
	go func() { finished <- waitForIdle(pid, port, 120*time.Millisecond, 15*time.Millisecond) }()
	select {
	case err := <-finished:
		t.Fatal("disconnected an active client", err)
	case <-time.After(250 * time.Millisecond):
	}
	outbound.Close()
	inbound.Close()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not disconnect idle tunnel")
	}
}

func TestOnlyMatchingNamedInstanceCanBeReused(t *testing.T) {
	name := `Local\Sub2DetectorTest-` + strconv.Itoa(os.Getpid())
	if markerExists(name) {
		t.Fatal("unexpected initial instance")
	}
	first, exists, err := namedMutex(name)
	if err != nil || exists {
		t.Fatal("first instance", exists, err)
	}
	second, exists, err := namedMutex(name)
	if err != nil || !exists {
		t.Fatal("second instance", exists, err)
	}
	if !markerExists(name) || markerExists(name+"-different-target") {
		t.Fatal("wrong instance marker")
	}
	syscall.CloseHandle(second)
	syscall.CloseHandle(first)
	if markerExists(name) {
		t.Fatal("marker survived owning processes")
	}
}

func TestLauncherChecksHomeAndDoesNotFollowRedirects(t *testing.T) {
	health := `{"status":"ok","version":"test"}`
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte(health))
			return
		}
		w.Write([]byte("page"))
	}))
	defer healthy.Close()
	if err := checkPage(healthy.URL + "/"); err != nil {
		t.Fatal(err)
	}
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("followed a redirect outside the detector") }))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte(health))
			return
		}
		http.Redirect(w, r, other.URL, 302)
	}))
	defer redirect.Close()
	if err := checkPage(redirect.URL + "/"); err == nil {
		t.Fatal("accepted redirected home page")
	}
}

func TestUnexpectedExitCleansOnlyOwnedProcess(t *testing.T) {
	if os.Getenv("SUB2_TEST_LAUNCHER_CHILD") == "1" {
		time.Sleep(20 * time.Second)
		return
	}
	child := exec.Command(os.Args[0], "-test.run=^TestUnexpectedExitCleansOnlyOwnedProcess$")
	child.Env = append(os.Environ(), "SUB2_TEST_LAUNCHER_CHILD=1")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	job, err := ownChild(child.Process.Pid)
	if err != nil {
		child.Process.Kill()
		child.Wait()
		t.Fatal(err)
	}
	if err = syscall.CloseHandle(job); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case <-done:
		// Windows may report exit code 0 when kill-on-close ends a job.
	case <-time.After(3 * time.Second):
		t.Fatal("child outlived the launcher's job handle")
	}
}
