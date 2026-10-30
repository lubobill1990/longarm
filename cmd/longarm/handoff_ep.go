package main

// The machines of longarm handoff / agent-sessions (docs/usage.md#hand-off-an-agent-session):
// this one ("local") or a worker, behind one set of file and command
// operations. Worker files go through the Files service and commands
// through Exec - worker-direct, inside the mTLS connection (the hub relays
// TLS records at most) - so transcripts never pass the hub.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/client"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// localMachine is the name of this machine in handoff and agent-sessions.
const localMachine = "local"

// hoEndpoint is one machine: local (conn nil) or a worker.
type hoEndpoint struct {
	name       string
	cert       string // the worker's certificate name (Node.Info)
	conn       *client.Conn
	goos       string // "windows", "linux", "darwin"
	home       string // absolute home directory (of the worker's account the files and sessions use)
	callerUser string // the user the worker sees us as ("" = no users)
	owner      string // the worker's owner ("" = none: a fleet worker, or no hub)
	isolation  string // "shared" | "per-user" ("" locally)
	version    string // longarmd's
	features   []string
}

func (e *hoEndpoint) local() bool { return e.conn == nil }

func goosLocal() string { return runtime.GOOS }

func (e *hoEndpoint) windows() bool { return e.goos == "windows" }

// sep is the endpoint's path separator.
func (e *hoEndpoint) sep() string {
	if e.windows() {
		return `\`
	}
	return "/"
}

// join joins path elements with the endpoint's separator.
func (e *hoEndpoint) join(elem ...string) string {
	if e.windows() {
		var parts []string
		for _, p := range elem {
			if p != "" {
				parts = append(parts, strings.ReplaceAll(p, "/", `\`))
			}
		}
		s := strings.Join(parts, `\`)
		for strings.Contains(s, `\\`) && !strings.HasPrefix(s, `\\`) {
			s = strings.ReplaceAll(s, `\\`, `\`)
		}
		return s
	}
	return path.Join(elem...)
}

// native converts a path a tool printed (git prints C:/x on Windows) to the
// endpoint's separators.
func (e *hoEndpoint) native(p string) string {
	if e.windows() {
		return strings.ReplaceAll(p, "/", `\`)
	}
	return p
}

// dir is the parent directory of p.
func (e *hoEndpoint) dir(p string) string {
	if e.windows() {
		i := strings.LastIndex(p, `\`)
		if i <= 2 { // C:\x -> C:\
			return p[:i+1]
		}
		return p[:i]
	}
	return path.Dir(p)
}

// samePath compares two paths of the endpoint (Windows: case-insensitive,
// either separator).
func (e *hoEndpoint) samePath(a, b string) bool {
	if e.windows() {
		return strings.EqualFold(strings.TrimRight(e.native(a), `\`), strings.TrimRight(e.native(b), `\`))
	}
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}

// within reports whether p is dir or inside it.
func (e *hoEndpoint) within(p, dir string) bool {
	if e.samePath(p, dir) {
		return true
	}
	s := e.sep()
	pp, dd := p, strings.TrimRight(dir, s)+s
	if e.windows() {
		pp, dd = strings.ToLower(e.native(pp)), strings.ToLower(e.native(dd))
	}
	return strings.HasPrefix(pp, dd)
}

// openEndpoint resolves a machine name: local, or a worker (asked for its
// OS, home directory and features).
func openEndpoint(ctx context.Context, s *session, name string) (*hoEndpoint, error) {
	if name == "" || name == localMachine {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		return &hoEndpoint{name: localMachine, goos: runtime.GOOS, home: home}, nil
	}
	conn, err := s.conn(name)
	if err != nil {
		return nil, err
	}
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	info, err := conn.Node().Info(c, &pb.InfoRequest{})
	if err != nil {
		conn.Close()
		return nil, err
	}
	home, err := conn.Files().Stat(c, &pb.FileStatRequest{Path: "~"})
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &hoEndpoint{name: name, cert: info.Name, conn: conn, goos: info.Os, home: home.AbsPath, callerUser: info.CallerUser,
		owner: info.Owner, isolation: info.Isolation, version: info.Version, features: info.Features}, nil
}

func (e *hoEndpoint) close() {
	if e != nil && e.conn != nil {
		e.conn.Close()
	}
}

// hoFile is a file or directory found by list (rel: '/'-separated, relative
// to the listed directory).
type hoFile struct {
	rel   string
	isDir bool
	size  int64
	mtime time.Time
}

// list walks dir recursively (symlinks are skipped); a missing dir is empty.
func (e *hoEndpoint) list(ctx context.Context, dir string) ([]hoFile, error) {
	var out []hoFile
	if e.local() {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if p == dir && errors.Is(err, fs.ErrNotExist) {
					return fs.SkipAll
				}
				return err
			}
			if p == dir || d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return nil // vanished meanwhile
			}
			rel, _ := filepath.Rel(dir, p)
			out = append(out, hoFile{rel: filepath.ToSlash(rel), isDir: d.IsDir(), size: fi.Size(), mtime: fi.ModTime()})
			return nil
		})
		return out, err
	}
	c, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	resp, err := e.conn.Files().List(c, &pb.FileListRequest{Path: dir})
	if status.Code(err) == codes.NotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, f := range resp.Entries {
		if f.IsSymlink {
			continue
		}
		out = append(out, hoFile{rel: f.Path, isDir: f.IsDir, size: f.Size, mtime: f.Mtime.AsTime()})
	}
	return out, nil
}

// stat reports a path (exists false when it does not).
func (e *hoEndpoint) stat(ctx context.Context, p string) (hoFile, bool, error) {
	if e.local() {
		fi, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return hoFile{}, false, nil
		}
		if err != nil {
			return hoFile{}, false, err
		}
		return hoFile{rel: p, isDir: fi.IsDir(), size: fi.Size(), mtime: fi.ModTime()}, true, nil
	}
	c, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	fi, err := e.conn.Files().Stat(c, &pb.FileStatRequest{Path: p})
	if err != nil {
		return hoFile{}, false, err
	}
	if !fi.Exists {
		return hoFile{}, false, nil
	}
	return hoFile{rel: p, isDir: fi.IsDir, size: fi.Size, mtime: fi.Mtime.AsTime()}, true, nil
}

// readRange reads at most n bytes of p from offset off (negative: that many
// bytes before the end). A missing file is fs.ErrNotExist.
func (e *hoEndpoint) readRange(ctx context.Context, p string, off, n int64) ([]byte, error) {
	if e.local() {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if off < 0 {
			fi, err := f.Stat()
			if err != nil {
				return nil, err
			}
			off = max(fi.Size()+off, 0)
		}
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return nil, err
		}
		return io.ReadAll(io.LimitReader(f, n))
	}
	if off < 0 {
		fi, ok, err := e.stat(ctx, p)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fs.ErrNotExist
		}
		off = max(fi.size+off, 0)
	}
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err := e.conn.Files().Read(c, &pb.FileReadRequest{Path: p, Offset: off})
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	for int64(buf.Len()) < n {
		ch, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if status.Code(err) == codes.NotFound {
			return nil, fs.ErrNotExist
		}
		if err != nil {
			return nil, err
		}
		buf.Write(ch.Data)
	}
	b := buf.Bytes()
	if int64(len(b)) > n {
		b = b[:n]
	}
	return b, nil
}

// readSmall reads a whole small file (config files; at most max bytes).
func (e *hoEndpoint) readSmall(ctx context.Context, p string, max int64) ([]byte, error) {
	return e.readRange(ctx, p, 0, max)
}

// download copies file p of the endpoint to the new local file dst
// (0600, created exclusively) and returns its size: at most limit bytes,
// and - from a worker - exactly the size and sha256 it reported, or an
// error (a worker cannot make this machine store more than it allows).
func (e *hoEndpoint) download(ctx context.Context, p, dst string, limit int64) (int64, error) {
	if e.local() {
		return copyLocalFile(p, dst, 0o600, limit)
	}
	c, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	info, err := e.conn.Files().Stat(c, &pb.FileStatRequest{Path: p, Hash: true})
	if err != nil {
		return 0, err
	}
	if !info.Exists || info.IsDir {
		return 0, fmt.Errorf("%s:%s: %w", e.name, p, fs.ErrNotExist)
	}
	if info.Size < 0 || info.Size > limit {
		return 0, errBudget(p, limit)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := func() (int64, error) {
		defer out.Close()
		st, err := e.conn.Files().Read(c, &pb.FileReadRequest{Path: p})
		if err != nil {
			return 0, err
		}
		h := sha256.New()
		var n int64
		for {
			ch, err := st.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return n, err
			}
			if ch.Offset != n {
				return n, fmt.Errorf("%s:%s: unexpected offset %d (want %d)", e.name, p, ch.Offset, n)
			}
			if n+int64(len(ch.Data)) > info.Size {
				return n, errBudget(p, info.Size)
			}
			out.Write(ch.Data)
			h.Write(ch.Data)
			n += int64(len(ch.Data))
		}
		if n != info.Size || !bytes.Equal(h.Sum(nil), info.Sha256) {
			return n, fmt.Errorf("%s:%s changed while it was copied (size or sha256); retry", e.name, p)
		}
		return n, out.Sync()
	}()
	if err != nil {
		os.Remove(dst)
		return 0, err
	}
	return n, nil
}

// upload copies the local file src to p on the endpoint (parents created;
// the mode of src, 0600 for the copies handoff makes).
func (e *hoEndpoint) upload(ctx context.Context, src, p string) error {
	if e.local() {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		_, err := copyLocalFile(src, p, 0o600, math.MaxInt64)
		return err
	}
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	return putFile(ctx, e.conn, src, filepath.ToSlash(p), fi, &transferStats{})
}

// writeFile writes a small file (the hand-off marker).
func (e *hoEndpoint) writeFile(ctx context.Context, p string, data []byte) error {
	if e.local() {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		return os.WriteFile(p, data, 0o600)
	}
	tmp, err := os.MkdirTemp("", "longarm-handoff-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	f := filepath.Join(tmp, "f")
	if err := os.WriteFile(f, data, 0o600); err != nil {
		return err
	}
	return e.upload(ctx, f, p)
}

// rename moves a file or directory (same volume) - the backup of state a
// handoff would otherwise overwrite.
func (e *hoEndpoint) rename(ctx context.Context, from, to string) error {
	if e.local() {
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			return err
		}
		return os.Rename(from, to)
	}
	if _, err := e.conn.Files().Mkdir(ctx, &pb.MkdirRequest{Path: e.dir(to), Mode: 0o700}); err != nil {
		return err
	}
	if e.windows() {
		// never through cmd.exe: names come from the source (& | ^ ...);
		// PowerShell takes them from the environment, literally
		_, err := e.run(ctx, "", map[string]string{"LONGARM_HO_FROM": from, "LONGARM_HO_TO": to}, nil, time.Minute,
			winPS("Move-Item -LiteralPath $env:LONGARM_HO_FROM -Destination $env:LONGARM_HO_TO -Force")...)
		return err
	}
	_, err := e.run(ctx, "", nil, nil, time.Minute, "mv", "--", from, to)
	return err
}

// winPS is a PowerShell command line that runs script (arguments come in
// through the environment, never spliced into the script).
func winPS(script string) []string {
	return []string{"powershell", "-NoProfile", "-NonInteractive", "-Command", "$ErrorActionPreference='Stop'; " + script}
}

// removeFile deletes a file longarm created on the endpoint (a temporary
// bundle or index).
func (e *hoEndpoint) removeFile(ctx context.Context, p string) error {
	if e.local() {
		err := os.Remove(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if e.windows() {
		_, err := e.run(ctx, "", map[string]string{"LONGARM_HO_PATH": p}, nil, time.Minute,
			winPS("if (Test-Path -LiteralPath $env:LONGARM_HO_PATH) { Remove-Item -LiteralPath $env:LONGARM_HO_PATH -Force }")...)
		return err
	}
	_, err := e.run(ctx, "", nil, nil, time.Minute, "rm", "-f", "--", p)
	return err
}

// runError is a command that ran and failed.
type runError struct {
	argv   []string
	code   int
	stderr string
}

func (r *runError) Error() string {
	return fmt.Sprintf("%s: exit %d: %s", strings.Join(r.argv, " "), r.code, firstLines(r.stderr, 3))
}

func firstLines(s string, n int) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	if len(ls) > n {
		ls = ls[:n]
	}
	return strings.TrimSpace(strings.Join(ls, " / "))
}

// errNoProgram: the command's program is not installed.
var errNoProgram = errors.New("program not found")

// run runs a command on the endpoint and returns its stdout (exit 0), else
// a *runError (errNoProgram wrapped when the program does not exist).
func (e *hoEndpoint) run(ctx context.Context, dir string, env map[string]string, stdin []byte, timeout time.Duration, argv ...string) ([]byte, error) {
	if e.local() {
		if _, err := exec.LookPath(argv[0]); err != nil {
			return nil, fmt.Errorf("%s: %w", argv[0], errNoProgram)
		}
		c, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(c, argv[0], argv[1:]...)
		cmd.Dir = dir
		if len(env) > 0 {
			cmd.Env = os.Environ()
			for k, v := range env {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
		}
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.Bytes(), &runError{argv: argv, code: ee.ExitCode(), stderr: errb.String()}
		}
		return out.Bytes(), err
	}
	c, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()
	resp, err := e.conn.Exec().Run(c, &pb.ExecRequest{Argv: argv, Cwd: dir, Env: env, Stdin: stdin,
		Timeout: durationpb.New(timeout), MaxOutputBytes: 16 << 20})
	if err != nil {
		return nil, err
	}
	if resp.ExitCode != 0 {
		if resp.ExitCode == -1 || (resp.ExitCode == 127 && strings.Contains(strings.ToLower(resp.Error+string(resp.Stderr)), "not found")) ||
			strings.Contains(strings.ToLower(resp.Error), "executable file not found") {
			return nil, fmt.Errorf("%s on %s: %w", argv[0], e.name, errNoProgram)
		}
		msg := string(resp.Stderr)
		if resp.Error != "" {
			msg = resp.Error + ": " + msg
		}
		if resp.TimedOut {
			msg = "timed out: " + msg
		}
		return resp.Stdout, &runError{argv: argv, code: int(resp.ExitCode), stderr: msg}
	}
	if resp.Truncated {
		return nil, fmt.Errorf("%s on %s: output truncated", argv[0], e.name)
	}
	return resp.Stdout, nil
}

// copyLocalFile copies src to the new file dst (created exclusively:
// never through an existing file or symlink), at most limit bytes.
func copyLocalFile(src, dst string, mode os.FileMode, limit int64) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return 0, err
	}
	if limit < math.MaxInt64 {
		limit++ // one byte more tells "over the limit"
	}
	n, err := io.Copy(out, io.LimitReader(in, limit))
	if err == nil && limit < math.MaxInt64 && n == limit {
		err = errBudget(src, limit-1)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return 0, err
	}
	if fi, err := in.Stat(); err == nil {
		os.Chtimes(dst, time.Now(), fi.ModTime())
	}
	return n, nil
}
