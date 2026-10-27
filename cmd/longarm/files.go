package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/client"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// partialSuffix marks an incomplete download (same as the worker side).
const partialSuffix = ".lapart"

const transferChunk = 256 << 10

// splitRemote parses "<worker>:<path>". Worker names come from workers.toml
// or are ad-hoc "certname@host:port"; a one-letter prefix is a Windows drive
// letter unless a worker of that name exists.
func splitRemote(cfg *client.Config, arg string) (worker, p string, ok bool) {
	if at := strings.Index(arg, "@"); at >= 0 {
		rest := arg[at+1:]
		// host:port:path (host may be [v6])
		if strings.HasPrefix(rest, "[") {
			if i := strings.Index(rest, "]:"); i >= 0 {
				rest2 := rest[i+2:]
				if j := strings.Index(rest2, ":"); j >= 0 {
					return arg[:at+1+i+2+j], rest2[j+1:], true
				}
			}
			return "", "", false
		}
		if i := strings.Index(rest, ":"); i >= 0 {
			if j := strings.Index(rest[i+1:], ":"); j >= 0 {
				return arg[:at+1+i+1+j], rest[i+1+j+1:], true
			}
		}
		return "", "", false
	}
	i := strings.Index(arg, ":")
	if i <= 0 {
		return "", "", false
	}
	name := arg[:i]
	if len(name) == 1 {
		found := false
		for _, w := range cfg.Workers {
			found = found || w.Name == name
		}
		if !found {
			return "", "", false // C:\path
		}
	}
	return name, arg[i+1:], true
}

type transferStats struct {
	files, skipped int
	bytes          int64
}

func putCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "put <local>... <worker>:<path>",
		Short: "Upload files/directories (streamed, resumable, sha256-verified)",
		Long: `Upload files or directories to the worker. Like cp -r: if <path> is an existing
directory (or ends with /), sources go inside it. Relative worker paths are
relative to the worker account's home. Files whose size and sha256 already
match are skipped; an interrupted upload resumes from the verified prefix of
<path>.lapart on the next run (and automatically after a connection loss).`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			worker, dst, ok := splitRemote(s.cfg, args[len(args)-1])
			if !ok {
				return usagef("last argument must be <worker>:<path>")
			}
			conn, err := s.conn(worker)
			if err != nil {
				return err
			}
			defer conn.Close()
			ctx := cmd.Context()
			srcs := args[:len(args)-1]
			dstInfo, err := remoteStat(ctx, conn, dst, false, 0, false)
			if err != nil {
				return err
			}
			intoDir := dstInfo.IsDir || strings.HasSuffix(dst, "/") || strings.HasSuffix(dst, `\`)
			if len(srcs) > 1 && !intoDir {
				return usagef("%s:%s is not a directory", worker, dst)
			}
			var st transferStats
			start := time.Now()
			for _, src := range srcs {
				fi, err := os.Stat(src)
				if err != nil {
					return err
				}
				target := strings.TrimRight(dst, `/\`)
				if intoDir {
					target = path.Join(filepath.ToSlash(target), filepath.Base(src))
					if dst == "" || dst == "/" {
						target = filepath.Base(src)
					}
				}
				if fi.IsDir() {
					err = putDir(ctx, conn, src, target, &st)
				} else {
					err = putFile(ctx, conn, src, target, fi, &st)
				}
				if err != nil {
					return err
				}
			}
			fmt.Fprintf(os.Stderr, "longarm: %d file(s) uploaded (%s in %s), %d up to date\n", st.files, humanSize(st.bytes),
				time.Since(start).Round(time.Millisecond), st.skipped)
			return nil
		},
	}
	return c
}

func getCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <worker>:<path> <local>",
		Short: "Download a file/directory (streamed, resumable, sha256-verified)",
		Long: `Download a file or directory from the worker. Like cp -r: if <local> is an
existing directory, the source goes inside it. Files whose size and sha256
already match are skipped; an interrupted download resumes from the verified
prefix of <local>.lapart.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			worker, src, ok := splitRemote(s.cfg, args[0])
			if !ok {
				return usagef("first argument must be <worker>:<path>")
			}
			conn, err := s.conn(worker)
			if err != nil {
				return err
			}
			defer conn.Close()
			ctx := cmd.Context()
			info, err := remoteStat(ctx, conn, src, false, 0, false)
			if err != nil {
				return err
			}
			if !info.Exists {
				return notFoundf("%s:%s does not exist", worker, src)
			}
			dst := args[1]
			if fi, err := os.Stat(dst); err == nil && fi.IsDir() {
				base := path.Base(strings.ReplaceAll(info.AbsPath, `\`, "/"))
				if dst, err = safeJoin(dst, base); err != nil {
					return err
				}
			}
			var st transferStats
			start := time.Now()
			if info.IsDir {
				err = getDir(ctx, conn, src, dst, &st)
			} else {
				err = getFile(ctx, conn, src, dst, info, &st)
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "longarm: %d file(s) downloaded (%s in %s), %d up to date\n", st.files, humanSize(st.bytes),
				time.Since(start).Round(time.Millisecond), st.skipped)
			return nil
		},
	}
	return c
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func remoteStat(ctx context.Context, conn *client.Conn, p string, partial bool, limit int64, hash bool) (*pb.FileInfo, error) {
	var info *pb.FileInfo
	err := client.Retry(ctx, time.Minute, notifier(conn.Worker.Name), func(ctx context.Context, _ func()) error {
		c, cancel := context.WithTimeout(ctx, 10*time.Minute) // hashing a big file takes a while
		defer cancel()
		var err error
		info, err = conn.Files().Stat(c, &pb.FileStatRequest{Path: p, Partial: partial, Hash: hash, HashLimit: limit})
		return err
	})
	return info, err
}

func hashLocal(p string, limit int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	var r io.Reader = f
	if limit > 0 {
		r = io.LimitReader(f, limit)
	}
	n, err := io.Copy(h, r)
	if err != nil {
		return nil, err
	}
	if limit > 0 && n != limit {
		return nil, fmt.Errorf("%s: short read", p)
	}
	return h.Sum(nil), nil
}

func putDir(ctx context.Context, conn *client.Conn, src, dst string, st *transferStats) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := dst
		if rel != "." {
			target = path.Join(dst, filepath.ToSlash(rel))
		}
		switch {
		case d.IsDir():
			_, err := conn.Files().Mkdir(ctx, &pb.MkdirRequest{Path: target})
			return err
		case d.Type()&fs.ModeSymlink != 0:
			fmt.Fprintf(os.Stderr, "longarm: skipping symlink %s\n", p)
			return nil
		case !d.Type().IsRegular():
			fmt.Fprintf(os.Stderr, "longarm: skipping special file %s\n", p)
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		return putFile(ctx, conn, p, target, fi, st)
	})
}

// putFile uploads one file, skipping it when the worker already has the same
// content and resuming a verified partial upload.
func putFile(ctx context.Context, conn *client.Conn, src, dst string, fi fs.FileInfo, st *transferStats) error {
	sum, err := hashLocal(src, 0)
	if err != nil {
		return err
	}
	size := fi.Size()
	if cur, err := remoteStat(ctx, conn, dst, false, 0, true); err != nil {
		return err
	} else if cur.Exists && !cur.IsDir && cur.Size == size && bytes.Equal(cur.Sha256, sum) {
		st.skipped++
		return nil
	}
	return client.Retry(ctx, 10*time.Minute, notifier(conn.Worker.Name), func(ctx context.Context, progress func()) error {
		var offset int64
		if part, err := remoteStat(ctx, conn, dst, true, 0, true); err != nil {
			return err
		} else if part.Exists && part.Size > 0 && part.Size <= size {
			if local, err := hashLocal(src, part.Size); err == nil && bytes.Equal(local, part.Sha256) {
				offset = part.Size
				fmt.Fprintf(os.Stderr, "longarm: resuming %s at %s\n", dst, humanSize(offset))
			}
		}
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		wst, err := conn.Files().Write(ctx)
		if err != nil {
			return err
		}
		hdr := &pb.FileWriteHeader{Path: dst, Offset: offset, Size: size, Sha256: sum,
			Mode: uint32(fi.Mode().Perm()), Mtime: timestamppb.New(fi.ModTime())}
		if err := wst.Send(&pb.FileWriteRequest{Msg: &pb.FileWriteRequest_Header{Header: hdr}}); err != nil {
			_, rerr := wst.CloseAndRecv()
			return firstErr(rerr, err)
		}
		buf := make([]byte, transferChunk)
		sent := offset
		for {
			n, rerr := f.Read(buf)
			if n > 0 {
				if err := wst.Send(&pb.FileWriteRequest{Msg: &pb.FileWriteRequest_Data{Data: buf[:n]}}); err != nil {
					_, cerr := wst.CloseAndRecv()
					return firstErr(cerr, err)
				}
				sent += int64(n)
				progress()
			}
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				wst.CloseSend()
				return rerr
			}
		}
		if _, err := wst.CloseAndRecv(); err != nil {
			return err
		}
		st.files++
		st.bytes += sent - offset
		return nil
	})
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil && !errors.Is(e, io.EOF) {
			return e
		}
	}
	return errors.New("stream closed")
}

func getDir(ctx context.Context, conn *client.Conn, src, dst string, st *transferStats) error {
	list, err := conn.Files().List(ctx, &pb.FileListRequest{Path: src})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, e := range list.Entries {
		target, err := safeJoin(dst, e.Path)
		if err != nil {
			return err
		}
		switch {
		case e.IsSymlink:
			fmt.Fprintf(os.Stderr, "longarm: skipping symlink %s\n", e.Path)
		case e.IsDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		default:
			if err := getFile(ctx, conn, path.Join(filepath.ToSlash(list.AbsPath), e.Path), target, e, st); err != nil {
				return err
			}
		}
	}
	return nil
}

// getFile downloads one file to dst via dst.lapart, skipping identical
// files and resuming a verified partial download.
func getFile(ctx context.Context, conn *client.Conn, src, dst string, info *pb.FileInfo, st *transferStats) error {
	full, err := remoteStat(ctx, conn, src, false, 0, true)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(dst); err == nil && !fi.IsDir() && fi.Size() == full.Size {
		if local, err := hashLocal(dst, 0); err == nil && bytes.Equal(local, full.Sha256) {
			st.skipped++
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	part := dst + partialSuffix
	err = client.Retry(ctx, 10*time.Minute, notifier(conn.Worker.Name), func(ctx context.Context, progress func()) error {
		var offset int64
		if fi, err := os.Stat(part); err == nil && fi.Size() > 0 && fi.Size() <= full.Size {
			rp, err := remoteStat(ctx, conn, src, false, fi.Size(), true)
			if err != nil {
				return err
			}
			if local, err := hashLocal(part, fi.Size()); err == nil && bytes.Equal(local, rp.Sha256) {
				offset = fi.Size()
				fmt.Fprintf(os.Stderr, "longarm: resuming %s at %s\n", dst, humanSize(offset))
			}
		}
		f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := f.Truncate(offset); err != nil {
			return err
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		rst, err := conn.Files().Read(ctx, &pb.FileReadRequest{Path: src, Offset: offset})
		if err != nil {
			return err
		}
		got := offset
		for {
			ch, err := rst.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if ch.Offset != got {
				return status.Errorf(codes.DataLoss, "unexpected offset %d (want %d)", ch.Offset, got)
			}
			if _, err := f.Write(ch.Data); err != nil {
				return err
			}
			got += int64(len(ch.Data))
			progress()
		}
		st.bytes += got - offset
		return f.Sync()
	})
	if err != nil {
		return err
	}
	sum, err := hashLocal(part, 0)
	if err != nil {
		return err
	}
	if !bytes.Equal(sum, full.Sha256) {
		os.Remove(part)
		return fmt.Errorf("%s: sha256 mismatch after download (the file changed on the worker?); retry", dst)
	}
	if err := os.Rename(part, dst); err != nil {
		os.Remove(dst)
		if err := os.Rename(part, dst); err != nil {
			return err
		}
	}
	if info.Mtime != nil {
		os.Chtimes(dst, time.Now(), info.Mtime.AsTime())
	}
	if m := os.FileMode(info.Mode).Perm(); m != 0 {
		os.Chmod(dst, m)
	}
	st.files++
	return nil
}

// safeJoin joins a worker-supplied relative path onto a local directory,
// refusing anything that could land outside it (a compromised worker must
// not be able to write ../../.bashrc on the client).
func safeJoin(dir, rel string) (string, error) {
	// On Windows "\" is a separator and ":" selects a drive or stream.
	if rel == "" || path.IsAbs(rel) || (runtime.GOOS == "windows" && strings.ContainsAny(rel, `\:`)) {
		return "", fmt.Errorf("unsafe path %q from worker", rel)
	}
	clean := path.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe path %q from worker", rel)
	}
	target := filepath.Join(dir, filepath.FromSlash(clean))
	if r, err := filepath.Rel(dir, target); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe path %q from worker", rel)
	}
	return target, nil
}
