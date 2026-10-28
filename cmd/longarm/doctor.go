package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/doctor"
	"github.com/lubobill1990/longarm/internal/fwcheck"
	"github.com/lubobill1990/longarm/internal/localapi"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/version"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func doctorCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Check this machine's Longarm setup: sign-in, hub, daemon, firewall, your workers; says what to run next",
		Long: `Check this machine's Longarm setup and print a checklist; every line that
isn't ok ends with the exact command to run next ("next: ...").

Checks: signed in (and as whom); the certificate's validity; the hub
(reachable, its version); the client daemon (longarm daemon, which the tray
starts); the firewall rule for hole punching (Windows); and for each of
your own workers whether the hub sees it online (or when it last did) and
which transport legs connect from here.

--json prints the report for programs (docs/usage.md#doctor---json). Exit
code: 0 without failures (warnings allowed), 1 with failures.

The worker side has its own: longarmd doctor.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 45*time.Second)
			defer cancel()
			env := newClientEnv()
			defer env.close()
			r := doctor.New("longarm", version.Version, time.Now())
			r.Add(clientChecks(ctx, env, time.Now())...)
			if asJSON {
				if err := r.WriteJSON(os.Stdout); err != nil {
					return err
				}
			} else if err := r.WriteText(os.Stdout); err != nil {
				return err
			}
			if !r.OK {
				return &exitError{code: 1}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output (docs/usage.md#doctor---json)")
	return c
}

// workerLegs is how this machine reaches one worker (diag's probe).
type workerLegs struct {
	Path  string // the winning path, "" when none
	Race  string // every leg's outcome (client.RaceSummary)
	Error string
}

// clientEnv is what longarm doctor looks at (faked in tests).
type clientEnv interface {
	// SignIn: this machine's client identity and the certificate's end;
	// an error when it isn't signed in (as a user).
	SignIn() (pki.Identity, time.Time, error)
	// Hub: the configured hub ("" = none), and whether it answers.
	Hub(ctx context.Context) (addr, version string, err error)
	// Daemon: nil when longarm daemon runs and answers.
	Daemon(ctx context.Context) error
	// Firewall: the Windows Firewall verdict for longarm (nil: not checked).
	Firewall() *pb.FirewallStatus
	// Workers: the hub's worker list (nil when unknown) and the legs that
	// connect to each worker probed.
	Workers(ctx context.Context) ([]*pb.NodeInfo, map[string]workerLegs)
}

// renewWarnDays: the certificate check warns this close to the end.
const certWarnWithin = renewWarnWindow

// clientChecks runs the checklist of longarm doctor.
func clientChecks(ctx context.Context, env clientEnv, now time.Time) []doctor.Check {
	var out []doctor.Check
	id, notAfter, err := env.SignIn()
	signedIn := err == nil
	switch {
	case errors.Is(err, client.ErrNotSignedIn):
		out = append(out, doctor.Check{ID: "signed-in", Title: "signed in", Status: doctor.Fail, Detail: "this machine isn't signed in", Fix: "longarm login"})
	case err != nil:
		out = append(out, doctor.Check{ID: "signed-in", Title: "signed in", Status: doctor.Fail, Detail: oneLine(err), Fix: "longarm login"})
	default:
		d := fmt.Sprintf("as %s (device %s, %s certificate)", id.Owner(), id.Name, id.Role)
		out = append(out, doctor.Check{ID: "signed-in", Title: "signed in", Status: doctor.OK, Detail: d,
			Data: map[string]any{"user": id.Owner(), "device": id.Name, "role": string(id.Role)}})
		out = append(out, doctor.CertCheck(notAfter, now, certWarnWithin, "longarm login"))
	}

	hubOK := false
	if signedIn {
		addr, ver, err := env.Hub(ctx)
		c := doctor.Check{ID: "hub", Title: "hub"}
		switch {
		case addr == "":
			c.Status, c.Detail, c.Fix = doctor.Fail, "no hub configured", "longarm login"
		case err != nil:
			c.Status, c.Detail, c.Fix = doctor.Fail, addr+" unreachable: "+oneLine(err), hubFix(err)
			c.Data = map[string]any{"address": addr}
		default:
			hubOK = true
			c.Status, c.Detail = doctor.OK, addr+" reachable"
			if ver != "" {
				c.Detail += ", version " + ver
			}
			c.Data = map[string]any{"address": addr, "version": ver}
		}
		out = append(out, c)
	} else {
		out = append(out, doctor.Check{ID: "hub", Title: "hub", Status: doctor.Skip, Detail: "needs a sign-in"})
	}

	if err := env.Daemon(ctx); err != nil {
		out = append(out, doctor.Check{ID: "daemon", Title: "client daemon", Status: doctor.Warn,
			Detail: "not running (background port forwards, faster connections)", Fix: "longarm daemon start"})
	} else {
		out = append(out, doctor.Check{ID: "daemon", Title: "client daemon", Status: doctor.OK, Detail: "running"})
	}

	switch fw := env.Firewall(); {
	case fw == nil:
		out = append(out, doctor.Check{ID: "firewall", Title: "firewall", Status: doctor.Skip, Detail: "not checked (Windows only)"})
	case fw.Ok:
		out = append(out, doctor.Check{ID: "firewall", Title: "firewall", Status: doctor.OK, Detail: "inbound allow rule for " + fw.Program})
	default:
		out = append(out, doctor.Check{ID: "firewall", Title: "firewall", Status: doctor.Warn,
			Detail: fw.Problem, Fix: fw.Fix + "  (elevated)"})
	}

	if !signedIn {
		return out
	}
	nodes, legs := env.Workers(ctx)
	var mine []*pb.NodeInfo
	for _, n := range nodes {
		if n.Owner != "" && n.Owner == id.Owner() && n.State != "revoked" {
			mine = append(mine, n)
		}
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].Name < mine[j].Name })
	switch {
	case !hubOK:
	case len(mine) == 0:
		out = append(out, doctor.Check{ID: "workers", Title: "your workers", Status: doctor.Info,
			Detail: "none (make a machine yours: longarm worker add)"})
	}
	for _, n := range mine {
		out = append(out, workerCheck(n, legs[n.Name], now))
	}
	return out
}

// hubFix is the next step for an unreachable hub: sign in again when the
// hub refused the certificate, else a diagnostic bundle.
func hubFix(err error) string {
	s := strings.ToLower(err.Error())
	for _, k := range []string{"certificate", "unauthenticated", "tls", "revoked", "permissiondenied", "permission denied"} {
		if strings.Contains(s, k) {
			return "longarm login"
		}
	}
	return "longarm diag --upload"
}

// workerCheck is the line of one of the user's workers.
func workerCheck(n *pb.NodeInfo, l workerLegs, now time.Time) doctor.Check {
	c := doctor.Check{ID: "worker:" + n.Name, Title: "worker " + n.Name, Data: map[string]any{"online": n.Online}}
	if ls := n.GetLastSeen(); ls != nil && ls.AsTime().Unix() > 0 {
		c.Data["last_seen"] = ls.AsTime().UTC().Format(time.RFC3339)
	}
	if l.Race != "" {
		c.Data["legs"] = l.Race
	}
	switch {
	case n.State == "pending":
		c.Status, c.Detail, c.Fix = doctor.Warn, "waiting for an admin's approval", "longarm admin approve "+n.Name+"  (an admin)"
	case !n.Online:
		c.Status, c.Detail = doctor.Fail, strings.TrimPrefix(offlineNote(n, now), "worker "+n.Name+" ")
		c.Fix = workerStartFix(n)
	case l.Error != "":
		c.Status, c.Detail, c.Fix = doctor.Fail, "online at the hub, but no leg connects from here: "+l.Error, "longarm diag --upload"
	default:
		c.Status, c.Detail = doctor.OK, "online"
		if l.Path != "" {
			c.Detail += ", via " + l.Path
		}
		if l.Race != "" {
			c.Detail += " [" + l.Race + "]"
		}
	}
	return c
}

// workerStartFix: how to bring an offline worker of one's own back, run on
// that machine.
func workerStartFix(n *pb.NodeInfo) string {
	if n.Os == "windows" {
		return "on " + n.Name + ": log on, or run longarmd session-mode start (a service: longarmd service start)"
	}
	return "on " + n.Name + ": longarmd service start"
}

// realClientEnv is clientEnv on this machine.
type realClientEnv struct {
	s       *session
	sessErr error

	probeOnce sync.Once
	pr        *probeResult
}

func newClientEnv() *realClientEnv {
	quietHub = true
	s, err := openSessionWith(true)
	return &realClientEnv{s: s, sessErr: err}
}

func (e *realClientEnv) close() {
	if e.s != nil {
		e.s.close()
	}
}

func (e *realClientEnv) SignIn() (pki.Identity, time.Time, error) {
	if e.sessErr != nil {
		return pki.Identity{}, time.Time{}, e.sessErr
	}
	var notAfter time.Time
	if leaf := e.s.dialer.Bundle().Leaf(); leaf != nil {
		notAfter = leaf.NotAfter
	}
	return e.s.dialer.Identity(), notAfter, nil
}

func (e *realClientEnv) probe(ctx context.Context) *probeResult {
	e.probeOnce.Do(func() { e.pr, _ = probe(ctx, e.s) })
	return e.pr
}

func (e *realClientEnv) Hub(ctx context.Context) (string, string, error) {
	if !e.s.cfg.Hub.Enabled() || noHub {
		return "", "", nil
	}
	h := e.probe(ctx).Hub
	if ok, _ := h["reachable"].(bool); !ok {
		msg, _ := h["error"].(string)
		if msg == "" {
			msg = "no answer"
		}
		return e.s.cfg.Hub.Address, "", errors.New(msg)
	}
	if msg, _ := h["whoami_error"].(string); msg != "" {
		return e.s.cfg.Hub.Address, "", errors.New(msg)
	}
	v, _ := h["hub_version"].(string)
	return e.s.cfg.Hub.Address, v, nil
}

func (e *realClientEnv) Daemon(ctx context.Context) error {
	cc, err := localapi.DialFile(filepath.Join(configDir, daemonAddrFile), "longarm daemon")
	if err != nil {
		return err
	}
	defer cc.Close()
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err = pb.NewLocalClientClient(cc).Status(cctx, &pb.LocalStatusRequest{})
	return err
}

func (e *realClientEnv) Firewall() *pb.FirewallStatus { return fwcheck.Self() }

func (e *realClientEnv) Workers(ctx context.Context) ([]*pb.NodeInfo, map[string]workerLegs) {
	pr := e.probe(ctx)
	legs := map[string]workerLegs{}
	for _, m := range pr.Workers {
		if m == nil {
			continue
		}
		name, _ := m["worker"].(string)
		var l workerLegs
		l.Path, _ = m["path"].(string)
		l.Race, _ = m["race"].(string)
		l.Error, _ = m["error"].(string)
		legs[name] = l
	}
	nodes, _ := e.s.hubNodes(ctx, false)
	return nodes, legs
}
