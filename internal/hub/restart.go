package hub

import (
	"os"
	"strconv"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// A stopping hub (SIGTERM: a redeploy, an upgrade) tells every connected
// node (HubCommand.restarting) and every Fleet.Events subscriber that asked
// for kind "hub" that it is restarting, so they reconnect within about
// restartReconnectAfter of the new hub answering instead of treating the
// drop as an outage and backing off. It waits up to restartFlush for the
// node sessions' queues to drain before the listeners close.
const (
	restartReconnectAfter = time.Second
	restartFlush          = time.Second
)

// EnvContainerStart is the container entrypoint's start (Unix milliseconds,
// set by `longarm-hub container` before it hands over to Litestream / serve):
// the hub logs how long the whole start took (restore, bootstrap, open).
const EnvContainerStart = "LONGARM_HUB_CONTAINER_START"

// processStart: this process's start, for the startup timing.
var processStart = time.Now()

func (h *Hub) announceRestart(after, flush time.Duration) {
	cmd := &pb.HubCommand{Msg: &pb.HubCommand_Restarting{Restarting: &pb.HubRestarting{
		ReconnectAfter: durationpb.New(after), Reason: "shutting down"}}}
	var outs []chan *pb.HubCommand
	h.mu.Lock()
	for _, n := range h.nodes {
		if n.sess == nil {
			continue
		}
		select {
		case n.sess.out <- cmd:
			outs = append(outs, n.sess.out)
		default: // its queue is full: it finds out from the closed stream
		}
	}
	h.mu.Unlock()
	h.bus.publish(&pb.FleetEvent{Kind: "hub", Message: "restarting"})
	start := time.Now()
	for deadline := start.Add(flush); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		queued := 0
		for _, o := range outs {
			queued += len(o)
		}
		if queued == 0 {
			// taken from the queues; give the last Send a moment
			time.Sleep(50 * time.Millisecond)
			break
		}
	}
	h.log.Info("restart announced", "nodes", len(outs), "took", time.Since(start).Round(time.Millisecond))
}

// logStartup logs how long the start took until the listeners were up:
// from this process's start (open the database, load the CA and state) and,
// in the container, from the entrypoint's start (Litestream restore and
// bootstrap before that).
// serveAt is when Serve began (Open was done).
func (h *Hub) logStartup(serveAt time.Time) {
	now := time.Now()
	args := []any{"process", now.Sub(processStart).Round(time.Millisecond), "open", serveAt.Sub(processStart).Round(time.Millisecond)}
	if ms, err := strconv.ParseInt(os.Getenv(EnvContainerStart), 10, 64); err == nil && ms > 0 {
		args = append(args, "container", now.Sub(time.UnixMilli(ms)).Round(time.Millisecond))
	}
	h.log.Info("startup timing", args...)
}
