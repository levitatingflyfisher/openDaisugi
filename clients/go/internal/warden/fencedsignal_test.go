package warden

import (
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// holdSocket is a resident warden that takes the request and never answers.
// got is closed once a request is in.
func holdSocket(t *testing.T) (root string, got chan struct{}) {
	t.Helper()
	root, err := os.MkdirTemp("", "fs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	sock := root + "/warden.sock"
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		t.Fatal(err)
	}
	got = make(chan struct{}, 8)
	var held []net.Conn
	done := make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
			buf := make([]byte, 65536)
			if n, _ := c.Read(buf); n > 0 {
				got <- struct{}{}
			}
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		for _, c := range held {
			c.Close()
		}
		close(done)
	})
	return root, got
}

func fencedEnv(t *testing.T) []string {
	return []string{"DAISUGI_FENCED=1", "HOME=" + t.TempDir()}
}

func TestAFencedCheckEndsInTheModesVerdictOnASignal(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGTERM, syscall.SIGINT} {
		for _, mode := range []string{"enforce", "audit"} {
			root, got := holdSocket(t)
			argv := []string{"--mode", mode, "--root", root, "--format", "claude"}
			sigCh := make(chan os.Signal, 1)
			go func() {
				select {
				case <-got:
					sigCh <- sig
				case <-time.After(20 * time.Second):
				}
			}()
			done := make(chan Result, 1)
			go func() { done <- fencedCheckWith(argv, []byte("{}"), fencedEnv(t), sigCh) }()
			var res Result
			select {
			case res = <-done:
			case <-time.After(30 * time.Second):
				t.Fatalf("%s %s: the check did not end on the signal", mode, sig)
			}
			if mode == "enforce" {
				if res.Exit != 2 || !strings.Contains(res.Stderr, "DENIED") {
					t.Errorf("%s %s: got exit %d, %q", mode, sig, res.Exit, res.Stderr)
				}
			} else if res.Exit != 0 || strings.Contains(res.Stderr, "DENIED") {
				t.Errorf("%s %s: got exit %d, %q", mode, sig, res.Exit, res.Stderr)
			}
		}
	}
}

// A real signal reaches the watch FencedCheck arms itself.
func TestAFencedCheckWatchesTheProcessSignals(t *testing.T) {
	root, got := holdSocket(t)
	argv := []string{"--mode", "enforce", "--root", root, "--format", "claude"}
	go func() {
		<-got
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}()
	done := make(chan Result, 1)
	go func() { done <- FencedCheck(argv, []byte("{}"), fencedEnv(t)) }()
	select {
	case res := <-done:
		if res.Exit != 2 || !strings.Contains(res.Stderr, "DENIED") {
			t.Fatalf("got exit %d, %q", res.Exit, res.Stderr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the check did not end on SIGTERM")
	}
}

// With no mode flag and no config the mode is audit (ModeFor), so a signal
// allows: the same verdict as the guarded parent.
func TestAFencedSignalWithNoModeFlagAndNoConfigAllows(t *testing.T) {
	root, got := holdSocket(t)
	argv := []string{"--root", root, "--format", "claude"}
	sigCh := make(chan os.Signal, 1)
	go func() {
		<-got
		sigCh <- syscall.SIGTERM
	}()
	res := fencedCheckWith(argv, []byte("{}"), fencedEnv(t), sigCh)
	if res.Exit != 0 || strings.Contains(res.Stderr, "DENIED") {
		t.Fatalf("got exit %d, %q", res.Exit, res.Stderr)
	}
}
