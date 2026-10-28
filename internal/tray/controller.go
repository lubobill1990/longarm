package tray

import (
	"context"
	"path/filepath"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/localapi"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Shell is what the controller drives: the tray icon and notifications.
type Shell interface {
	SetIcon(png []byte)
	SetTooltip(text string)
	Notify(title, body string)
}

// Controller follows longarm daemon's pool view (WatchPool, every 2 s) and this
// client's jobs (every 10 s) and keeps the tray icon, its tooltip and the
// notifications current. It starts the daemon (longarm daemon start) when the
// client role is installed and the daemon is not running.
type Controller struct {
	opts  Options
	shell Shell
	rules *Rules
	start func() error // longarm daemon start

	mu   sync.Mutex
	last *pb.PoolView
	err  error
}

// NewController returns a controller; Run drives it.
func NewController(opts Options, shell Shell, startDaemon func() error) *Controller {
	return &Controller{opts: opts, shell: shell, rules: NewRules(), start: startDaemon}
}

// Last is the latest pool view (nil before the first) and the last error.
func (c *Controller) Last() (*pb.PoolView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last, c.err
}

// Run follows the daemon until ctx is done.
func (c *Controller) Run(ctx context.Context) {
	addrFile := filepath.Join(c.opts.LongarmConfigDir, "daemon.addr")
	c.shell.SetIcon(IconPNG(nil))
	c.shell.SetTooltip("Remote Workers: starting")
	var lastStart time.Time
	go c.jobsLoop(ctx, addrFile)
	for ctx.Err() == nil {
		err := c.watch(ctx, addrFile)
		if ctx.Err() != nil {
			return
		}
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		c.shell.SetTooltip("Remote Workers: longarm daemon unreachable")
		if c.start != nil && time.Since(lastStart) > 30*time.Second {
			lastStart = time.Now()
			if err := c.start(); err != nil {
				c.opts.Log.Warn("longarm daemon start", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (c *Controller) watch(ctx context.Context, addrFile string) error {
	cc, err := localapi.DialFile(addrFile, "longarm daemon")
	if err != nil {
		return err
	}
	defer cc.Close()
	st, err := pb.NewLocalClientClient(cc).WatchPool(ctx, &pb.PoolRequest{})
	if err != nil {
		return err
	}
	for {
		v, err := st.Recv()
		if err != nil {
			return err
		}
		c.Apply(v, time.Now())
	}
}

// Apply updates the icon, tooltip and notifications from a pool view.
func (c *Controller) Apply(v *pb.PoolView, now time.Time) {
	c.mu.Lock()
	c.last, c.err = v, nil
	notes := c.rules.Pool(v, now)
	c.mu.Unlock()
	c.shell.SetIcon(IconPNG(IconWorkers(v)))
	c.shell.SetTooltip(Tooltip(v))
	if LoadPrefs(c.opts.LongarmConfigDir).MuteWorkers { // Settings: the state is still followed
		return
	}
	for _, n := range notes {
		c.shell.Notify(n.Title, n.Body)
	}
}

func (c *Controller) jobsLoop(ctx context.Context, addrFile string) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		if cc, err := localapi.DialFile(addrFile, "longarm daemon"); err == nil {
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			r, err := pb.NewLocalClientClient(cc).ListJobs(cctx, &pb.ClientJobsRequest{Mine: true, Limit: 100})
			cancel()
			cc.Close()
			if err == nil {
				c.mu.Lock()
				notes := c.rules.Jobs(r.Jobs)
				c.mu.Unlock()
				if LoadPrefs(c.opts.LongarmConfigDir).MuteJobs {
					notes = nil
				}
				for _, n := range notes {
					c.shell.Notify(n.Title, n.Body)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
