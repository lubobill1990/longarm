package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/platform"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// PartialSuffix marks an incomplete upload.
const PartialSuffix = ".lapart"

const fileChunk = 256 << 10

type filesServer struct {
	pb.UnimplementedFilesServer
	s *Server
}

// resolvePath maps a client path to an absolute worker path: relative paths
// (and "") are relative to the home directory of longarmd's account; "/" works
// as a separator on every OS.
func resolvePath(p string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if p == "" || p == "~" {
		return home, nil
	}
	if len(p) >= 2 && p[0] == '~' && (p[1] == '/' || p[1] == '\\') {
		p = p[2:]
	}
	p = filepath.FromSlash(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(home, p)
	}
	return filepath.Clean(p), nil
}

func fileErr(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, fs.ErrPermission):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, fs.ErrExist):
		return status.Error(codes.AlreadyExists, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}

func infoOf(name, abs string, fi fs.FileInfo) *pb.FileInfo {
	return &pb.FileInfo{
		Path: name, AbsPath: abs, Exists: true, IsDir: fi.IsDir(), IsSymlink: fi.Mode()&fs.ModeSymlink != 0,
		Size: fi.Size(), Mode: uint32(fi.Mode().Perm()), Mtime: timestamppb.New(fi.ModTime()),
	}
}

// hashFile returns the sha256 of the first limit bytes (limit <= 0: all).
func hashFile(p string, limit int64) ([]byte, error) {
	f, err := platform.OpenShared(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	var r io.Reader = f
	if limit > 0 {
		r = io.LimitReader(f, limit)
	}
	if _, err := io.Copy(h, r); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

func (f *filesServer) Stat(ctx context.Context, req *pb.FileStatRequest) (*pb.FileInfo, error) {
	if f.s.perUser() {
		return f.proxyStat(ctx, req)
	}
	abs, err := resolvePath(req.Path)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	target := abs
	if req.Partial {
		target += PartialSuffix
	}
	fi, err := os.Stat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return &pb.FileInfo{Path: req.Path, AbsPath: abs}, nil
	}
	if err != nil {
		return nil, fileErr(err)
	}
	out := infoOf(req.Path, abs, fi)
	if req.Hash && !fi.IsDir() {
		if out.Sha256, err = hashFile(target, req.HashLimit); err != nil {
			return nil, fileErr(err)
		}
	}
	return out, nil
}

func (f *filesServer) List(ctx context.Context, req *pb.FileListRequest) (*pb.FileListResponse, error) {
	if f.s.perUser() {
		return f.proxyList(ctx, req)
	}
	abs, err := resolvePath(req.Path)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	out := &pb.FileListResponse{AbsPath: abs}
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == abs {
			if !d.IsDir() {
				return fmt.Errorf("%s is not a directory", abs)
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(abs, p)
		out.Entries = append(out.Entries, infoOf(filepath.ToSlash(rel), p, fi))
		if len(out.Entries) > 1_000_000 {
			return errors.New("too many entries")
		}
		return nil
	})
	if err != nil {
		return nil, fileErr(err)
	}
	return out, nil
}

func (f *filesServer) Read(req *pb.FileReadRequest, st pb.Files_ReadServer) error {
	if f.s.perUser() {
		return f.proxyRead(req, st)
	}
	abs, err := resolvePath(req.Path)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	fh, err := platform.OpenShared(abs)
	if err != nil {
		return fileErr(err)
	}
	defer fh.Close()
	if fi, err := fh.Stat(); err == nil && fi.IsDir() {
		return status.Errorf(codes.InvalidArgument, "%s is a directory", abs)
	}
	f.s.Audit.Log(IdentityFrom(st.Context()), pb.Files_Read_FullMethodName, map[string]any{"path": abs, "offset": req.Offset})
	off := req.Offset
	buf := make([]byte, fileChunk)
	for {
		n, err := fh.ReadAt(buf, off)
		if n > 0 {
			if err := st.Send(&pb.FileChunk{Offset: off, Data: buf[:n]}); err != nil {
				return err
			}
			off += int64(n)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fileErr(err)
		}
	}
}

func (f *filesServer) Write(st pb.Files_WriteServer) error {
	ctx := st.Context()
	first, err := st.Recv()
	if err != nil {
		return err
	}
	h := first.GetHeader()
	if h == nil {
		return status.Error(codes.InvalidArgument, "first message must be the header")
	}
	if f.s.perUser() {
		return f.proxyWrite(st, first)
	}
	abs, err := resolvePath(h.Path)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	res, err := f.write(st, h, abs)
	f.s.Audit.Log(IdentityFrom(ctx), pb.Files_Write_FullMethodName, map[string]any{
		"path": abs, "offset": h.Offset, "size": h.Size, "error": errString(err),
	})
	if err != nil {
		return err
	}
	return st.SendAndClose(res)
}

func (f *filesServer) write(st pb.Files_WriteServer, h *pb.FileWriteHeader, abs string) (*pb.FileWriteResult, error) {
	if h.Offset < 0 || h.Size < 0 || h.Offset > h.Size {
		return nil, status.Error(codes.InvalidArgument, "bad offset/size")
	}
	if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
		return nil, status.Errorf(codes.FailedPrecondition, "%s is a directory", abs)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, fileErr(err)
	}
	part := abs + PartialSuffix
	fh, err := os.OpenFile(part, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fileErr(err)
	}
	ok := false
	defer func() {
		fh.Close()
		if !ok {
			// keep a consistent prefix for resuming, unless it is unusable
			if fi, err := os.Stat(part); err == nil && fi.Size() == 0 {
				os.Remove(part)
			}
		}
	}()
	fi, err := fh.Stat()
	if err != nil {
		return nil, fileErr(err)
	}
	if fi.Size() < h.Offset {
		return nil, status.Errorf(codes.FailedPrecondition, "partial upload has %d bytes, cannot resume at %d", fi.Size(), h.Offset)
	}
	if err := fh.Truncate(h.Offset); err != nil {
		return nil, fileErr(err)
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, io.NewSectionReader(fh, 0, h.Offset)); err != nil {
		return nil, fileErr(err)
	}
	if _, err := fh.Seek(h.Offset, io.SeekStart); err != nil {
		return nil, fileErr(err)
	}
	size := h.Offset
	for {
		msg, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fh.Sync()
			return nil, err
		}
		data := msg.GetData()
		if size+int64(len(data)) > h.Size {
			return nil, status.Error(codes.InvalidArgument, "more data than announced")
		}
		if _, err := fh.Write(data); err != nil {
			return nil, fileErr(err)
		}
		sum.Write(data)
		size += int64(len(data))
	}
	if size != h.Size {
		fh.Sync()
		return nil, status.Errorf(codes.DataLoss, "got %d of %d bytes", size, h.Size)
	}
	if len(h.Sha256) > 0 && !bytes.Equal(sum.Sum(nil), h.Sha256) {
		fh.Close()
		os.Remove(part)
		return nil, status.Error(codes.DataLoss, "sha256 mismatch; partial upload discarded")
	}
	if err := fh.Sync(); err != nil {
		return nil, fileErr(err)
	}
	fh.Close()
	mode := os.FileMode(h.Mode).Perm()
	if mode == 0 {
		mode = 0o644
	}
	os.Chmod(part, mode)
	if h.Mtime != nil {
		t := h.Mtime.AsTime()
		os.Chtimes(part, time.Now(), t)
	}
	if err := platform.ReplaceFile(part, abs); err != nil {
		return nil, fileErr(err)
	}
	ok = true
	return &pb.FileWriteResult{AbsPath: abs, Size: size}, nil
}

func (f *filesServer) Mkdir(ctx context.Context, req *pb.MkdirRequest) (*pb.FileInfo, error) {
	if f.s.perUser() {
		return f.proxyMkdir(ctx, req)
	}
	abs, err := resolvePath(req.Path)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	mode := os.FileMode(req.Mode).Perm()
	if mode == 0 {
		mode = 0o755
	}
	err = os.MkdirAll(abs, mode)
	f.s.Audit.Log(IdentityFrom(ctx), pb.Files_Mkdir_FullMethodName, map[string]any{"path": abs, "error": errString(err)})
	if err != nil {
		return nil, fileErr(err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fileErr(err)
	}
	return infoOf(req.Path, abs, fi), nil
}
