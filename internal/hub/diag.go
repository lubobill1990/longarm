package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/diag"
	"github.com/lubobill1990/longarm/internal/pii"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// DiagConfig is [diagnostics]: where uploaded diagnostic bundles
// (`longarmd diag --upload`, `longarm diag --upload`) go.
type DiagConfig struct {
	// BlobURL is the Azure Blob Storage container,
	// https://<account>.blob.core.windows.net/<container> (the hub's managed
	// identity needs Storage Blob Data Contributor on it; no keys, no SAS),
	// or file:///<dir> (a local directory). Empty: the hub doesn't collect
	// diagnostics. Env LONGARM_HUB_DIAG_BLOB_URL.
	BlobURL string `toml:"blob_url"`
	// MaxMB is the largest bundle accepted (default 50).
	MaxMB int `toml:"max_mb"`
	// PerNodePerHour is how many bundles one node may upload per hour
	// (default 10).
	PerNodePerHour int `toml:"per_node_per_hour"`
}

// EnvDiagBlobURL overrides [diagnostics] blob_url.
const EnvDiagBlobURL = "LONGARM_HUB_DIAG_BLOB_URL"

func (c *Config) diagEnvDefaults() error {
	if v := strings.TrimSpace(os.Getenv(EnvDiagBlobURL)); v != "" {
		c.Diagnostics.BlobURL = v
	}
	if c.Diagnostics.MaxMB <= 0 {
		c.Diagnostics.MaxMB = 50
	}
	if c.Diagnostics.PerNodePerHour <= 0 {
		c.Diagnostics.PerNodePerHour = 10
	}
	if c.Diagnostics.BlobURL != "" {
		if _, err := diag.OpenStore(c.Diagnostics.BlobURL, nil); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	hubMethodRoles[pb.Diagnostics_Upload_FullMethodName] = anyNode
	hubMethodRoles[pb.Diagnostics_List_FullMethodName] = adminOnly
	hubMethodRoles[pb.Diagnostics_Download_FullMethodName] = adminOnly
}

// diagServer is the Diagnostics service.
type diagServer struct {
	pb.UnimplementedDiagnosticsServer
	h     *Hub
	store diag.Store // nil = not collecting
	max   int64
	rate  int

	mu      sync.Mutex
	uploads map[string][]time.Time // node -> upload times within the last hour
}

func newDiagServer(h *Hub) (*diagServer, error) {
	s := &diagServer{h: h, max: int64(h.cfg.Diagnostics.MaxMB) << 20, rate: h.cfg.Diagnostics.PerNodePerHour, uploads: map[string][]time.Time{}}
	if s.max <= 0 {
		s.max = 50 << 20
	}
	if s.rate <= 0 {
		s.rate = 10
	}
	if u := h.cfg.Diagnostics.BlobURL; u != "" {
		st, err := diag.OpenStore(u, nil)
		if err != nil {
			return nil, err
		}
		s.store = st
		h.log.Info("diagnostics: collecting bundles", "location", st.Location(), "max_mb", s.max>>20, "per_node_per_hour", s.rate)
	}
	return s, nil
}

var errNoDiag = status.Error(codes.FailedPrecondition,
	"this hub doesn't collect diagnostics ([diagnostics] blob_url in hub.toml is not set); attach the local zip to the bug report by hand")

// scrubber: the fleet salt and the names the hub knows.
func (s *diagServer) scrubber() *pii.Scrubber {
	names := map[string]string{}
	s.h.mu.Lock()
	for n, nd := range s.h.nodes {
		names[n] = pii.KindNode
		if nd.rec.User != "" {
			names[nd.rec.User] = pii.KindUser
		}
	}
	for u := range s.h.tenants {
		names[u] = pii.KindUser
	}
	s.h.mu.Unlock()
	return pii.New(pii.Options{Salt: pii.FleetSalt(s.h.ca.Cert.Raw), Names: names, NoLocal: true})
}

// allow takes one upload from node's hourly budget (refund gives it back).
func (s *diagServer) allow(node string) (refund func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var keep []time.Time
	for _, t := range s.uploads[node] {
		if now.Sub(t) < time.Hour {
			keep = append(keep, t)
		}
	}
	if len(keep) >= s.rate {
		s.uploads[node] = keep
		return nil, false
	}
	s.uploads[node] = append(keep, now)
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		l := s.uploads[node]
		for i, t := range l {
			if t.Equal(now) {
				s.uploads[node] = append(l[:i:i], l[i+1:]...)
				break
			}
		}
	}, true
}

func metaField(s string, max int) string {
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// Upload receives a bundle and streams it to the store.
func (s *diagServer) Upload(stream pb.Diagnostics_UploadServer) error {
	if s.store == nil {
		return errNoDiag
	}
	c := callerFrom(stream.Context())
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	m := first.GetMeta()
	if m == nil {
		return status.Error(codes.InvalidArgument, "the first chunk must carry the bundle's metadata")
	}
	if m.Size <= 0 {
		return status.Error(codes.InvalidArgument, "size missing")
	}
	if m.Size > s.max {
		return status.Errorf(codes.ResourceExhausted, "bundle of %s exceeds this hub's limit of %d MiB ([diagnostics] max_mb)", diag.HumanBytes(m.Size), s.max>>20)
	}
	want := strings.ToLower(m.Sha256)
	if _, err := hex.DecodeString(want); err != nil || len(want) != 64 {
		return status.Error(codes.InvalidArgument, "sha256 missing")
	}
	refund, ok := s.allow(c.id.Name)
	if !ok {
		return status.Errorf(codes.ResourceExhausted, "at most %d diagnostic uploads per node and hour ([diagnostics] per_node_per_hour); try later", s.rate)
	}
	sc := s.scrubber()
	now := time.Now().UTC()
	id := diag.NewReportID(now)
	blob, _ := diag.BlobPath(id)
	pseudo := sc.Hash(pii.KindNode, c.id.Name)
	note := metaField(sc.String(m.Note), diag.MaxNote)
	meta := map[string]string{
		"report_id": id, "component": metaField(asciiMeta(m.Component), 32), "version": metaField(asciiMeta(m.Version), 64),
		"node": pseudo, "uploader_role": string(c.id.Role), "os": metaField(asciiMeta(m.Os), 16), "arch": metaField(asciiMeta(m.Arch), 16),
		"size": strconv.FormatInt(m.Size, 10), "sha256": want, "created": now.Format(time.RFC3339),
		"note": url.QueryEscape(note), // metadata values are ASCII
	}

	pr, pw := io.Pipe()
	var putErr error
	putDone := make(chan struct{})
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	go func() {
		defer close(putDone)
		putErr = s.store.Put(ctx, blob, pr, m.Size, meta)
		pr.CloseWithError(putErr) // unblock the writer (nil: io.ErrClosedPipe)
	}()
	h := sha256.New()
	var got int64
	fail := func(err error) error {
		pw.CloseWithError(err)
		cancel()
		<-putDone
		refund()
		return err
	}
	write := func(data []byte) error {
		if got+int64(len(data)) > m.Size {
			return fail(status.Errorf(codes.InvalidArgument, "more than the announced %d bytes", m.Size))
		}
		h.Write(data)
		got += int64(len(data))
		if got == m.Size && hex.EncodeToString(h.Sum(nil)) != want {
			// the last bytes are held back: the blob is never committed
			return fail(status.Error(codes.DataLoss, "sha256 mismatch"))
		}
		if _, err := pw.Write(data); err != nil {
			<-putDone
			if putErr != nil {
				err = putErr
			}
			return fail(status.Errorf(codes.Unavailable, "storing the bundle: %v", err))
		}
		return nil
	}
	if len(first.Data) > 0 {
		if err := write(first.Data); err != nil {
			return err
		}
	}
	for {
		ch, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(err)
		}
		if err := write(ch.Data); err != nil {
			return err
		}
	}
	if got != m.Size {
		return fail(status.Errorf(codes.InvalidArgument, "got %d bytes, announced %d", got, m.Size))
	}
	pw.Close()
	<-putDone
	if err := putErr; err != nil {
		refund()
		s.h.log.Warn("diagnostic upload failed", "uploader", pseudo, "err", err)
		return status.Errorf(codes.Unavailable, "storing the bundle: %v", err)
	}
	s.h.log.Info("diagnostic bundle uploaded", "report", id, "size", got, "uploader", pseudo, "role", c.id.Role, "component", meta["component"])
	return stream.SendAndClose(&pb.DiagReceipt{ReportId: id, Blob: blob, Location: s.store.Location(), Size: got, Sha256: want})
}

// asciiMeta keeps printable ASCII (blob metadata values are HTTP headers).
func asciiMeta(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return -1
		}
		return r
	}, s)
}

func metaPB(bi diag.BlobInfo) *pb.DiagMeta {
	m := bi.Meta
	d := &pb.DiagMeta{Component: m["component"], Version: m["version"], Node: m["node"], Os: m["os"], Arch: m["arch"],
		Sha256: m["sha256"], ReportId: m["report_id"], UploaderRole: m["uploader_role"], Blob: bi.Name, Size: bi.Size}
	if n, err := url.QueryUnescape(m["note"]); err == nil {
		d.Note = n
	}
	t := bi.Modified
	if ct, err := time.Parse(time.RFC3339, m["created"]); err == nil {
		t = ct
	}
	if !t.IsZero() {
		d.Created = timestamppb.New(t)
	}
	if d.ReportId == "" {
		base := bi.Name[strings.LastIndex(bi.Name, "/")+1:]
		d.ReportId = strings.TrimSuffix(base, ".zip")
	}
	return d
}

// List lists bundles of the last days (admin).
func (s *diagServer) List(ctx context.Context, req *pb.DiagListRequest) (*pb.DiagListResponse, error) {
	if s.store == nil {
		return nil, errNoDiag
	}
	since := 7 * 24 * time.Hour
	if d := req.GetSince(); d != nil && d.AsDuration() > 0 {
		since = d.AsDuration()
	}
	if since > 366*24*time.Hour {
		since = 366 * 24 * time.Hour
	}
	limit := int(req.Limit)
	if limit <= 0 {
		limit = 100
	}
	now := time.Now().UTC()
	cutoff := now.Add(-since)
	var out []*pb.DiagMeta
	for day := now; ; day = day.AddDate(0, 0, -1) {
		blobs, err := s.store.List(ctx, diag.DayPrefix(day))
		if err != nil {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
		for _, b := range blobs {
			if !strings.HasSuffix(b.Name, ".zip") {
				continue
			}
			d := metaPB(b)
			if d.Created != nil && d.Created.AsTime().Before(cutoff) {
				continue
			}
			out = append(out, d)
		}
		if DayStart(day).Before(cutoff) || DayStart(day).Equal(cutoff) {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetCreated().AsTime().After(out[j].GetCreated().AsTime()) })
	if len(out) > limit {
		out = out[:limit]
	}
	return &pb.DiagListResponse{Reports: out}, nil
}

// DayStart is midnight UTC of t's day.
func DayStart(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// Download streams a bundle (admin).
func (s *diagServer) Download(req *pb.DiagDownloadRequest, stream pb.Diagnostics_DownloadServer) error {
	if s.store == nil {
		return errNoDiag
	}
	blob, err := diag.BlobPath(req.ReportId)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	rc, bi, err := s.store.Get(stream.Context(), blob)
	if errors.Is(err, diag.ErrNotFound) {
		return status.Errorf(codes.NotFound, "no diagnostic report %s", strings.ToUpper(req.ReportId))
	}
	if err != nil {
		return status.Error(codes.Unavailable, err.Error())
	}
	defer rc.Close()
	c := callerFrom(stream.Context())
	s.h.log.Info("diagnostic bundle downloaded", "report", req.ReportId, "by", c.id.String())
	if err := stream.Send(&pb.DiagChunk{Meta: metaPB(bi)}); err != nil {
		return err
	}
	buf := make([]byte, diag.ChunkSize)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if err := stream.Send(&pb.DiagChunk{Data: buf[:n]}); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return status.Error(codes.Unavailable, fmt.Sprintf("reading the bundle: %v", err))
		}
	}
}
