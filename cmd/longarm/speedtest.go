package main

// longarm speedtest: throughput between this machine and a worker, or between
// two workers (over the path longarm cp takes), cached for the transfer guard
// of put / get / cp / sync (guardTransfer).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/platform"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

const (
	speedCacheFile = "speed-cache.json"
	// speedFresh: a measurement this old still guards transfers
	speedFresh = 10 * time.Minute
	// guardMinBytes: smaller transfers are never held back
	guardMinBytes = 1 << 20
	// guardMaxTook: a transfer estimated to take longer, not over a direct
	// path, needs --force
	guardMaxTook = 30 * time.Second
	// guardUnmeasuredBytes: over a relayed path with no measurement, this
	// much needs --force (or a speedtest first)
	guardUnmeasuredBytes = 100 << 20
)

// speedEntry is one measurement ("up": from From to To).
type speedEntry struct {
	From      string    `json:"from"` // "local" = this machine
	To        string    `json:"to"`
	UpMbps    float64   `json:"upMbps"`
	DownMbps  float64   `json:"downMbps"`
	UpBytes   int64     `json:"upBytes"`
	DownBytes int64     `json:"downBytes"`
	Transport string    `json:"transport,omitempty"`
	Path      string    `json:"path,omitempty"` // direct, relay, derp, peer-relay, unknown
	RTTMs     float64   `json:"rttMs,omitempty"`
	At        time.Time `json:"at"`
}

func (e speedEntry) relayed() bool { return e.Path != "direct" }

func speedKey(from, to string) string { return from + "|" + to }

var speedCacheMu sync.Mutex

func loadSpeeds(dir string) map[string]speedEntry {
	m := map[string]speedEntry{}
	if b, err := os.ReadFile(filepath.Join(dir, speedCacheFile)); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

// saveSpeed records e (both directions are one entry, under From|To).
func saveSpeed(dir string, e speedEntry) {
	speedCacheMu.Lock()
	defer speedCacheMu.Unlock()
	m := loadSpeeds(dir) // what other longarm processes wrote meanwhile
	for k, v := range m {
		if time.Since(v.At) > 24*time.Hour {
			delete(m, k)
		}
	}
	m[speedKey(e.From, e.To)] = e
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil || os.MkdirAll(dir, 0o700) != nil {
		return
	}
	p := filepath.Join(dir, speedCacheFile)
	tmp := fmt.Sprintf("%s.%d.tmp", p, os.Getpid())
	if os.WriteFile(tmp, b, 0o600) == nil && platform.ReplaceFile(tmp, p) != nil {
		os.Remove(tmp)
	}
}

// cachedSpeed is a fresh measurement between a and b, either way round
// (reversed: up and down swapped).
func cachedSpeed(dir, a, b string) (speedEntry, bool) {
	m := loadSpeeds(dir)
	if e, ok := m[speedKey(a, b)]; ok && time.Since(e.At) < speedFresh {
		return e, true
	}
	if e, ok := m[speedKey(b, a)]; ok && time.Since(e.At) < speedFresh {
		e.From, e.To, e.UpMbps, e.DownMbps, e.UpBytes, e.DownBytes = a, b, e.DownMbps, e.UpMbps, e.DownBytes, e.UpBytes
		return e, true
	}
	return speedEntry{}, false
}

func speedtestCmd() *cobra.Command {
	var asJSON bool
	var dur time.Duration
	var maxBytes int64
	c := &cobra.Command{
		Use:   "speedtest <worker> [<worker2>]",
		Short: "Measure throughput to a worker, or between two workers (the path longarm cp takes)",
		Long: `Measure how fast bytes move both ways: between this machine and <worker>, or
between <worker> and <worker2> (worker2 dials worker, as longarm cp does).
Each direction runs for --duration (filler, nothing on disk) and reports
Mbit/s, the transport path (direct, relay, derp, peer-relay) and the RTT.
The result is kept for 10 minutes: put, get, cp and sync use it to refuse a
large transfer that would be slow over a relayed path (exit 75; --force).`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			ctx := cmd.Context()
			var e speedEntry
			if len(args) == 1 {
				e, err = speedToWorker(ctx, s, args[0], dur, maxBytes)
			} else {
				e, err = speedBetween(ctx, s, args[0], args[1], dur, maxBytes)
			}
			if err != nil {
				return err
			}
			saveSpeed(s.cfg.Dir, e)
			if asJSON {
				b, _ := json.MarshalIndent(e, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			fmt.Println(speedLine(e))
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	c.Flags().DurationVar(&dur, "duration", 3*time.Second, "per direction (at most 10s)")
	c.Flags().Int64Var(&maxBytes, "max-bytes", 256<<20, "per direction: stop after this many bytes")
	return c
}

func speedLine(e speedEntry) string {
	from := e.From
	if from == "local" {
		from = "this machine"
	}
	return fmt.Sprintf("%s -> %s: %.1f Mbit/s, %s <- %s: %.1f Mbit/s (%s/%s, rtt %.0f ms)", from, e.To, e.UpMbps, from, e.To, e.DownMbps,
		orUnknown(e.Transport), orUnknown(e.Path), e.RTTMs)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func needSpeedtest(e *hoEndpoint) error {
	if !slices.Contains(e.features, jobs.FeatureSpeedtest) {
		return fmt.Errorf("%s: longarmd %s measures throughput only once updated (longarm worker update %s)", e.name, e.version, e.name)
	}
	return nil
}

// speedToWorker measures between this machine and w over the connection
// put and get use.
func speedToWorker(ctx context.Context, s *session, w string, d time.Duration, maxBytes int64) (speedEntry, error) {
	ep, err := openEndpoint(ctx, s, w)
	if err != nil {
		return speedEntry{}, err
	}
	defer ep.close()
	if err := needSpeedtest(ep); err != nil {
		return speedEntry{}, err
	}
	e := speedEntry{From: "local", To: w, At: time.Now()}
	cp := ep.conn.Copy()
	// down
	rd, err := cp.Source(ctx, &pb.SpeedSourceRequest{Duration: durationpb.New(d), MaxBytes: maxBytes})
	if err != nil {
		return e, fmt.Errorf("%s: %s", w, grpcMessage(err))
	}
	var start time.Time
	for {
		m, err := rd.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return e, fmt.Errorf("%s: %s", w, grpcMessage(err))
		}
		if start.IsZero() {
			start = time.Now()
		}
		e.DownBytes += int64(len(m.Data))
	}
	if !start.IsZero() {
		e.DownMbps = toMbps(e.DownBytes, time.Since(start))
	}
	// up, timed by the worker
	wr, err := cp.Sink(ctx)
	if err != nil {
		return e, fmt.Errorf("%s: %s", w, grpcMessage(err))
	}
	buf := make([]byte, 256<<10)
	rand.Read(buf)
	end := time.Now().Add(d)
	for sent := int64(0); sent < maxBytes && time.Now().Before(end); sent += int64(len(buf)) {
		if err := wr.Send(&pb.SpeedSinkChunk{Data: buf}); err != nil {
			break // the worker ended it: CloseAndRecv says why
		}
	}
	sum, err := wr.CloseAndRecv()
	if err != nil {
		return e, fmt.Errorf("%s: %s", w, grpcMessage(err))
	}
	e.UpBytes, e.UpMbps = sum.Bytes, toMbps(sum.Bytes, sum.Took.AsDuration())
	// the path at the end: longnet may have moved from the relay to a direct path meanwhile
	p := ep.conn.Path()
	e.Transport, e.Path, e.RTTMs = p.Transport, string(p.Kind), float64(p.RTT.Microseconds())/1000
	return e, nil
}

// speedBetween measures between workers a and b: a offers a speed test to
// b, b dials a (as for longarm cp) and measures both ways ("up": a -> b).
func speedBetween(ctx context.Context, s *session, a, b string, d time.Duration, maxBytes int64) (speedEntry, error) {
	src, err := openEndpoint(ctx, s, a)
	if err != nil {
		return speedEntry{}, err
	}
	defer src.close()
	dst, err := openEndpoint(ctx, s, b)
	if err != nil {
		return speedEntry{}, err
	}
	defer dst.close()
	for _, e := range []*hoEndpoint{src, dst} {
		if err := needSpeedtest(e); err != nil {
			return speedEntry{}, err
		}
	}
	endpoints := workerEndpoints(s, a)
	if len(endpoints) == 0 {
		return speedEntry{}, fmt.Errorf("no endpoints of %s known here", a)
	}
	token := make([]byte, 32)
	rand.Read(token)
	defer clear(token)
	id := make([]byte, 8)
	rand.Read(id)
	grant := &pb.CopyGrant{TransferId: "speed-" + hex.EncodeToString(id), Token: token}
	if _, err := src.conn.Copy().Offer(ctx, &pb.CopyOfferRequest{Speed: true, Peer: dst.cert, PeerId: dst.id, TransferId: grant.TransferId,
		Token: token, Ttl: durationpb.New(2 * time.Minute)}); err != nil {
		return speedEntry{}, fmt.Errorf("%s: %s", a, grpcMessage(err))
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hubTimeout)
		defer cancel()
		src.conn.Copy().Revoke(rctx, &pb.CopyRevokeRequest{TransferId: grant.TransferId})
	}()
	defer relayPair(ctx, s, src, dst, true, notef)()
	r, err := dst.conn.Copy().Speed(ctx, &pb.CopySpeedRequest{From: src.cert, FromId: src.id, FromEndpoints: endpoints, Grant: grant,
		Duration: durationpb.New(d), MaxBytes: maxBytes})
	if err != nil {
		return speedEntry{}, fmt.Errorf("%s <-> %s: %s", a, b, grpcMessage(err))
	}
	// the result is from b's side: its "down" is a -> b
	return speedEntry{From: a, To: b, UpMbps: r.DownMbps, DownMbps: r.UpMbps, UpBytes: r.DownBytes, DownBytes: r.UpBytes,
		Transport: r.Transport, Path: r.Path, RTTMs: float64(r.Rtt.AsDuration().Microseconds()) / 1000, At: time.Now()}, nil
}

func toMbps(n int64, took time.Duration) float64 {
	if took <= 0 {
		return 0
	}
	return float64(n) * 8 / took.Seconds() / 1e6
}

// guardTransfer refuses (exit 75) moving n bytes from -> to that would be
// slow: not over a direct path, and estimated at more than guardMaxTook
// from a fresh measurement - or, unmeasured, more than
// guardUnmeasuredBytes over a path known to be relayed (path: the
// connection's, "" = not known). force skips it.
func guardTransfer(dir, verb, from, to string, n int64, path string, force bool) error {
	if force || n < guardMinBytes {
		return nil
	}
	e, measured := cachedSpeed(dir, from, to)
	if !measured {
		if path == "" || path == "direct" || n < guardUnmeasuredBytes {
			return nil
		}
		return tempFailf("%s %s over a %s path, speed not measured: run longarm speedtest %s first, use --force, or move it another way (a git remote, cloud storage both sides use)",
			verb, humanSize(n), path, speedPair(from, to))
	}
	if !e.relayed() || e.UpMbps <= 0 {
		return nil
	}
	took := time.Duration(float64(n) * 8 / (e.UpMbps * 1e6) * float64(time.Second))
	if took <= guardMaxTook {
		return nil
	}
	return tempFailf("%s %s over a %s path at %.1f Mbit/s (measured %s ago) takes about %s: use --force, or move it another way (a git remote, cloud storage both sides use)",
		verb, humanSize(n), orUnknown(e.Path), e.UpMbps, time.Since(e.At).Round(time.Second), took.Round(time.Second))
}

type speedIn struct {
	Worker          string `json:"worker" jsonschema:"worker: name, node id or unique id prefix"`
	Worker2         string `json:"worker2,omitempty" jsonschema:"a second worker: measure between worker and worker2 instead of from this machine"`
	DurationSeconds int    `json:"duration_seconds,omitempty" jsonschema:"seconds per direction (default 3, max 10)"`
}

// speedtest is fleet_speedtest.
func (t *mcpTools) speedtest(ctx context.Context, _ *mcp.CallToolRequest, in speedIn) (*mcp.CallToolResult, speedEntry, error) {
	d := time.Duration(in.DurationSeconds) * time.Second
	if d <= 0 {
		d = 3 * time.Second
	}
	var e speedEntry
	var err error
	if in.Worker2 == "" {
		e, err = speedToWorker(ctx, t.s, in.Worker, d, 256<<20)
	} else {
		e, err = speedBetween(ctx, t.s, in.Worker, in.Worker2, d, 256<<20)
	}
	if err != nil {
		return nil, speedEntry{}, err
	}
	saveSpeed(t.s.cfg.Dir, e)
	return nil, e, nil
}

func speedPair(from, to string) string {
	switch {
	case from == "local":
		return to
	case to == "local":
		return from
	}
	return from + " " + to
}
