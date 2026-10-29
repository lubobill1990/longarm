package longarmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/updatesvc"
	"github.com/lubobill1990/longarm/internal/userspace"
	"github.com/lubobill1990/longarm/internal/version"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// machineUpdater serves MachineUpdate (server.MachineUpdater): an owner's
// update of this machine. An MSI install (Windows, the LongarmUpdate
// service present) forwards it to the service over its pipe - the one
// path that installs an MSI, with its own checks (newer, signed, from the
// recorded hub); any other install updates itself (updater.applyWanted).
// Nothing here is reachable from the hub: the request comes end to end
// from the owner's client (server/machineupdate.go checks the owner).
type machineUpdater struct {
	name string
	log  *slog.Logger
	// service: the update service's client; nil when this machine has none.
	service func() serviceClient
	// self: the self-updater (nil: none - no hub, [update] disabled, the
	// admin worker).
	self func() *updater
	why  string // why self is nil

	// probe: the service's client to try when Installed() said no and
	// longarmd can't update itself (Windows: the service query may have
	// failed; the client checks the pipe's server is the real service).
	probe func() serviceClient

	mu   sync.Mutex
	busy bool
}

// serviceClient is updatesvc.Client (tests fake it).
type serviceClient interface {
	Do(ctx context.Context, req *updatesvc.Request) (*updatesvc.Response, error)
}

func newMachineUpdater(name string, log *slog.Logger, self func() *updater, why string) *machineUpdater {
	m := &machineUpdater{name: name, log: log.With("component", "machine-update"), self: self, why: why}
	client := func() serviceClient { return updatesvc.NewClient("longarmd " + name) }
	if updatesvc.Installed() {
		m.service = client
	} else if runtime.GOOS == "windows" {
		m.probe = client
	}
	return m
}

// selfWhy says why longarmd can't update itself (nil: it can).
func (m *machineUpdater) selfWhy() error {
	u := m.selfUpdater()
	switch {
	case u == nil && m.why != "":
		return errors.New(m.why)
	case u == nil:
		return errors.New("this installation can't update itself")
	}
	if u.canApply != nil {
		return fmt.Errorf("this installation can't update itself: %w", u.canApply)
	}
	return nil
}

// RemoteMethod is how its owner's `longarm worker update` installs a
// release here (UpdateAvailable.method): "msi" through the Longarm Update
// service, "self" longarmd itself, "none" neither (see noServiceError).
func (m *machineUpdater) RemoteMethod() string {
	switch {
	case m.service != nil:
		return "msi"
	case m.selfWhy() == nil:
		return "self"
	case m.probe != nil && updatesvc.Installed(): // installed since longarmd started
		return "msi"
	}
	return "none"
}

// noServiceError: an update of a Windows install that can't update itself
// (Program Files, an MSI) and has no Longarm Update service - what to do.
func noServiceError(why error) string {
	return fmt.Sprintf("this machine has no Longarm Update service (%s), which installs updates of an MSI install, "+
		"and longarmd can't update itself (%v): run the new MSI on the machine once (the tray's update banner there, "+
		"or msiexec /i longarm-<version>-windows-amd64.msi); it installs the service, and later updates work from here", updatesvc.ServiceName, why)
}

func (m *machineUpdater) method() string {
	if m.service != nil {
		return "msi"
	}
	return "self"
}

func (m *machineUpdater) Start(ctx context.Context, by string, req *pb.MachineUpdateRequest, send func(*pb.MachineUpdateEvent) error) error {
	m.mu.Lock()
	if m.busy {
		m.mu.Unlock()
		return send(failedEvent(m.method(), "busy", "an update of this machine is in progress"))
	}
	m.busy = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.busy = false
		m.mu.Unlock()
	}()
	want := req.Version
	if req.Latest {
		want = "latest"
	}
	method := m.method()
	ev := func(phase, msg string) *pb.MachineUpdateEvent {
		return &pb.MachineUpdateEvent{Phase: phase, Message: msg, From: version.Version, Method: method, At: timestamppb.Now()}
	}
	if err := send(ev("checking", "updating "+m.name+" to "+want)); err != nil {
		return err
	}
	if m.service != nil {
		return m.startMSI(ctx, by, req, send, ev, m.service(), nil)
	}
	if why := m.selfWhy(); why != nil {
		if m.probe != nil {
			// The service query said no, but the service may be there all
			// the same: its pipe answers (the client checks it is the real
			// service), else the actionable message.
			method = "msi"
			return m.startMSI(ctx, by, req, send, ev, m.probe(), why)
		}
		return send(failedEvent("self", "unsupported", why.Error()))
	}
	u := m.selfUpdater()
	var sendErr error
	to, phase, err := u.applyWanted(ctx, want, func(phase, msg string) {
		if phase != "checking" && sendErr == nil {
			sendErr = send(ev(phase, msg))
		}
	})
	if err != nil {
		code := "failed"
		var ce *errCode
		if errors.As(err, &ce) {
			code = ce.code
		}
		return send(failedEvent("self", code, err.Error()))
	}
	e := ev(phase, "")
	e.To = to
	switch phase {
	case wantUpToDate:
		e.Message = "Longarm " + to + " runs here already"
	default:
		e.Message = fmt.Sprintf("updated to %s; longarmd restarts (running jobs and sessions keep running)", to)
	}
	return send(e)
}

func (m *machineUpdater) selfUpdater() *updater {
	if m.self == nil {
		return nil
	}
	return m.self()
}

// startMSI hands the request to the LongarmUpdate service and reports its
// phases while it downloads and verifies. Once msiexec runs, Longarm
// (this longarmd too) restarts: the last event is "installing".
//
// selfWhy != nil: probing a service Installed() didn't see - no service
// answering ends with what to do instead (noServiceError).
func (m *machineUpdater) startMSI(ctx context.Context, by string, req *pb.MachineUpdateRequest, send func(*pb.MachineUpdateEvent) error,
	ev func(phase, msg string) *pb.MachineUpdateEvent, c serviceClient, selfWhy error) error {
	type answer struct {
		r   *updatesvc.Response
		err error
	}
	done := make(chan answer, 1)
	go func() {
		r, err := c.Do(ctx, &updatesvc.Request{Op: updatesvc.OpInstall, Version: req.Version, Latest: req.Latest,
			Requester: "longarm worker update by " + by + " via " + m.name})
		done <- answer{r, err}
	}()
	last := "checking"
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case a := <-done:
			if a.err != nil {
				if selfWhy != nil && errors.Is(a.err, updatesvc.ErrNoService) {
					return send(failedEvent("msi", "unsupported", noServiceError(selfWhy)))
				}
				return send(failedEvent("msi", "unavailable", "the Longarm Update service: "+a.err.Error()))
			}
			r := a.r
			if !r.OK {
				return send(failedEvent("msi", r.Code, r.Error))
			}
			e := ev(r.Phase, r.Message)
			e.From, e.To = r.From, r.To
			if e.From == "" {
				e.From = version.Version
			}
			return send(e)
		case <-t.C:
			sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			r, err := c.Do(sctx, &updatesvc.Request{Op: updatesvc.OpStatus})
			cancel()
			if err != nil || !r.OK || r.Status == nil {
				continue
			}
			if p := r.Status.State; p != last && p != updatesvc.PhaseIdle {
				last = p
				e := ev(p, r.Status.Message)
				e.To = r.Status.Target
				if err := send(e); err != nil {
					return err
				}
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func failedEvent(method, code, msg string) *pb.MachineUpdateEvent {
	if code == "" {
		code = "failed"
	}
	return &pb.MachineUpdateEvent{Phase: "failed", Method: method, Code: code, Error: msg, From: version.Version, At: timestamppb.Now()}
}

func (m *machineUpdater) Status(ctx context.Context) (*pb.MachineUpdateEvent, error) {
	e := &pb.MachineUpdateEvent{Phase: "status", Method: m.method(), Installed: version.Version, From: version.Version, At: timestamppb.Now()}
	if m.service != nil {
		r, err := m.service().Do(ctx, &updatesvc.Request{Op: updatesvc.OpStatus})
		if err != nil {
			e.Error, e.Code = "the Longarm Update service: "+err.Error(), "unavailable"
			return e, nil
		}
		if st := r.Status; r.OK && st != nil {
			fillStatus(e, st)
		}
		return e, nil
	}
	if u := m.selfUpdater(); u != nil {
		if s := u.Status(); s != nil {
			e.LastFrom, e.LastTo, e.LastOk, e.LastError, e.LastAt = s.From, s.To, s.State == "ok", s.Error, s.At
		}
	}
	return e, nil
}

func fillStatus(e *pb.MachineUpdateEvent, st *updatesvc.Status) {
	e.Installed, e.Available, e.Auto, e.AutoIntervalHours = st.Installed, st.Available, st.Auto, int32(st.AutoIntervalHours)
	if st.State != updatesvc.PhaseIdle {
		e.Phase, e.To, e.Message = st.State, st.Target, st.Message
	}
	if l := st.Last; l != nil {
		e.LastFrom, e.LastTo, e.LastOk, e.LastError, e.LastAt = l.From, l.To, l.OK, l.Error, timestamppb.New(l.At)
	}
}

func (m *machineUpdater) SetAuto(ctx context.Context, by string, req *pb.MachineUpdateAutoRequest) (*pb.MachineUpdateEvent, error) {
	if m.service == nil {
		return failedEvent("self", "unsupported", "automatic updates are a Windows MSI install's (the Longarm Update service); this worker follows the hub's rollouts"), nil
	}
	r, err := m.service().Do(ctx, &updatesvc.Request{Op: updatesvc.OpSetAuto, Auto: req.Enabled, IntervalHours: int(req.IntervalHours),
		Requester: "longarm worker update --auto by " + by + " via " + m.name})
	if err != nil {
		return failedEvent("msi", "unavailable", "the Longarm Update service: "+err.Error()), nil
	}
	if !r.OK {
		return failedEvent("msi", r.Code, r.Error), nil
	}
	e := &pb.MachineUpdateEvent{Phase: "status", Method: "msi", Installed: version.Version, At: timestamppb.Now()}
	if r.Status != nil {
		fillStatus(e, r.Status)
	}
	return e, nil
}

// reportActivity tells the update service, once a minute while jobs run,
// that the machine is busy: automatic updates wait for a quiet moment
// (updatesvc.DefaultIdleFor). Best effort.
func (m *machineUpdater) reportActivity(ctx context.Context, spaces *userspace.Spaces) {
	if m.service == nil {
		return
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if len(spaces.ListJobs([]jobs.State{jobs.Preparing, jobs.Running}, 1)) == 0 {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		m.service().Do(cctx, &updatesvc.Request{Op: updatesvc.OpActivity})
		cancel()
	}
}
