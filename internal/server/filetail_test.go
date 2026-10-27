package server

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

type tailSink struct {
	mu     sync.Mutex
	chunks []*pb.FileChunk
}

func (s *tailSink) send(c *pb.FileChunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunks = append(s.chunks, &pb.FileChunk{Offset: c.Offset, Data: append([]byte(nil), c.Data...)})
	return nil
}

func (s *tailSink) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := ""
	for _, c := range s.chunks {
		out += string(c.Data)
	}
	return out
}

func waitText(t *testing.T, s *tailSink, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for s.text() != want {
		if time.Now().After(deadline) {
			t.Fatalf("got %q, want %q", s.text(), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTailFile(t *testing.T) {
	tailPoll = 20 * time.Millisecond
	dir := t.TempDir()
	p := filepath.Join(dir, "events.jsonl")
	// Without follow: a missing file is NotFound, a directory refused.
	if err := tailFile(context.Background(), p, 0, false, (&tailSink{}).send); status.Code(fileErrOr(err)) != codes.NotFound {
		t.Fatalf("missing: %v", err)
	}
	if err := tailFile(context.Background(), dir, 0, false, (&tailSink{}).send); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("directory: %v", err)
	}
	// Follow waits for the file, streams appends, restarts after truncation.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &tailSink{}
	done := make(chan error, 1)
	go func() { done <- tailFile(ctx, p, 0, true, s.send) }()
	time.Sleep(60 * time.Millisecond)
	os.WriteFile(p, []byte("a\n"), 0o644)
	waitText(t, s, "a\n")
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("bb\n")
	f.Close()
	waitText(t, s, "a\nbb\n")
	os.WriteFile(p, []byte("c\n"), 0o644) // truncated to fewer bytes
	waitText(t, s, "a\nbb\nc\n")
	s.mu.Lock()
	last := s.chunks[len(s.chunks)-1]
	s.mu.Unlock()
	if last.Offset != 0 {
		t.Fatalf("after truncation: offset %d", last.Offset)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Negative offset: from before the end.
	s2 := &tailSink{}
	if err := tailFile(context.Background(), p, -1, false, s2.send); err != nil || s2.text() != "\n" {
		t.Fatalf("offset -1: %q %v", s2.text(), err)
	}
}

func fileErrOr(err error) error {
	if _, ok := status.FromError(err); ok {
		return err
	}
	return fileErr(err)
}
