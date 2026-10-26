package telemetry

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// Crash reports: a long-running process directs the Go runtime's crash
// output (unrecovered panics in any goroutine, fatal errors) to
// <dir>/crash-<component>-<pid>.txt (WatchCrashes). A clean exit removes the
// file; the next start of any process using the dir sends a non-empty one
// as an exception (ReportCrashes) and removes it. The file is local and
// holds the raw stack; only function names and file:line are sent.

const crashHeader = "longarm-crash v1"

// WatchCrashes sends the runtime's crash output to a crash file in the
// client's dir. stop (call it on a clean exit) removes the file.
func (c *Client) WatchCrashes() (stop func()) {
	if c == nil || c.cfg.Dir == "" || c.cfg.Disabled {
		return func() {}
	}
	if err := os.MkdirAll(c.cfg.Dir, 0o700); err != nil {
		return func() {}
	}
	path := filepath.Join(c.cfg.Dir, fmt.Sprintf("crash-%s-%d.txt", c.cfg.Component, os.Getpid()))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return func() {}
	}
	fmt.Fprintf(f, "%s component=%s version=%s pid=%d started=%s\n", crashHeader, c.cfg.Component, c.cfg.Version,
		os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		f.Close()
		os.Remove(path)
		return func() {}
	}
	f.Close() // the runtime holds its own descriptor
	c.watching.Store(true)
	return func() {
		c.watching.Store(false)
		debug.SetCrashOutput(nil, debug.CrashOptions{})
		os.Remove(path)
	}
}

// WriteCrash writes a recovered panic to a crash file in dir (the next
// start reports it).
func WriteCrash(dir, component, version string, v any, stack []byte) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, fmt.Sprintf("crash-%s-%d-%d.txt", component, os.Getpid(), time.Now().UnixNano()))
	body := fmt.Sprintf("%s component=%s version=%s pid=%d\npanic: %v\n\n%s", crashHeader, component, version, os.Getpid(), v, stack)
	return os.WriteFile(path, []byte(body), 0o600)
}

// Guard is deferred at the top of main and of goroutines worth it in
// processes without WatchCrashes: it writes a recovered panic to a crash
// file (the next start reports it), then panics again - the process still
// crashes and a service manager restarts it.
func Guard() {
	r := recover()
	if r == nil {
		return
	}
	if c := Default(); c.Enabled() && !c.watching.Load() {
		WriteCrash(c.cfg.Dir, c.cfg.Component, c.cfg.Version, r, debug.Stack())
	}
	panic(r)
}

// ReportCrashes turns crash files in the dir into exception items and
// removes them. Empty ones (the process is alive, or was killed) older
// than a week are removed too.
func (c *Client) ReportCrashes() int {
	if c == nil || c.cfg.Dir == "" {
		return 0
	}
	files, _ := filepath.Glob(filepath.Join(c.cfg.Dir, "crash-*.txt"))
	n := 0
	for _, f := range files {
		it, ok, err := ReadCrash(f)
		if err != nil {
			continue
		}
		if !ok {
			if fi, err := os.Stat(f); err == nil && time.Since(fi.ModTime()) > 7*24*time.Hour {
				os.Remove(f)
			}
			continue
		}
		if os.Remove(f) != nil {
			continue // in use: its process may still be writing
		}
		if c.Enabled() {
			ctx := c.ctx
			if it.Context.Component != "" {
				ctx.Component, ctx.Version = it.Context.Component, it.Context.Version
			}
			it.Context = ctx
			c.record(context.Background(), it)
			n++
		}
	}
	return n
}

// ReadCrash parses a crash file; ok = false when it holds no crash.
func ReadCrash(path string) (Item, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Item{}, false, err
	}
	s := string(b)
	header, body, _ := strings.Cut(s, "\n")
	it := Item{Kind: KindException, Severity: SevCritical, Props: map[string]string{"crash": "true"}}
	if strings.HasPrefix(header, crashHeader) {
		for _, f := range strings.Fields(header) {
			k, v, _ := strings.Cut(f, "=")
			switch k {
			case "component":
				it.Context.Component = v
			case "version":
				it.Context.Version = v
			case "started":
				if t, err := time.Parse(time.RFC3339, v); err == nil {
					it.Props["uptime_s"] = strconv.FormatInt(int64(time.Since(t).Seconds()), 10)
				}
			}
		}
	} else {
		body = s
	}
	body = strings.TrimLeft(body, "\r\n")
	if strings.TrimSpace(body) == "" {
		return it, false, nil
	}
	if fi, err := os.Stat(path); err == nil {
		it.Time = fi.ModTime()
	}
	first, _, _ := strings.Cut(body, "\n")
	first = strings.TrimSpace(first)
	switch {
	case strings.HasPrefix(first, "panic: "):
		it.Name, it.Message = "panic", strings.TrimPrefix(first, "panic: ")
	case strings.HasPrefix(first, "fatal error: "):
		it.Name, it.Message = "fatal error", strings.TrimPrefix(first, "fatal error: ")
	default:
		it.Name, it.Message = "crash", first
	}
	it.Message = strings.TrimSuffix(it.Message, " [recovered]")
	it.Stack = ParseStack(body)
	return it, true, nil
}

// ParseStack extracts the frames of the first goroutine of a Go traceback
// (debug.Stack, crash output): function names and file:line.
func ParseStack(s string) []Frame {
	var out []Frame
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	inG := false
	var fn string
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case strings.HasPrefix(line, "goroutine "):
			if inG && len(out) > 0 {
				return out
			}
			inG = true
			fn = ""
		case !inG:
		case line == "":
			if len(out) > 0 {
				return out
			}
		case strings.HasPrefix(line, "\t"):
			if fn == "" {
				continue
			}
			loc := strings.TrimSpace(line)
			if i := strings.LastIndex(loc, " +0x"); i > 0 {
				loc = loc[:i]
			}
			file, ln := loc, 0
			if i := strings.LastIndexByte(loc, ':'); i > 0 {
				if n, err := strconv.Atoi(loc[i+1:]); err == nil {
					file, ln = loc[:i], n
				}
			}
			out = append(out, Frame{Function: fn, File: trimFile(file), Line: ln})
			fn = ""
			if len(out) >= MaxFrames {
				return out
			}
		default:
			fn = line
			if i := strings.LastIndexByte(fn, '('); i > 0 && strings.HasSuffix(fn, ")") {
				fn = fn[:i] // drop the argument words
			}
			fn = strings.TrimPrefix(fn, "created by ")
			if i := strings.Index(fn, " in goroutine "); i > 0 {
				fn = fn[:i]
			}
		}
	}
	return out
}

// Callers is the current stack (skip frames above the caller).
func Callers(skip int) []Frame {
	pcs := make([]uintptr, MaxFrames)
	n := runtime.Callers(skip+2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var out []Frame
	for {
		f, more := frames.Next()
		out = append(out, Frame{Function: f.Function, File: trimFile(f.File), Line: f.Line})
		if !more {
			return out
		}
	}
}

// trimFile keeps the part of a source path that says which file it is
// without where it was built: module-relative for Longarm, below
// pkg/mod for dependencies, below src for the standard library.
func trimFile(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // trimmed paths stay as they are
	}
	for _, marker := range []string{"/internal/", "/cmd/", "/proto/", "/e2e/"} {
		if i := strings.LastIndex(p, marker); i >= 0 && !strings.Contains(p, "/pkg/mod/") {
			return p[i+1:]
		}
	}
	if i := strings.LastIndex(p, "/pkg/mod/"); i >= 0 {
		return p[i+len("/pkg/mod/"):]
	}
	if i := strings.LastIndex(p, "/src/"); i >= 0 {
		return p[i+len("/src/"):]
	}
	parts := strings.Split(p, "/")
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, "/")
}
