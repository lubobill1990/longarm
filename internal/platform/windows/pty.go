//go:build windows

package windows

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/lubobill1990/longarm/internal/platform/ptypes"
)

// PTY is a process running on a pseudo console (ConPTY, Windows 10 1809+).
// Read returns the terminal's VT/UTF-8 output; Write feeds keyboard input.
type PTY struct {
	hpc  windows.Handle
	in   *os.File // our end of the input pipe
	out  *os.File // our end of the output pipe
	proc windows.Handle
	pid  int

	closeOnce sync.Once
	waitOnce  sync.Once
	code      int
	waitErr   error
}

// StartPTY starts o.Argv on a new pseudo console inside the container: the
// process is created suspended, assigned to the Job Object and resumed, like
// Container.Start.
func (c *Container) StartPTY(o ptypes.PTYOptions) (*PTY, error) {
	if len(o.Argv) == 0 {
		return nil, errors.New("empty argv")
	}
	app, err := lookPath(o.Argv[0], o.Env)
	if err != nil {
		return nil, err
	}
	var inR, inW, outR, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inW, nil, 0); err != nil {
		return nil, fmt.Errorf("CreatePipe: %w", err)
	}
	if err := windows.CreatePipe(&outR, &outW, nil, 0); err != nil {
		windows.CloseHandle(inR)
		windows.CloseHandle(inW)
		return nil, fmt.Errorf("CreatePipe: %w", err)
	}
	var hpc windows.Handle
	err = windows.CreatePseudoConsole(coord(o.Cols, o.Rows), inR, outW, 0, &hpc)
	// The pseudo console duplicated its ends of the pipes.
	windows.CloseHandle(inR)
	windows.CloseHandle(outW)
	p := &PTY{in: os.NewFile(uintptr(inW), "pty-in"), out: os.NewFile(uintptr(outR), "pty-out")}
	if err != nil {
		p.in.Close()
		p.out.Close()
		return nil, fmt.Errorf("CreatePseudoConsole: %w", err)
	}
	p.hpc = hpc
	fail := func(err error) (*PTY, error) {
		p.Close()
		p.Release()
		return nil, err
	}

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return fail(err)
	}
	defer attrs.Delete()
	// The attribute value is the HPCON itself, not a pointer to it.
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, *(*unsafe.Pointer)(unsafe.Pointer(&hpc)), unsafe.Sizeof(hpc)); err != nil {
		return fail(fmt.Errorf("UpdateProcThreadAttribute: %w", err))
	}
	si := windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(si))
	// No std handles: the child must use the pseudo console, not whatever
	// (redirected) handles the shim has.
	si.Flags = windows.STARTF_USESTDHANDLES

	appPtr, err := windows.UTF16PtrFromString(app)
	if err != nil {
		return fail(err)
	}
	line := o.CmdLine
	if line == "" {
		line = windows.ComposeCommandLine(o.Argv)
	}
	cmdline, err := windows.UTF16PtrFromString(line)
	if err != nil {
		return fail(err)
	}
	var dirPtr *uint16
	if o.Dir != "" {
		if dirPtr, err = windows.UTF16PtrFromString(o.Dir); err != nil {
			return fail(err)
		}
	}
	env := envBlock(o.Env)
	var pi windows.ProcessInformation
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT | createSuspended)
	if err := windows.CreateProcess(appPtr, cmdline, nil, nil, false, flags, env, dirPtr, &si.StartupInfo, &pi); err != nil {
		return fail(fmt.Errorf("CreateProcess %s: %w", app, err))
	}
	defer windows.CloseHandle(pi.Thread)
	p.proc, p.pid = pi.Process, int(pi.ProcessId)
	if err := c.assign(pi.ProcessId); err != nil {
		windows.TerminateProcess(pi.Process, KilledExitCode)
		return fail(err)
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		windows.TerminateProcess(pi.Process, KilledExitCode)
		return fail(fmt.Errorf("resume: %w", err))
	}
	return p, nil
}

func coord(cols, rows uint16) windows.Coord {
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 25
	}
	return windows.Coord{X: int16(cols), Y: int16(rows)}
}

// lookPath resolves argv0 using the PATH/PATHEXT of the child environment.
func lookPath(argv0 string, env []string) (string, error) {
	if filepath.IsAbs(argv0) || strings.ContainsAny(argv0, `\/`) {
		return exec.LookPath(argv0)
	}
	path, pathext := os.Getenv("PATH"), os.Getenv("PATHEXT")
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "PATH":
			path = v
		case "PATHEXT":
			pathext = v
		}
	}
	exts := strings.Split(strings.ToLower(pathext), ";")
	if pathext == "" {
		exts = []string{".com", ".exe", ".bat", ".cmd"}
	}
	hasExt := filepath.Ext(argv0) != ""
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		cand := filepath.Join(dir, argv0)
		if hasExt {
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				return cand, nil
			}
		}
		for _, e := range exts {
			if e == "" {
				continue
			}
			if fi, err := os.Stat(cand + e); err == nil && !fi.IsDir() {
				return cand + e, nil
			}
		}
	}
	return "", &exec.Error{Name: argv0, Err: exec.ErrNotFound}
}

// envBlock builds a sorted, de-duplicated (case-insensitively, last wins)
// UTF-16 environment block; nil inherits the caller's environment.
func envBlock(env []string) *uint16 {
	if len(env) == 0 {
		return nil
	}
	idx := map[string]int{}
	var kept []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv[min(1, len(kv)):], "=") // "=C:=C:\x" keeps its leading '='
		k = strings.ToUpper(kv[:min(1, len(kv))] + k)
		if i, ok := idx[k]; ok {
			kept[i] = kv
			continue
		}
		idx[k] = len(kept)
		kept = append(kept, kv)
	}
	sort.Slice(kept, func(i, j int) bool { return strings.ToUpper(kept[i]) < strings.ToUpper(kept[j]) })
	var b []uint16
	for _, kv := range kept {
		b = append(b, utf16.Encode([]rune(kv))...)
		b = append(b, 0)
	}
	b = append(b, 0)
	return &b[0]
}

// Read reads terminal output. It returns io.EOF after Close.
func (p *PTY) Read(b []byte) (int, error) { return p.out.Read(b) }

// Write sends keyboard input.
func (p *PTY) Write(b []byte) (int, error) { return p.in.Write(b) }

// Resize changes the terminal size.
func (p *PTY) Resize(cols, rows uint16) error {
	return windows.ResizePseudoConsole(p.hpc, coord(cols, rows))
}

// Pid of the process.
func (p *PTY) Pid() int { return p.pid }

// Wait waits for the process to exit and returns its exit code.
func (p *PTY) Wait() (int, error) {
	p.waitOnce.Do(func() {
		if _, err := windows.WaitForSingleObject(p.proc, windows.INFINITE); err != nil {
			p.code, p.waitErr = -1, err
			return
		}
		var code uint32
		if err := windows.GetExitCodeProcess(p.proc, &code); err != nil {
			p.code, p.waitErr = -1, err
			return
		}
		p.code = int(code)
	})
	return p.code, p.waitErr
}

// Close closes the pseudo console; pending output is flushed and Read then
// returns EOF. Keep reading while closing: on older Windows builds
// ClosePseudoConsole blocks until the output pipe is drained.
func (p *PTY) Close() error {
	p.closeOnce.Do(func() {
		if p.hpc != 0 {
			windows.ClosePseudoConsole(p.hpc)
		}
		p.in.Close()
	})
	return nil
}

// Release frees the process handle and the output pipe (after Wait, Close
// and the last Read).
func (p *PTY) Release() {
	p.out.Close()
	if p.proc != 0 {
		windows.CloseHandle(p.proc)
		p.proc = 0
	}
}
