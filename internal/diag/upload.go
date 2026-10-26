package diag

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/version"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// ChunkSize of uploads and downloads.
const ChunkSize = 256 << 10

// MaxNote is the longest --note kept.
const MaxNote = 1000

// Upload sends the zip at p to the hub and returns its receipt; meta's
// size and sha256 are filled in here and checked against the receipt.
func Upload(ctx context.Context, cc grpc.ClientConnInterface, p string, meta *pb.DiagMeta) (*pb.DiagReceipt, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	meta.Size, meta.Sha256 = size, hex.EncodeToString(h.Sum(nil))
	if meta.Os == "" {
		meta.Os, meta.Arch = runtime.GOOS, runtime.GOARCH
	}
	if meta.Version == "" {
		meta.Version = version.Version
	}
	stream, err := pb.NewDiagnosticsClient(cc).Upload(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&pb.DiagChunk{Meta: meta}); err != nil {
		_, rerr := stream.CloseAndRecv()
		return nil, firstErr(rerr, err)
	}
	buf := make([]byte, ChunkSize)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if err := stream.Send(&pb.DiagChunk{Data: buf[:n]}); err != nil {
				_, cerr := stream.CloseAndRecv() // the hub's reason
				return nil, firstErr(cerr, err)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	r, err := stream.CloseAndRecv()
	if err != nil {
		return nil, err
	}
	if r.Sha256 != meta.Sha256 || r.Size != size {
		return r, fmt.Errorf("the hub stored %d bytes (sha256 %s), sent %d (%s)", r.Size, r.Sha256, size, meta.Sha256)
	}
	return r, nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil && !errors.Is(e, io.EOF) {
			return e
		}
	}
	return errors.New("upload failed")
}

// Download fetches a report into w and returns its metadata.
func Download(ctx context.Context, cc grpc.ClientConnInterface, id string, w io.Writer) (*pb.DiagMeta, error) {
	stream, err := pb.NewDiagnosticsClient(cc).Download(ctx, &pb.DiagDownloadRequest{ReportId: id})
	if err != nil {
		return nil, err
	}
	var meta *pb.DiagMeta
	h := sha256.New()
	var n int64
	for {
		c, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return meta, err
		}
		if c.Meta != nil {
			meta = c.Meta
		}
		if len(c.Data) > 0 {
			if _, err := w.Write(c.Data); err != nil {
				return meta, err
			}
			h.Write(c.Data)
			n += int64(len(c.Data))
		}
	}
	if meta == nil {
		return nil, errors.New("download: no metadata")
	}
	if meta.Size > 0 && meta.Size != n {
		return meta, fmt.Errorf("download: got %d bytes, want %d", n, meta.Size)
	}
	if meta.Sha256 != "" && meta.Sha256 != hex.EncodeToString(h.Sum(nil)) {
		return meta, errors.New("download: sha256 mismatch")
	}
	return meta, nil
}

// FinishOptions control what happens to a collected bundle.
type FinishOptions struct {
	Out    string // zip path ("" = longarm-diag-<time>.zip in the current dir)
	Upload bool
	Yes    bool // upload without asking
	Note   string
	// Dial connects to the hub (Upload); close is called afterwards.
	Dial   func(ctx context.Context) (cc grpc.ClientConnInterface, close func(), err error)
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// DefaultOut is the default file name of a bundle.
func DefaultOut(component string, t time.Time) string {
	return fmt.Sprintf("%s-diag-%s.zip", component, t.Format("20060102-150405"))
}

// Finish prints the summary, writes the zip and - with Upload, after a
// confirmation unless Yes - uploads it, printing the report id.
func Finish(ctx context.Context, b *Builder, o FinishOptions) (*pb.DiagReceipt, error) {
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	if o.Stdin == nil {
		o.Stdin = os.Stdin
	}
	if o.Out == "" {
		o.Out = DefaultOut("longarm", time.Now())
	}
	fmt.Fprint(o.Stderr, b.Summary())
	n, err := b.WriteFile(o.Out)
	if err != nil {
		return nil, err
	}
	abs, _ := filepath.Abs(o.Out)
	fmt.Fprintf(o.Stderr, "wrote %s (%s); look inside before sending it anywhere\n", abs, HumanBytes(n))
	if !o.Upload {
		fmt.Fprintf(o.Stderr, "to send it to the fleet's maintainers: run again with --upload, or attach the zip to the bug report\n")
		return nil, nil
	}
	if !o.Yes {
		fmt.Fprintf(o.Stderr, "upload %s to the hub (readable by the hub's admins)? [y/N] ", HumanBytes(n))
		line, _ := bufio.NewReader(o.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return nil, errors.New("not uploaded (answer y, or pass --yes); the zip stays at " + abs)
		}
	}
	if o.Dial == nil {
		return nil, errors.New("no hub configured: attach " + abs + " to the bug report by hand")
	}
	cc, closeFn, err := o.Dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("hub: %w (the zip stays at %s)", err, abs)
	}
	defer closeFn()
	uctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	note := o.Note
	if len(note) > MaxNote {
		note = note[:MaxNote]
	}
	r, err := Upload(uctx, cc, o.Out, &pb.DiagMeta{Component: b.Component, Node: b.Node, Note: b.ScrubString(note)})
	if err != nil {
		if s, ok := status.FromError(err); ok {
			err = errors.New(s.Message())
			if s.Code() == codes.Unimplemented {
				err = errors.New("this hub doesn't collect diagnostics (an older hub)")
			}
		}
		return nil, fmt.Errorf("upload: %w (attach %s to the bug report by hand)", err, abs)
	}
	fmt.Fprintf(o.Stderr, "uploaded: %s/%s\n", r.Location, r.Blob)
	fmt.Fprintf(o.Stdout, "%s\n", r.ReportId)
	fmt.Fprintf(o.Stderr, "put the report id %s into the bug report\n", r.ReportId)
	return r, nil
}
