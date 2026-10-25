package toolchain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// New returns the backend named by longarmd.toml [toolchain] backend:
// "system" (default: winget / npm on Windows, apt + official install
// scripts / npm on Linux, Homebrew / npm on macOS) or "fake" (a JSON state
// file, for tests).
func New(name, fakeState string) (Backend, error) {
	switch name {
	case "", "system":
		return &System{}, nil
	case "fake":
		if fakeState == "" {
			return nil, errors.New("toolchain backend fake needs fake_state")
		}
		return &Fake{Path: fakeState}, nil
	}
	return nil, fmt.Errorf("unknown toolchain backend %q (system|fake)", name)
}

// ---------------------------------------------------------------- fake

// Fake keeps "installed" versions in a JSON file {"tool": "version"}. A
// tool listed in the "fail" array fails to install. "latest" installs as
// "99.0.0", a prefix "22" as "22.0.0".
type Fake struct {
	Path string
	mu   sync.Mutex
}

type fakeState struct {
	Tools map[string]string `json:"tools"`
	Fail  []string          `json:"fail,omitempty"`
	Log   []string          `json:"log,omitempty"` // installs done, in order
}

func (f *Fake) load() (*fakeState, error) {
	st := &fakeState{Tools: map[string]string{}}
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, err
	}
	if st.Tools == nil {
		st.Tools = map[string]string{}
	}
	return st, nil
}

// Version implements Backend.
func (f *Fake) Version(_ context.Context, tool string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, err := f.load()
	if err != nil {
		return "", err
	}
	return st.Tools[tool], nil
}

// Install implements Backend.
func (f *Fake) Install(_ context.Context, tool, version string, _ *slog.Logger) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, err := f.load()
	if err != nil {
		return err
	}
	for _, t := range st.Fail {
		if t == tool {
			return fmt.Errorf("fake install of %s failed", tool)
		}
	}
	v := version
	switch {
	case v == "latest" || v == "lts" || v == "":
		v = "99.0.0"
	case strings.Count(v, ".") < 2:
		v += strings.Repeat(".0", 2-strings.Count(v, "."))
	}
	st.Tools[tool] = v
	st.Log = append(st.Log, tool+"@"+v)
	b, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(f.Path, b, 0o600)
}

// ---------------------------------------------------------------- system

// System installs with the platform's package managers. Known pitfalls
// handled (from bootstrap-windows.ps1): PowerShell 7 via winget needs
// --installer-type wix; npm global packages are installed with npm.cmd
// (npm.ps1 may be blocked by the execution policy); winget runs fully
// non-interactive. On Linux, apt needs root or passwordless sudo.
type System struct {
	// MachineWide (per-user isolation: longarmd is LocalSystem / root and
	// the workloads run as other accounts) installs for every account:
	// winget --scope machine, npm global packages next to node on Windows
	// (C:\Program Files\nodejs, on the machine PATH), uv into
	// /usr/local/bin on Linux. Per-user agent settings (~/.claude, ...)
	// stay in each account's profile.
	MachineWide bool
}

var versionArgs = map[string][]string{
	"git": {"git", "--version"}, "gh": {"gh", "--version"}, "node": {"node", "--version"},
	"pnpm": {"pnpm", "--version"}, "uv": {"uv", "--version"}, "pwsh": {"pwsh", "-NoProfile", "-Command", "$PSVersionTable.PSVersion.ToString()"},
	"claude": {"claude", "--version"}, "copilot": {"copilot", "--version"}, "go": {"go", "version"},
}

// npm global packages.
var npmPackages = map[string]string{"pnpm": "pnpm", "claude": "@anthropic-ai/claude-code", "copilot": "@github/copilot"}

// winget package ids.
var wingetIDs = map[string]string{
	"git": "Git.Git", "gh": "GitHub.cli", "node": "OpenJS.NodeJS.LTS", "uv": "astral-sh.uv",
	"pwsh": "Microsoft.PowerShell", "go": "GoLang.Go",
}

// Version implements Backend.
func (s *System) Version(ctx context.Context, tool string) (string, error) {
	argv, ok := versionArgs[tool]
	if !ok {
		return "", fmt.Errorf("unknown tool %s", tool)
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return "", nil // not installed
	}
	out, err := exec.CommandContext(ctx, path, argv[1:]...).Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	return ParseVersion(string(out)), nil
}

func run(ctx context.Context, log *slog.Logger, argv ...string) error {
	log.Info("toolchain: run", "cmd", strings.Join(argv, " "))
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		tail := string(out)
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		return fmt.Errorf("%s: %v: %s", strings.Join(argv, " "), err, strings.TrimSpace(tail))
	}
	return nil
}

func exact(version string) string {
	if version == "" || version == "latest" || version == "lts" {
		return ""
	}
	return version
}

// Install implements Backend.
func (s *System) Install(ctx context.Context, tool, version string, log *slog.Logger) error {
	if !safeVersion.MatchString(version) { // versions end up in command lines
		return fmt.Errorf("refusing version %q", version)
	}
	if pkg, ok := npmPackages[tool]; ok {
		npm := "npm"
		if runtime.GOOS == "windows" {
			npm = "npm.cmd"
		}
		spec := pkg + "@latest"
		if v := exact(version); v != "" {
			spec = pkg + "@" + v
		}
		args := []string{npm, "install", "-g", spec}
		if s.MachineWide && runtime.GOOS == "windows" {
			// LocalSystem's own npm prefix is in its profile, on nobody's PATH
			if node, err := exec.LookPath("node.exe"); err == nil {
				args = append(args, "--prefix", filepath.Dir(node))
			}
		}
		return run(ctx, log, args...)
	}
	switch runtime.GOOS {
	case "windows":
		return s.winget(ctx, tool, version, log)
	case "darwin":
		formula := map[string]string{"git": "git", "gh": "gh", "node": "node@22", "uv": "uv", "pwsh": "powershell", "go": "go"}[tool]
		if formula == "" {
			return fmt.Errorf("no Homebrew formula for %s", tool)
		}
		if tool == "pwsh" {
			return run(ctx, log, "brew", "install", "--cask", formula)
		}
		return run(ctx, log, "brew", "install", formula)
	default:
		return s.linux(ctx, tool, version, log)
	}
}

func (s *System) winget(ctx context.Context, tool, version string, log *slog.Logger) error {
	id, ok := wingetIDs[tool]
	if !ok {
		return fmt.Errorf("no winget package for %s", tool)
	}
	args := []string{wingetExe(), "install", "--id", id, "--exact", "--silent", "--disable-interactivity",
		"--accept-package-agreements", "--accept-source-agreements"}
	if s.MachineWide {
		args = append(args, "--scope", "machine")
	}
	if v := exact(version); v != "" && strings.Count(v, ".") >= 2 {
		args = append(args, "--version", v, "--force")
	}
	if tool == "pwsh" {
		args = append(args, "--installer-type", "wix") // the msix variant breaks services and ssh
	}
	return run(ctx, log, args...)
}

func (s *System) linux(ctx context.Context, tool, version string, log *slog.Logger) error {
	sudo := []string{}
	if os.Geteuid() != 0 {
		sudo = []string{"sudo", "-n"}
	}
	sh := func(script string) error { return run(ctx, log, append(sudo, "sh", "-c", script)...) }
	apt := func(pkgs ...string) error {
		if err := run(ctx, log, append(sudo, "apt-get", "update", "-q")...); err != nil {
			return err
		}
		return run(ctx, log, append(append(sudo, "env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", "-q"), pkgs...)...)
	}
	switch tool {
	case "git":
		return apt("git")
	case "gh":
		return apt("gh")
	case "node":
		major := "lts"
		if v := exact(version); v != "" {
			major = strings.SplitN(v, ".", 2)[0]
		}
		return sh("curl -fsSL https://deb.nodesource.com/setup_" + major + ".x | bash - && apt-get install -y nodejs")
	case "uv":
		if s.MachineWide {
			return run(ctx, log, "sh", "-c", "curl -LsSf https://astral.sh/uv/install.sh | env UV_INSTALL_DIR=/usr/local/bin INSTALLER_NO_MODIFY_PATH=1 sh")
		}
		return run(ctx, log, "sh", "-c", "curl -LsSf https://astral.sh/uv/install.sh | sh")
	case "pwsh":
		return run(ctx, log, append(sudo, "snap", "install", "powershell", "--classic")...)
	case "go":
		v := exact(version)
		if v == "" {
			return errors.New("go needs an explicit version (e.g. 1.24.3) on Linux")
		}
		arch := runtime.GOARCH
		return sh(fmt.Sprintf("curl -fsSL https://go.dev/dl/go%s.linux-%s.tar.gz -o /tmp/go.tgz && rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && ln -sf /usr/local/go/bin/go /usr/local/bin/go", v, arch))
	}
	return fmt.Errorf("don't know how to install %s on Linux", tool)
}

var safeVersion = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}$`)

// wingetExe finds winget: LocalSystem has no App Installer alias on its
// PATH, but the package's winget.exe runs for it.
func wingetExe() string {
	if p, err := exec.LookPath("winget"); err == nil {
		return p
	}
	pf := os.Getenv("ProgramFiles")
	if pf == "" {
		pf = `C:\Program Files`
	}
	matches, _ := filepath.Glob(filepath.Join(pf, "WindowsApps", "Microsoft.DesktopAppInstaller_*_x64__8wekyb3d8bbwe", "winget.exe"))
	if len(matches) == 0 {
		matches, _ = filepath.Glob(filepath.Join(pf, "WindowsApps", "Microsoft.DesktopAppInstaller_*", "winget.exe"))
	}
	if len(matches) > 0 {
		sort.Strings(matches)
		return matches[len(matches)-1] // the newest version
	}
	return "winget"
}
