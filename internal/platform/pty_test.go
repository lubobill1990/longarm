package platform

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ptyOutput collects everything read from p in the background.
type ptyOutput struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	done chan struct{}
}

func readAll(p PTY) *ptyOutput {
	o := &ptyOutput{done: make(chan struct{})}
	go func() {
		defer close(o.done)
		b := make([]byte, 4096)
		for {
			n, err := p.Read(b)
			o.mu.Lock()
			o.buf.Write(b[:n])
			o.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return o
}

func (o *ptyOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func (o *ptyOutput) waitFor(t *testing.T, s string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(o.String(), s) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q in %q", s, o.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPTYInteractive(t *testing.T) {
	ctr, err := NewContainer(ContainerOptions{Name: "pty-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer ctr.Close()
	argv := []string{"/bin/sh"}
	if runtime.GOOS == "windows" {
		argv = []string{"cmd.exe", "/q", "/k", "prompt $G"}
	}
	p, err := StartPTY(ctr, PTYOptions{Argv: argv, Env: os.Environ(), Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release()
	out := readAll(p)
	if err := p.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	io.WriteString(p, "echo hello-pty\r")
	out.waitFor(t, "hello-pty")
	io.WriteString(p, "exit 3\r")
	code, err := p.Wait()
	if err != nil || code != 3 {
		t.Fatalf("exit %d %v", code, err)
	}
	p.Close()
	ctr.Kill()
	select {
	case <-out.done:
	case <-time.After(10 * time.Second):
		t.Fatal("output did not reach EOF after exit")
	}
}

func TestLocalChannel(t *testing.T) {
	b := make([]byte, 8)
	rand.Read(b)
	ln, addr, err := ListenLocal("test-" + hex.EncodeToString(b))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		io.Copy(c, c)
		c.Close()
	}()
	c, err := DialLocal(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "ping")
	got := make([]byte, 4)
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "ping" {
		t.Fatalf("%q %v", got, err)
	}
}
