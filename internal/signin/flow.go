package signin

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/sched"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// SignIn signs this machine in to a Longarm hub with the user's Microsoft
// account and installs what the hub issues: a client device certificate
// (Options.Client), an owned-worker enrollment (Options.Worker), or both
// with ONE code and ONE approval. It is the flow behind `longarm login`
// and `longarmd login`, and the API a GUI (the tray) calls.
//
// Contract:
//
//   - Input: Options.Hub as the user typed it - "longarm.example.com",
//     "https://longarm.example.com", "host:port" (gRPC port). Without
//     Options.Fingerprint the hub's CA is fetched from
//     https://<host>/ca.crt over WebPKI-verified HTTPS and pinned (the
//     gRPC address from https://<host>/.well-known/longarm-hub, default
//     port 7443); a private hub without a public HTTPS panel fails with an
//     error asking for the fingerprint (errors.Is(err,
//     hubclient.ErrNoBootstrap)). An explicit fingerprint wins and is
//     verified on the gRPC connection. No input at all after an MSI
//     install with LOGIN=1: Hub and Fingerprint from
//     longarmd.PendingHub(dataDir, "") (the data dir's hub.toml, else
//     InstalledHub(): HKLM\Software\Longarm).
//   - Targets: Client is client.DeviceTarget{ConfigDir:
//     client.DefaultConfigDir()} for the user's longarm; Worker is
//     longarmd.WorkerTarget{DataDir: longarmd.DefaultDataDir()} for
//     longarmd's data dir. At least one; both = one pairing of kind
//     "both" (older hubs: two codes, one after the other). Both are
//     checked (Prepare) before anything goes to the hub, and installed only
//     when every requested part was approved.
//   - Method: MethodLink (zero value; the browser pairing, right for a
//     GUI), MethodAuto (the Azure CLI's token when it has one, else the
//     pairing; the CLI default of longarm login), MethodAz (az only).
//   - Events (all optional, called on SignIn's goroutine, must not block
//     long): Progress for each step with a one-line message (show it, or
//     log it); Code once per pairing with the /link URL and the code to
//     display - the user opens the URL in any browser, signs in with
//     Microsoft and types the code. SignIn opens the URL itself only with
//     Options.OpenBrowser (never with the code in it).
//   - Cancellation: cancel ctx; SignIn returns ctx.Err() (wrapped or not:
//     use errors.Is). A pairing ends by itself after its code expires (10
//     minutes): hubclient.ErrLinkExpired; a denial: hubclient.ErrLinkDenied.
//   - Output: nothing is printed; the Result names what was enrolled
//     (device and worker names, the user, the pinned CA fingerprint and
//     where it came from). Notes for the user (e.g. how to pin the
//     worker's owner) arrive as Progress events of StepNote.
//   - AdminWorker (optional, with any of the above): an admin worker of
//     the same machine (Windows: longarmd as LocalSystem, the MSI's
//     ADMIN_WORKER) enrolled with the same code and approval; the hub
//     marks it longarm.io/admin=true (owner-only, never shared, never
//     placed automatically) and the /link page says it runs as SYSTEM.
//     Its key is not ours: the target is a CSRSource (a pre-made CSR) and
//     its Install gets the result with KeyPEM nil. Pairing only: MethodAz
//     fails, MethodAuto pairs. An older hub: hubclient.ErrNoAdminLink,
//     nothing installed.
//   - After a worker sign-in the worker service must run: a service
//     waiting for its sign-in (an MSI install with LOGIN=1, hub.toml
//     sign_in) picks the certificate up by itself within seconds; else
//     start or install it (longarmd.EnsureWorkerService, elevated).
//
// A GUI signing the machine in as both, with one approval:
//
//	r, err := signin.SignIn(ctx, signin.Options{
//		Hub:    "longarm.example.com",
//		Client: client.DeviceTarget{ConfigDir: client.DefaultConfigDir()},
//		Worker: longarmd.WorkerTarget{DataDir: longarmd.DefaultDataDir()},
//	}, signin.Events{
//		Code:     func(c signin.Code) { show(c.URL, c.Code) }, // and a "Cancel" that cancels ctx
//		Progress: func(p signin.Progress) { status(p.Message) },
//	})
//	// r.Device.Name, r.Worker.Name, r.User, r.CAFingerprint
func SignIn(ctx context.Context, o Options, ev Events) (*Result, error) {
	if o.Client == nil && o.Worker == nil && o.AdminWorker == nil {
		return nil, errors.New("sign in as what? (no client and no worker)")
	}
	if o.AdminWorker != nil && o.Method == MethodAz {
		return nil, errors.New("an admin worker signs in through the browser only (not with az)")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ev.progress(StepResolve, "looking up the hub "+o.Hub)
	tg, err := hubclient.ResolveHub(ctx, o.Hub, hubclient.ResolveOptions{Fingerprint: o.Fingerprint, Roots: o.Roots})
	if err != nil {
		return nil, err
	}
	res := &Result{Hub: tg.Address, PanelURL: tg.PanelURL, CAFingerprint: tg.Fingerprint, CAFromHTTPS: tg.FromHTTPS}
	if tg.FromHTTPS {
		ev.progress(StepCA, fmt.Sprintf("hub %s: CA %s pinned (from %s/ca.crt, checked with this system's HTTPS roots)", tg.Address, tg.Fingerprint, tg.PanelURL))
	} else {
		ev.progress(StepCA, fmt.Sprintf("hub %s: CA %s pinned (given)", tg.Address, tg.Fingerprint))
	}
	var ctr, wtr, atr *pki.CSRTrust
	if o.Client != nil {
		if ctr, err = o.Client.Prepare(tg.Address); err != nil {
			return nil, err
		}
	}
	if o.Worker != nil {
		if wtr, err = o.Worker.Prepare(tg.Address); err != nil {
			return nil, err
		}
	}
	if o.AdminWorker != nil {
		if atr, err = o.AdminWorker.Prepare(tg.Address); err != nil {
			return nil, fmt.Errorf("the admin worker: %w", err)
		}
	}
	cjo := hubclient.JoinOptions{Name: o.DeviceName, Fingerprint: tg.Fingerprint, Trust: ctr}
	if o.Worker != nil && o.WorkerLabels == nil {
		o.WorkerLabels = InstalledLabels()
	}
	wjo := hubclient.JoinOptions{Name: o.WorkerName, Fingerprint: tg.Fingerprint, Trust: wtr, Labels: o.WorkerLabels}
	var ajo *hubclient.JoinOptions
	if o.AdminWorker != nil {
		ajo = &hubclient.JoinOptions{Name: o.AdminWorkerName, Fingerprint: tg.Fingerprint, Trust: atr}
	}
	// pre-made CSRs (keys held elsewhere: an admin worker's)
	for _, x := range []struct {
		t  Target
		jo *hubclient.JoinOptions
	}{{o.Client, &cjo}, {o.Worker, &wjo}, {o.AdminWorker, ajo}} {
		if cs, ok := x.t.(CSRSource); ok && x.jo != nil {
			if x.jo.CSR, err = cs.CSR(tg.Address); err != nil {
				return nil, err
			}
		}
	}

	var cr, wr, ar *hubclient.JoinResult
	method := o.Method
	if method == MethodAuto && o.AdminWorker != nil {
		ev.progress(StepNote, "the admin worker signs in through the browser: one code for everything")
		method = MethodLink
	}
	if method == MethodAz || method == MethodAuto {
		res.Method = MethodAz
		ev.progress(StepAz, "signing in with the Azure CLI's account")
		cr, wr, err = azJoin(ctx, tg.Address, o, cjo, wjo)
		if err != nil && method == MethodAuto && Fallback(err) {
			reason := "no az sign-in for this hub"
			if errors.Is(err, hubclient.ErrNoEntra) {
				reason = "this hub has no Microsoft Entra sign-in for az"
			}
			ev.progress(StepFallback, reason+": signing in through the browser instead (--az: az only)")
			method, err = MethodLink, nil
		}
		if err != nil {
			return nil, err
		}
	}
	if method != MethodAz && method != MethodAuto {
		res.Method = MethodLink
		if cr, wr, ar, err = pair(ctx, tg.Address, o, ev, cjo, wjo, ajo); err != nil {
			return nil, err
		}
	}

	// Installed only now: every requested part was approved.
	ev.progress(StepInstall, "installing the certificates")
	note := func(s string) { ev.progress(StepNote, s) }
	if wr != nil {
		dir, err := o.Worker.Install(tg.Address, wr, note)
		if err != nil {
			return nil, fmt.Errorf("installing the worker's enrollment: %w", err)
		}
		res.Worker = enrolled(wr, dir)
		res.User = wr.Resp.Owner
		if len(o.WorkerLabels) > 0 && len(wr.Resp.Labels) == 0 {
			note(fmt.Sprintf("this hub did not record the worker's labels (it is older); set them with: longarm worker label %s %s",
				wr.Resp.Name, labelArgs(o.WorkerLabels)))
		}
	}
	if ar != nil {
		dir, err := o.AdminWorker.Install(tg.Address, ar, note)
		if err != nil {
			return nil, fmt.Errorf("installing the admin worker's enrollment: %w", err)
		}
		res.AdminWorker = enrolled(ar, dir)
		if res.User == "" {
			res.User = ar.Resp.Owner
		}
	}
	if cr != nil {
		dir, err := o.Client.Install(tg.Address, cr, note)
		if err != nil {
			return nil, fmt.Errorf("installing the device's certificate: %w", err)
		}
		res.Device = enrolled(cr, dir)
		if c, err := pki.ParseCertPEM(cr.Resp.CertPem); err == nil {
			if id, err := pki.IdentityFromCert(c); err == nil && id.User != "" {
				res.User = id.User
			}
		}
	}
	ev.progress(StepDone, res.summary())
	return res, nil
}

// Target is one enrollment SignIn makes: client.DeviceTarget (the
// user's longarm) or longarmd.WorkerTarget (longarmd's data dir).
type Target interface {
	// Prepare checks that the target may be (re-)enrolled with hub and
	// returns the end-to-end trust binding its CSR carries (nil: none).
	Prepare(hub string) (*pki.CSRTrust, error)
	// Install writes an approved enrollment; it returns the directory and
	// passes notes for the user to note.
	Install(hub string, r *hubclient.JoinResult, note func(string)) (string, error)
}

// CSRSource is a Target whose key SignIn never holds (an admin worker's,
// made by its LocalSystem longarmd): SignIn sends CSR's request instead of
// making a key, and Install gets a result with KeyPEM nil.
type CSRSource interface {
	CSR(hub string) ([]byte, error)
}

// Method is how SignIn gets the user's approval.
type Method int

const (
	// MethodLink: the hub's pairing - a code the user approves at the
	// hub's /link page in any browser (the zero value).
	MethodLink Method = iota
	// MethodAuto: the Azure CLI's token when az has one for the hub, else
	// the pairing.
	MethodAuto
	// MethodAz: the Azure CLI's token only.
	MethodAz
)

func (m Method) String() string {
	switch m {
	case MethodAuto:
		return "auto"
	case MethodAz:
		return "az"
	}
	return "link"
}

// Options for SignIn.
type Options struct {
	// Hub as the user typed it: "longarm.example.com",
	// "https://longarm.example.com", "host:port".
	Hub string
	// Fingerprint pins the hub CA explicitly (sha256 hex, longarm-hub ca
	// show); empty: bootstrapped over HTTPS.
	Fingerprint string
	// Client / Worker: what to enroll (nil: not that role).
	Client, Worker Target
	// AdminWorker: an admin worker of this machine to enroll with the same
	// approval (nil: none), usually a CSRSource; AdminWorkerName is its
	// requested name (default: <worker name or host name>-admin).
	AdminWorker     Target
	AdminWorkerName string
	// DeviceName (default <user>-<hostname>) and WorkerName (default: the
	// hostname) as requested from the hub.
	DeviceName, WorkerName string
	// WorkerLabels: labels for the worker (scheduler constraints, as
	// `longarm worker add --label`), recorded by the hub at enrollment;
	// nil: the ones an installer recorded (InstalledLabels: the MSI's
	// LABELS). Change them later with `longarm worker label`.
	WorkerLabels map[string]string
	Method       Method
	// Scope overrides the az token's scope (MethodAz / MethodAuto).
	Scope string
	// OpenBrowser: open the /link page here (when there is a desktop).
	OpenBrowser bool
	// Roots verify the hub panel's HTTPS certificate (nil: the system's;
	// tests).
	Roots *x509.CertPool
}

// Step names a SignIn progress step.
type Step string

const (
	StepResolve  Step = "resolve"  // finding the hub, fetching its CA
	StepCA       Step = "ca"       // the CA fingerprint pinned (in the message)
	StepAz       Step = "az"       // trying the Azure CLI's token
	StepFallback Step = "fallback" // az unusable: pairing instead (message: why)
	StepCode     Step = "code"     // a pairing code is shown (Events.Code has it)
	StepBrowser  Step = "browser"  // the /link page was opened here
	StepWaiting  Step = "waiting"  // waiting for the approval
	StepInstall  Step = "install"  // approved; writing the certificates
	StepNote     Step = "note"     // something for the user to read
	StepDone     Step = "done"     // finished (message: the summary)
)

// Progress is one SignIn progress event.
type Progress struct {
	Step    Step
	Message string // one line for the user
}

// Code is a pairing code to show: "open URL, sign in with Microsoft,
// enter Code".
type Code struct {
	URL     string    // the hub's /link page (never with the code in it)
	Code    string    // "BCDF-GHJK"
	Expires time.Time // when the code stops working (10 minutes)
	// What the approval enrolls: "client", "worker" or "both".
	Kind string
	// Host: this machine's name, which the /link page shows to check.
	Host string
	// AdminWorker: the approval also enrolls an admin worker (it runs
	// commands as SYSTEM): say so next to the code.
	AdminWorker bool
}

// Events receives SignIn's progress (nil funcs: ignored).
type Events struct {
	Progress func(Progress)
	Code     func(Code)
}

func (e Events) progress(s Step, msg string) {
	if e.Progress != nil {
		e.Progress(Progress{Step: s, Message: msg})
	}
}

// Result of a SignIn.
type Result struct {
	Hub           string // gRPC address now in hub.toml
	PanelURL      string // where the CA came from ("" when given)
	CAFingerprint string // pinned
	CAFromHTTPS   bool
	User          string // the Longarm user signed in
	Method        Method // MethodAz or MethodLink: how it was approved
	Device        *Enrolled
	Worker        *Enrolled
	AdminWorker   *Enrolled // Options.AdminWorker's
}

// Enrolled is one installed enrollment.
type Enrolled struct {
	Name, Role string
	Owner      string // workers: the owning user
	Dir        string // config dir / data dir
	Message    string // the hub's message
	// Tailnet: the hub's tailnet hostname the node joins ("": none)
	Tailnet string
	// Labels the hub recorded for a worker
	Labels map[string]string
}

func enrolled(r *hubclient.JoinResult, dir string) *Enrolled {
	e := &Enrolled{Name: r.Resp.Name, Role: r.Resp.Role, Owner: r.Resp.Owner, Dir: dir, Message: r.Resp.Message, Labels: r.Resp.Labels}
	if t := r.Resp.Tailnet; t != nil {
		e.Tailnet = t.Hostname
	}
	return e
}

func (r *Result) summary() string {
	s := "signed in to " + r.Hub
	if r.User != "" {
		s += " as " + r.User
	}
	if r.Device != nil {
		s += "; device " + r.Device.Name + " (" + r.Device.Role + ")"
	}
	if r.Worker != nil {
		s += "; worker " + r.Worker.Name
	}
	if r.AdminWorker != nil {
		s += "; admin worker " + r.AdminWorker.Name + " (runs as SYSTEM, yours only)"
	}
	return s
}

// azJoin enrolls the requested parts with az's token (no approval).
func azJoin(ctx context.Context, addr string, o Options, cjo, wjo hubclient.JoinOptions) (cr, wr *hubclient.JoinResult, err error) {
	tok := func(ctx context.Context, info *pb.AuthInfoResponse) (string, error) {
		return AzToken(ctx, info, o.Scope)
	}
	actx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if o.Worker != nil && o.Client != nil {
		// all or nothing: a device that fails takes the worker back
		// (hubclient.LoginBoth explains why revoking beats reusing)
		cr, wr, err = hubclient.LoginBoth(actx, addr, hubclient.LoginOptions{JoinOptions: cjo, Token: tok},
			hubclient.LoginOptions{JoinOptions: wjo, Token: tok})
		if err != nil {
			return nil, nil, err
		}
		return cr, wr, checkWorker(wr)
	}
	if o.Worker != nil {
		if wr, err = hubclient.Login(actx, addr, hubclient.LoginOptions{JoinOptions: wjo, Kind: "worker", Token: tok}); err != nil {
			return nil, nil, err
		}
		return nil, wr, checkWorker(wr)
	}
	if cr, err = hubclient.Login(actx, addr, hubclient.LoginOptions{JoinOptions: cjo, Token: tok}); err != nil {
		return nil, nil, err
	}
	if cr.Resp.State != "approved" {
		return nil, nil, fmt.Errorf("enrollment is %s: %s", cr.Resp.State, cr.Resp.Message)
	}
	return cr, nil, nil
}

// pair enrolls the requested parts through the hub's /link pairing: one
// code for both when both are asked for.
// An admin worker (ajo) goes with the one code (alone: kind "admin");
// older hubs without "both" can't have one (hubclient.ErrNoAdminLink).
func pair(ctx context.Context, addr string, o Options, ev Events, cjo, wjo hubclient.JoinOptions, ajo *hubclient.JoinOptions) (cr, wr, ar *hubclient.JoinResult, err error) {
	link := func(kind string, jo hubclient.JoinOptions, w hubclient.JoinOptions, a *hubclient.JoinOptions) (*hubclient.JoinResult, error) {
		return hubclient.Link(ctx, addr, hubclient.LinkOptions{JoinOptions: jo, Kind: kind, Worker: w, AdminWorker: a,
			Show: func(st *pb.StartLinkResponse) {
				c := Code{URL: st.VerificationUrl, Code: st.UserCode, Expires: st.ExpiresAt.AsTime(), Kind: kind, Host: hostname(),
					AdminWorker: ajo != nil}
				ev.progress(StepCode, fmt.Sprintf("open %s and enter code %s", c.URL, c.Code))
				if ev.Code != nil {
					ev.Code(c)
				}
				if o.OpenBrowser && OpenBrowser(st.VerificationUrl) == nil {
					ev.progress(StepBrowser, "opened the page in your browser")
				}
				ev.progress(StepWaiting, "waiting for approval ...")
			}})
	}
	switch {
	case o.Client == nil && o.Worker == nil: // the admin worker alone
		if ar, err = link("admin", *ajo, hubclient.JoinOptions{}, nil); err != nil {
			return nil, nil, nil, err
		}
		return nil, nil, ar, checkAdminWorker(ar)
	case o.Client != nil && o.Worker != nil:
		r, err := link("both", cjo, wjo, ajo)
		if errors.Is(err, hubclient.ErrNoBothLink) {
			if ajo != nil {
				return nil, nil, nil, hubclient.ErrNoAdminLink
			}
			ev.progress(StepNote, "this hub is older: one code for the worker, then one for the device")
			if wr, err = link("worker", wjo, hubclient.JoinOptions{}, nil); err != nil {
				return nil, nil, nil, err
			}
			if err := checkWorker(wr); err != nil {
				return nil, nil, nil, err
			}
			if cr, err = link("client", cjo, hubclient.JoinOptions{}, nil); err != nil {
				// all or nothing, as LoginBoth: take the worker back
				if aerr := hubclient.Abandon(ctx, addr, wr, "sign-in failed"); aerr != nil {
					return nil, nil, nil, fmt.Errorf("%w (and worker %s could not be taken back: %v; remove it: longarm worker remove %s)", err, wr.Resp.Name, aerr, wr.Resp.Name)
				}
				return nil, nil, nil, err
			}
			return cr, wr, nil, nil
		}
		if err != nil {
			return nil, nil, nil, err
		}
		if err := checkWorker(r.Worker); err != nil {
			return nil, nil, nil, err
		}
		w, a := r.Worker, r.AdminWorker
		r.Worker, r.AdminWorker = nil, nil
		return r, w, a, checkAdminWorker(a)
	case o.Worker != nil:
		if wr, err = link("worker", wjo, hubclient.JoinOptions{}, ajo); err != nil {
			return nil, nil, nil, err
		}
		ar, wr.AdminWorker = wr.AdminWorker, nil
		if err := checkWorker(wr); err != nil {
			return nil, nil, nil, err
		}
		return nil, wr, ar, checkAdminWorker(ar)
	default:
		if cr, err = link("client", cjo, hubclient.JoinOptions{}, ajo); err != nil {
			return nil, nil, nil, err
		}
		ar, cr.AdminWorker = cr.AdminWorker, nil
		return cr, nil, ar, checkAdminWorker(ar)
	}
}

// checkAdminWorker checks an admin worker's enrollment, if there is one.
func checkAdminWorker(r *hubclient.JoinResult) error {
	if r == nil {
		return nil
	}
	if err := checkWorker(r); err != nil {
		return err
	}
	if r.Resp.Labels[sched.AdminLabel] != "true" {
		return errors.New("the hub did not mark the admin worker as one")
	}
	return nil
}

func checkWorker(r *hubclient.JoinResult) error {
	if r == nil || r.Resp.State != "approved" || r.Resp.Role != "worker" || r.Resp.Owner == "" {
		if r == nil {
			return errors.New("the hub sent no worker enrollment")
		}
		return fmt.Errorf("the hub answered %s %s (owner %q), not an owned worker", r.Resp.State, r.Resp.Role, r.Resp.Owner)
	}
	return nil
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

// labelArgs renders labels as KEY=VALUE arguments, sorted.
func labelArgs(m map[string]string) string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}
