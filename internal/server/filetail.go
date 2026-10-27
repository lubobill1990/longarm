package server

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/platform"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// tailPoll is how often Files.Tail looks for appended bytes.
var tailPoll = 250 * time.Millisecond

// tailAbsentTimeout ends a follow of a file that does not exist with
// NOT_FOUND after this long (it would hold a user helper forever).
var tailAbsentTimeout = time.Hour

// Defaults of Server.MaxTails / MaxTailsPerUser ([files] in longarmd.toml).
const (
	DefaultMaxTails        = 64
	DefaultMaxTailsPerUser = 8
)

// tailCounter counts open tails, per user and in total.
type tailCounter struct {
	mu    sync.Mutex
	total int
	users map[string]int
}

func limitOf(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// acquire takes a tail slot for user (RESOURCE_EXHAUSTED when none is
// left); release gives it back.
func (s *Server) acquireTail(user string) (release func(), err error) {
	c := &s.tails
	maxAll, maxUser := limitOf(s.MaxTails, DefaultMaxTails), limitOf(s.MaxTailsPerUser, DefaultMaxTailsPerUser)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.users == nil {
		c.users = map[string]int{}
	}
	switch {
	case maxUser > 0 && c.users[user] >= maxUser:
		return nil, status.Errorf(codes.ResourceExhausted, "%d tails of %s are open on this worker (max_tails_per_user); retry later", c.users[user], user)
	case maxAll > 0 && c.total >= maxAll:
		return nil, status.Errorf(codes.ResourceExhausted, "%d tails are open on this worker (max_tails); retry later", c.total)
	}
	c.users[user]++
	c.total++
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.total--
			if c.users[user]--; c.users[user] <= 0 {
				delete(c.users, user)
			}
		})
	}, nil
}

// Tail streams a file like Read and, with follow, what is appended to it
// (longarm tail). Same paths and access as Read: with per-user isolation
// the user's helper reads as the user's account.
func (f *filesServer) Tail(req *pb.FileTailRequest, st pb.Files_TailServer) error {
	release, err := f.s.acquireTail(IdentityFrom(st.Context()).Owner())
	if err != nil {
		return err
	}
	defer release()
	if f.s.perUser() {
		return f.proxyTail(req, st)
	}
	abs, err := resolvePath(req.Path)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	f.s.Audit.Log(IdentityFrom(st.Context()), pb.Files_Tail_FullMethodName, map[string]any{"path": abs, "offset": req.Offset, "follow": req.Follow})
	return tailFile(st.Context(), abs, req.Offset, req.Follow, st.Send)
}

func (f *filesServer) proxyTail(req *pb.FileTailRequest, st pb.Files_TailServer) error {
	ctx := st.Context()
	h, err := f.s.helper(ctx)
	if err != nil {
		return err
	}
	defer h.Close()
	f.s.Audit.Log(IdentityFrom(ctx), pb.Files_Tail_FullMethodName, map[string]any{"path": req.Path, "offset": req.Offset, "follow": req.Follow})
	rs, err := pb.NewFilesClient(h.Conn).Tail(ctx, req)
	if err != nil {
		return err
	}
	for {
		c, err := rs.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := st.Send(c); err != nil {
			return err
		}
	}
}

// tailFile sends path from off (negative: before the end) to its end and,
// with follow, then what is appended until ctx ends. A missing file is
// waited for with follow; a file that was truncated or replaced is read
// again from 0.
func tailFile(ctx context.Context, path string, off int64, follow bool, send func(*pb.FileChunk) error) error {
	var fh *os.File
	defer func() {
		if fh != nil {
			fh.Close()
		}
	}()
	open := func() error {
		if fi, err := os.Stat(path); err == nil && fi.IsDir() { // Windows: opening one is "access denied"
			return status.Errorf(codes.InvalidArgument, "%s is a directory", path)
		}
		f, err := platform.OpenShared(path)
		if err != nil {
			return err
		}
		if fi, err := f.Stat(); err == nil && fi.IsDir() {
			f.Close()
			return status.Errorf(codes.InvalidArgument, "%s is a directory", path)
		}
		fh = f
		return nil
	}
	sleep := func() bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(tailPoll):
			return true
		}
	}
	absentSince := time.Now()
	for {
		err := open()
		if err == nil {
			break
		}
		if !follow || !errors.Is(err, fs.ErrNotExist) {
			if _, ok := status.FromError(err); ok {
				return err
			}
			return fileErr(err)
		}
		if time.Since(absentSince) > tailAbsentTimeout {
			return status.Errorf(codes.NotFound, "%s did not appear within %s", path, tailAbsentTimeout)
		}
		if !sleep() {
			return nil
		}
	}
	if off < 0 {
		if fi, err := fh.Stat(); err == nil {
			off = max(fi.Size()+off, 0)
		} else {
			off = 0
		}
	}
	buf := make([]byte, fileChunk)
	for {
		for {
			n, err := fh.ReadAt(buf, off)
			if n > 0 {
				if err := send(&pb.FileChunk{Offset: off, Data: buf[:n]}); err != nil {
					return err
				}
				off += int64(n)
			}
			if errors.Is(err, io.EOF) || (err == nil && n == 0) {
				break
			}
			if err != nil {
				return fileErr(err)
			}
		}
		if !follow || !sleep() {
			return nil
		}
		fi, err := os.Stat(path)
		if err != nil {
			continue // gone for now (rotation): keep reading the open file
		}
		cur, err := fh.Stat()
		switch {
		case err != nil || !os.SameFile(fi, cur): // replaced: the new file from its start
			fh.Close()
			fh = nil
			if err := open(); err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					return fileErr(err)
				}
				gone := time.Now()
				for fh == nil { // replaced, then removed again: wait for it
					if time.Since(gone) > tailAbsentTimeout {
						return status.Errorf(codes.NotFound, "%s did not appear again within %s", path, tailAbsentTimeout)
					}
					if !sleep() {
						return nil
					}
					if err := open(); err != nil && !errors.Is(err, fs.ErrNotExist) {
						return fileErr(err)
					}
				}
			}
			off = 0
		case cur.Size() < off: // truncated
			off = 0
		}
	}
}
