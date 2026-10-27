package tgrpc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/telemetry"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Pusher hands a node's telemetry queue to the hub (Telemetry.Push): every
// Interval, and once more when Run ends.
type Pusher struct {
	Tel *telemetry.Client
	// Conn is the hub connection to push on; nil = no hub right now.
	Conn func(ctx context.Context) grpc.ClientConnInterface
	// Interval between pushes (default 30 s).
	Interval time.Duration
	Log      *slog.Logger
	// BeforeStop runs when Run's context ends, before the last push (a
	// process.stop event).
	BeforeStop func()
}

// ErrNoHub: there is no hub connection to push on.
var ErrNoHub = errors.New("no hub connection")

// DefaultBatch is the batch size until the hub says otherwise.
const DefaultBatch = 500

// maxBatches per Push (the rest waits for the next one).
const maxBatches = 20

// ApplyAck applies the hub's answer to tel; it reports whether the hub
// collects.
func ApplyAck(tel *telemetry.Client, ack *pb.TelemetryAck, err error) bool {
	if status.Code(err) == codes.Unimplemented {
		// a hub from before telemetry: it never collects
		tel.SetCollecting(false, 24*time.Hour)
		return false
	}
	if err != nil {
		return false
	}
	tel.SetCollecting(ack.Collecting, time.Duration(ack.RetryAfterSeconds)*time.Second)
	return ack.Collecting
}

// Push moves the memory buffer to the queue and sends the queue to the
// hub. With the hub not collecting it only asks again when that is due.
func (p *Pusher) Push(ctx context.Context) (int, error) {
	tel := p.Tel
	if tel == nil || tel.Config().Disabled {
		return 0, nil
	}
	tel.Flush()
	if !tel.Enabled() && !tel.RecheckDue() {
		return 0, nil
	}
	var cc grpc.ClientConnInterface
	if p.Conn != nil {
		cc = p.Conn(ctx)
	}
	if cc == nil {
		return 0, ErrNoHub
	}
	return PushQueue(ctx, tel, pb.NewTelemetryClient(cc))
}

// PushQueue sends tel's queue on cl.
func PushQueue(ctx context.Context, tel *telemetry.Client, cl pb.TelemetryClient) (int, error) {
	if !tel.Enabled() {
		// ask whether the hub collects now
		ack, err := cl.Push(ctx, &pb.TelemetryBatch{})
		ApplyAck(tel, ack, err)
		if err != nil && status.Code(err) != codes.Unimplemented {
			return 0, err
		}
		return 0, nil
	}
	q := tel.Queue()
	if q == nil {
		return 0, nil
	}
	batch, sent := DefaultBatch, 0
	for range maxBatches {
		items, err := q.Take(batch)
		if err != nil {
			return sent, err
		}
		if len(items) == 0 {
			return sent, nil
		}
		b := &pb.TelemetryBatch{Items: make([]*pb.TelemetryItem, len(items))}
		for i, it := range items {
			b.Items[i] = ToPB(it)
		}
		ack, err := cl.Push(ctx, b)
		if !ApplyAck(tel, ack, err) {
			if err != nil && status.Code(err) != codes.Unimplemented {
				q.Append(items) // try again next time
				return sent, err
			}
			return sent, nil // the hub doesn't collect: the queue is gone
		}
		sent += len(items)
		if ack.MaxBatch > 0 {
			batch = int(ack.MaxBatch)
		}
	}
	return sent, nil
}

// Run pushes every Interval until ctx ends, then once more (best effort,
// a few seconds).
func (p *Pusher) Run(ctx context.Context) {
	if p.Tel == nil || p.Tel.Config().Disabled {
		return
	}
	iv := p.Interval
	if iv <= 0 {
		iv = 30 * time.Second
		// LONGARM_TELEMETRY_INTERVAL: tests (and impatient people)
		if d, err := time.ParseDuration(os.Getenv("LONGARM_TELEMETRY_INTERVAL")); err == nil && d > 0 {
			iv = d
		}
	}
	log := p.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	push := func(ctx context.Context, d time.Duration) {
		pctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		if n, err := p.Push(pctx); err != nil && !errors.Is(err, ErrNoHub) {
			log.Debug("telemetry: push", "sent", n, "err", err)
		}
	}
	for {
		select {
		case <-ctx.Done():
			if p.BeforeStop != nil {
				p.BeforeStop()
			}
			p.Tel.Close()
			push(context.Background(), 3*time.Second)
			return
		case <-t.C:
			push(ctx, 15*time.Second)
		}
	}
}
