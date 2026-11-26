//go:build windows

package desktopsvc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/mgr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	win "longarm/internal/platform/windows"
	"longarm/internal/rdesk"
	"longarm/internal/screen"
	"longarm/internal/updatesvc"
	pb "longarm/proto/longarm/v1"
)

const (
	systemSID = "S-1-5-18"
	policyKey = `SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System`
	sasPolicy = "SoftwareSASGeneration"
	// sasSDDL: SYSTEM only (the agent).
	sasSDDL = "D:P(A;;GA;;;SY)"
)

// RunService is the service's work (session 0, SYSTEM) until ctx ends:
// the agent in the console session, Ctrl+Alt+Del on its request. exe is
// longarmd's path.
func RunService(ctx context.Context, exe string, log *slog.Logger) error {
	ln, err := winio.ListenPipe(SASPipe, &winio.PipeConfig{SecurityDescriptor: sasSDDL})
	if err != nil {
		return fmt.Errorf("%s: %w", SASPipe, err)
	}
	defer ln.Close()
	go serveSAS(ln, log)
	cmdline := syscall.EscapeArg(exe) + " desktop-agent"
	for ctx.Err() == nil {
		sid := screen.ConsoleSession()
		if sid == 0xFFFFFFFF { // switching
			sleep(ctx, time.Second)
			continue
		}
		proc, err := screen.StartInSession(sid, cmdline)
		if err != nil {
			log.Error("desktop agent: start", "session", sid, "err", err)
			sleep(ctx, 5*time.Second)
			continue
		}
		log.Info("desktop agent started", "session", sid)
		watchAgent(ctx, proc, sid, log)
		windows.CloseHandle(proc)
		sleep(ctx, time.Second)
	}
	return nil
}

// watchAgent returns when the agent exits, the console moves to another
// session (the agent is ended: it would show a desktop nobody sees) or
// ctx ends.
func watchAgent(ctx context.Context, proc windows.Handle, sid uint32, log *slog.Logger) {
	for {
		ev, _ := windows.WaitForSingleObject(proc, 2000)
		if ev == windows.WAIT_OBJECT_0 {
			var code uint32
			windows.GetExitCodeProcess(proc, &code)
			log.Warn("desktop agent exited", "code", code)
			return
		}
		if ctx.Err() != nil {
			endAgent(proc) // before the service reports stopped: an installer replaces longarmd.exe next
			return
		}
		if now := screen.ConsoleSession(); now != sid && now != 0xFFFFFFFF {
			log.Info("console session changed: moving the desktop agent", "from", sid, "to", now)
			endAgent(proc)
			return
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// serveSAS sends Ctrl+Alt+Del for SYSTEM callers (the agent): one line
// "sas" in, "ok" or "error: ..." out.
func serveSAS(ln net.Listener, log *slog.Logger) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			caller, err := updatesvc.IdentifyPipeClient(c)
			if err != nil || !caller.System {
				log.Warn("Ctrl+Alt+Del refused", "caller", caller, "err", err)
				return
			}
			c.SetDeadline(time.Now().Add(5 * time.Second))
			line, _ := bufio.NewReader(c).ReadString('\n')
			if strings.TrimSpace(line) != "sas" {
				return
			}
			if err := screen.SendSAS(); err != nil {
				fmt.Fprintf(c, "error: %v\n", err)
				return
			}
			log.Info("sent Ctrl+Alt+Del")
			io.WriteString(c, "ok\n")
		}()
	}
}

// requestSAS asks the service for Ctrl+Alt+Del (the agent's
// screen.SecureAttentionVia).
func requestSAS() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Identification: the service checks that the caller is SYSTEM.
	c, err := winio.DialPipeAccessImpLevel(ctx, SASPipe, windows.GENERIC_READ|windows.GENERIC_WRITE, winio.PipeImpLevelIdentification)
	if err != nil {
		return fmt.Errorf("the Longarm Desktop service: %w", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, "sas\n"); err != nil {
		return err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return fmt.Errorf("the Longarm Desktop service: %w", err)
	}
	if line = strings.TrimSpace(line); line != "ok" {
		return errors.New(line)
	}
	return nil
}

// agentSDDL: SYSTEM and administrators, and the recorded clients (read and
// write: enough to connect).
func agentSDDL(clients []string) string {
	s := "D:P(A;;GA;;;SY)(A;;GA;;;BA)"
	for _, c := range clients {
		if _, err := windows.StringToSid(c); err == nil {
			s += "(A;;GRGW;;;" + c + ")"
		}
	}
	return s
}

// Clients are the SIDs the installer allowed to use the agent.
func Clients() []string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, RegKey, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	v, _, _ := k.GetStringsValue("Clients")
	return v
}

// RunAgent serves RemoteDesktop on AgentPipe until ctx ends (SYSTEM in the
// console session).
func RunAgent(ctx context.Context, log *slog.Logger) error {
	var ln net.Listener
	var err error
	// The previous agent may still be going away.
	for i := 0; i < 20; i++ {
		if ln, err = winio.ListenPipe(AgentPipe, &winio.PipeConfig{SecurityDescriptor: agentSDDL(Clients())}); err == nil {
			break
		}
		sleep(ctx, 250*time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", AgentPipe, err)
	}
	screen.SecureAttentionVia = requestSAS
	g := grpc.NewServer()
	pb.RegisterRemoteDesktopServer(g, &agentServer{log: log})
	go func() { <-ctx.Done(); g.Stop() }()
	log.Info("desktop agent listening", "pipe", AgentPipe)
	return g.Serve(ln)
}

// agentServer is RemoteDesktop for longarmd, which decided who may use it
// (the pipe's DACL decided which processes may ask).
type agentServer struct {
	pb.UnimplementedRemoteDesktopServer
	log *slog.Logger
}

func (a *agentServer) Displays(context.Context, *pb.DesktopDisplaysRequest) (*pb.DesktopDisplaysResponse, error) {
	out := &pb.DesktopDisplaysResponse{NativeReason: screen.Available(), Codecs: rdesk.Codecs()}
	ds, err := screen.Displays()
	if err != nil && out.NativeReason == "" {
		out.NativeReason = err.Error()
	}
	for _, d := range ds {
		out.Displays = append(out.Displays, &pb.DesktopDisplay{Index: int32(d.Index), Name: d.Name,
			X: int32(d.X), Y: int32(d.Y), Width: int32(d.W), Height: int32(d.H)})
	}
	return out, nil
}

func (a *agentServer) Stream(st pb.RemoteDesktop_StreamServer) error {
	first, err := st.Recv()
	if err != nil {
		return err
	}
	if first.GetStart() == nil {
		return errors.New("the first message must be start")
	}
	return rdesk.Serve(st, first.GetStart(), a.log)
}

var agent struct {
	mu       sync.Mutex
	conn     *grpc.ClientConn
	lastMiss time.Time
}

// dialAgent opens the agent's pipe and checks that SYSTEM (or the
// administrators) created it: anybody may create a pipe of that name when
// the agent is not running.
func dialAgent(ctx context.Context) (net.Conn, error) {
	c, err := winio.DialPipeContext(ctx, AgentPipe)
	if err != nil {
		return nil, err
	}
	f, ok := c.(interface{ Fd() uintptr })
	if !ok {
		c.Close()
		return nil, errors.New("not a pipe")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err == nil {
		var owner *windows.SID
		if owner, _, err = sd.Owner(); err == nil && (owner == nil || !win.SystemOwnerSID(owner)) {
			err = fmt.Errorf("the pipe's owner is %v, not SYSTEM", owner)
		}
	}
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("%s: %w", AgentPipe, err)
	}
	return c, nil
}

// Agent reaches the agent for longarmd (server.DesktopAgent); ok is false
// when it does not run here (no service, or this process may not use it).
func Agent(ctx context.Context) (pb.RemoteDesktopClient, bool) {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if time.Since(agent.lastMiss) < 3*time.Second {
		return nil, false
	}
	// Probe: the pipe must be there and ours to open now (the agent moves
	// with the console session; gRPC would wait for it otherwise).
	pctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	c, err := dialAgent(pctx)
	cancel()
	if err != nil {
		agent.lastMiss = time.Now()
		return nil, false
	}
	c.Close()
	if agent.conn == nil {
		agent.conn, err = grpc.NewClient("passthrough:///longarm-desktop-agent",
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return dialAgent(ctx) }),
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20)))
		if err != nil {
			agent.lastMiss = time.Now()
			return nil, false
		}
	}
	return pb.NewRemoteDesktopClient(agent.conn), true
}

// Install creates (or updates) the service for longarmd at exe, allows
// clients (SIDs) to use the agent, turns the SoftwareSASGeneration policy
// on for services (remembering it did) and starts the service. Elevated.
func Install(exe string, clients []string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	cfg := mgr.Config{DisplayName: "Longarm Desktop", StartType: mgr.StartAutomatic,
		Description: "Lets the owner of this Longarm worker see and use its screen remotely, UAC prompts and the lock screen included"}
	s, err := m.OpenService(ServiceName)
	if err == nil {
		win.StopService(ServiceName)
		c, err := s.Config()
		if err == nil {
			c.BinaryPathName = syscall.EscapeArg(exe) + " desktop-service run"
			c.DisplayName, c.Description, c.StartType = cfg.DisplayName, cfg.Description, cfg.StartType
			err = s.UpdateConfig(c)
		}
		if err != nil {
			s.Close()
			return fmt.Errorf("update the %s service: %w", ServiceName, err)
		}
	} else if s, err = m.CreateService(ServiceName, exe, cfg, "desktop-service", "run"); err != nil {
		return fmt.Errorf("create the %s service: %w", ServiceName, err)
	}
	defer s.Close()
	s.SetRecoveryActions([]mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second}, {Type: mgr.ServiceRestart, Delay: time.Minute}}, 86400)

	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, RegKey, registry.ALL_ACCESS)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.SetStringsValue("Clients", clients); err != nil {
		return err
	}
	pk, _, err := registry.CreateKey(registry.LOCAL_MACHINE, policyKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer pk.Close()
	if v, _, err := pk.GetIntegerValue(sasPolicy); err != nil || v&1 == 0 {
		prev := int64(-1) // not set
		if err == nil {
			prev = int64(v)
		}
		if _, _, err := k.GetIntegerValue("SASPolicyPrev"); err != nil { // the first install records it
			k.SetQWordValue("SASPolicyPrev", uint64(prev))
		}
		if err := pk.SetDWordValue(sasPolicy, uint32(max(prev, 0))|1); err != nil {
			return fmt.Errorf("the %s policy: %w", sasPolicy, err)
		}
	}
	return win.StartService(ServiceName)
}

// Uninstall removes the service and puts the policy back as it was.
func Uninstall() error {
	var errs []error
	if err := win.UninstallService(ServiceName); err != nil && !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) &&
		!strings.Contains(err.Error(), "does not exist") {
		errs = append(errs, err)
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, RegKey, registry.QUERY_VALUE); err == nil {
		prev, _, perr := k.GetIntegerValue("SASPolicyPrev")
		k.Close()
		if perr == nil {
			if pk, err := registry.OpenKey(registry.LOCAL_MACHINE, policyKey, registry.SET_VALUE); err == nil {
				if int64(prev) < 0 {
					pk.DeleteValue(sasPolicy)
				} else {
					pk.SetDWordValue(sasPolicy, uint32(prev))
				}
				pk.Close()
			}
		}
		if err := registry.DeleteKey(registry.LOCAL_MACHINE, RegKey); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Installed: the service exists (asked with the rights any user has).
func Installed() bool {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return false
	}
	defer windows.CloseServiceHandle(m)
	name, _ := windows.UTF16PtrFromString(ServiceName)
	s, err := windows.OpenService(m, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return false
	}
	windows.CloseServiceHandle(s)
	return true
}

// LogFile is where the service and the agent log (SYSTEM and
// administrators only: ProgramData\Longarm\desktop).
func LogFile(name string) (*os.File, error) {
	dir := filepath.Join(os.Getenv("ProgramData"), "Longarm", "desktop")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, name)
	if fi, err := os.Stat(p); err == nil && fi.Size() > 8<<20 {
		os.Rename(p, p+".1")
	}
	return os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

// RunningAsSystem: this process is LocalSystem.
func RunningAsSystem() bool {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	return err == nil && u.User.Sid.String() == systemSID
}

// AccountSID is the SID of a Windows account (".\alice", "DOMAIN\bob").
func AccountSID(name string) (string, error) { return win.AccountSID(name) }

// CurrentUserSID is this process's account's SID.
func CurrentUserSID() string {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return ""
	}
	return u.User.Sid.String()
}

// endAgent ends the agent and waits until it is gone (its image is free).
func endAgent(proc windows.Handle) {
	windows.TerminateProcess(proc, 0)
	windows.WaitForSingleObject(proc, 10000)
}
