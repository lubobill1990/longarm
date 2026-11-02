package longarmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/update"
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

	// report sends an update's state to the hub (NodeReport.update: the
	// hub's NodeInfo.update, what every viewer of this worker sees); nil
	// = no hub. lossy: progress, which may be dropped.
	report func(s *pb.UpdateStatus, lossy bool)

	mu   sync.Mutex
	busy bool

	// the update service as watchService last saw it (for the hello)
	svcMu     sync.Mutex
	svcStatus *pb.UpdateStatus
	svcLastAt time.Time // the last result reported
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

func (m *machineUpdater) Start(ctx context.Context, by string, req *pb.MachineUpdateRequest, send0 func(*pb.MachineUpdateEvent) error) error {
	m.mu.Lock()
	if m.busy {
		m.mu.Unlock()
		return send0(failedEvent(m.method(), "busy", "an update of this machine is in progress"))
	}
	// A failure the update path doesn't report itself goes to the hub too
	// ("Update to X failed: ..." for every viewer): refusals before
	// anything ran (a downgrade, a version the hub lacks, no service).
	// The self-updater and the update service report what happens once
	// they work on it (their own failures included). Never "busy": the
	// update that runs stays shown.
	started := time.Now()
	send := func(e *pb.MachineUpdateEvent) error {
		if e.Phase == "failed" && e.Code != "busy" && m.report != nil && !(e.Method == "self" && e.Code == "failed") {
			to := e.To
			if to == "" {
				to = req.Version
			}
			if to != "" && update.ValidVersion(to) {
				m.report(&pb.UpdateStatus{State: "error", From: version.Version, To: to, Error: e.Error, Code: e.Code, Method: e.Method,
					Initiator: "owner", Attempt: fmt.Sprintf("owner-%d", started.UnixNano()), Started: timestamppb.New(started)}, false)
			}
		}
		return send0(e)
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

// watchService follows the Longarm Update service for the hub: an install
// in progress (whoever started it - this worker's owner, automatic
// updates, the tray's banner on the machine) and how it ended, as
// NodeReport.update. Every few seconds while it installs, every 30 s
// otherwise. The admin worker and the session worker of a machine both
// report it (one MSI updates both).
func (m *machineUpdater) watchService(ctx context.Context) {
	if m.service == nil || m.report == nil {
		return
	}
	since := time.Now().Add(-time.Hour) // a result from before this start: an update that restarted longarmd
	for {
		busy := m.pollService(ctx, &since)
		wait := 30 * time.Second
		if busy {
			wait = 3 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// pollService reads the service's status once and reports changes;
// busy: an install is in progress.
func (m *machineUpdater) pollService(ctx context.Context, since *time.Time) (busy bool) {
	if m.service == nil || m.report == nil {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	r, err := m.service().Do(cctx, &updatesvc.Request{Op: updatesvc.OpStatus})
	cancel()
	if err != nil || !r.OK || r.Status == nil {
		return false
	}
	st := r.Status
	if s := serviceProgress(st); s != nil {
		m.svcMu.Lock()
		prev := m.svcStatus
		m.svcStatus = s
		m.svcMu.Unlock()
		if prev == nil || prev.Phase != s.Phase || prev.BytesDone != s.BytesDone || prev.Attempt != s.Attempt {
			m.report(s, prev != nil && prev.Phase == s.Phase && prev.Attempt == s.Attempt)
		}
		return true
	}
	l := st.Last
	if l == nil || !l.At.After(*since) {
		m.svcMu.Lock()
		if m.svcStatus != nil && m.svcStatus.State == "applying" {
			m.svcStatus = nil // the install went away without a result (a new service lost it)
		}
		m.svcMu.Unlock()
		return false
	}
	*since = l.At
	s := serviceResult(st.Installed, l)
	m.svcMu.Lock()
	m.svcStatus = s
	m.svcMu.Unlock()
	// The owner's refusals before anything ran were reported by Start.
	if !l.OK && l.Initiator == updatesvc.InitiatorOwner && l.Reason != "" && l.Reason != updatesvc.CodeFailed {
		return false
	}
	m.report(s, false)
	return false
}

// serviceProgress is an install in progress as an UpdateStatus (nil: none).
func serviceProgress(st *updatesvc.Status) *pb.UpdateStatus {
	switch st.State {
	case updatesvc.PhaseChecking, updatesvc.PhaseDownloading, updatesvc.PhaseVerifying, updatesvc.PhaseInstalling:
	default:
		return nil
	}
	if st.Target == "" || !update.ValidVersion(st.Target) {
		return nil
	}
	s := &pb.UpdateStatus{State: "applying", Phase: st.State, From: st.Installed, To: st.Target, Method: "msi",
		BytesDone: st.BytesDone, BytesTotal: st.BytesTotal, Initiator: st.Initiator, Attempt: st.Attempt, Message: st.Message, At: timestamppb.Now()}
	if st.State == updatesvc.PhaseInstalling {
		s.State = "restarting" // Longarm restarts while msiexec runs
	}
	if !st.Started.IsZero() {
		s.Started = timestamppb.New(st.Started)
		if s.Attempt == "" { // an older service
			s.Attempt = fmt.Sprintf("svc-%d", st.Started.UnixNano())
		}
	}
	return s
}

// serviceResult is a finished install as an UpdateStatus: "ok" (a 3010:
// the machine must restart), else "error" (MSI workers are in no rollout:
// it halts none).
func serviceResult(installed string, l *updatesvc.Result) *pb.UpdateStatus {
	s := &pb.UpdateStatus{State: "ok", From: l.From, To: l.To, Method: "msi", Initiator: l.Initiator, Attempt: l.Attempt,
		RebootRequired: l.RebootRequired(), At: timestamppb.New(l.At)}
	if !l.OK {
		s.State, s.Error = "error", l.Error
		switch l.Reason {
		case updatesvc.CodeFailed, "":
			s.Code = "msi"
		case updatesvc.CodeInvalid:
			s.Code = "" // the hub reads signature / sha256 from the message
		default:
			s.Code = l.Reason
		}
	}
	if s.Attempt == "" && !l.At.IsZero() {
		s.Attempt = "to:" + l.To // an older service: matched by the version
	}
	return s
}

// serviceUpdate is the update service's last status for the hello: an
// install in progress or a result of the last hour (nil: none).
func (m *machineUpdater) serviceUpdate() *pb.UpdateStatus {
	if m == nil {
		return nil
	}
	m.svcMu.Lock()
	defer m.svcMu.Unlock()
	if m.svcStatus == nil {
		return nil
	}
	if at := m.svcStatus.At; m.svcStatus.State != "applying" && m.svcStatus.State != "restarting" && at != nil && time.Since(at.AsTime()) > time.Hour {
		return nil
	}
	return proto.Clone(m.svcStatus).(*pb.UpdateStatus)
}
