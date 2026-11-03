//go:build windows

package toolchain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows/registry"

	"github.com/lubobill1990/longarm/internal/platform/ptypes"
	platformwindows "github.com/lubobill1990/longarm/internal/platform/windows"
)

var pathMu sync.Mutex

func probeOutput(ctx context.Context, argv ...string) ([]byte, error) {
	cmd, err := systemCommand(ctx, argv...)
	if err != nil {
		return nil, err
	}
	container, err := platformwindows.NewContainer(ptypes.ContainerOptions{})
	if err != nil {
		return nil, err
	}
	defer container.Close()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.WaitDelay = 250 * time.Millisecond
	// Suspended start assigns the shim before it can spawn a child.
	if err := container.Start(cmd); err != nil {
		return nil, err
	}
	err = cmd.Wait()
	return out.Bytes(), err
}

// Keep the daemon environment in sync with installers, not just version
// probes: its next shim/job inherits the same executable lookup.
// Preserve inherited entries (including custom installer paths); registry
// removals take effect only after a restart. Job PATH refresh must retain them.
func refreshPath(extra ...string) error {
	pathMu.Lock()
	defer pathMu.Unlock()
	paths := append([]string{os.Getenv("PATH")}, extra...)
	for _, entry := range []struct {
		key registry.Key
		sub string
	}{
		{registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`},
		{registry.CURRENT_USER, `Environment`},
	} {
		key, err := registry.OpenKey(entry.key, entry.sub, registry.QUERY_VALUE)
		if errors.Is(err, registry.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("toolchain PATH: %w", err)
		}
		value, kind, err := key.GetStringValue("Path")
		key.Close()
		if errors.Is(err, registry.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("toolchain PATH: %w", err)
		}
		if kind == registry.EXPAND_SZ {
			value, err = registry.ExpandString(value)
			if err != nil {
				return fmt.Errorf("toolchain PATH: %w", err)
			}
		}
		paths = append(paths, value)
	}
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		paths = append(paths, filepath.Join(appdata, "npm"))
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		paths = append(paths, filepath.Join(local, "Microsoft", "WinGet", "Links"))
	}
	seen := map[string]bool{}
	var dirs []string
	for _, path := range paths {
		for _, dir := range filepath.SplitList(path) {
			key := strings.ToLower(filepath.Clean(dir))
			if dir != "" && !seen[key] {
				seen[key] = true
				dirs = append(dirs, dir)
			}
		}
	}
	return os.Setenv("PATH", strings.Join(dirs, string(os.PathListSeparator)))
}

func systemCommand(ctx context.Context, argv ...string) (*exec.Cmd, error) {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, err
	}
	ext := filepath.Ext(path)
	if !strings.EqualFold(ext, ".cmd") && !strings.EqualFold(ext, ".bat") {
		return exec.CommandContext(ctx, path, argv[1:]...), nil
	}
	// npm's .cmd shims cannot be launched with CreateProcess directly.
	// All arguments are literal; reject cmd expansion and quote escapes.
	quoted := make([]string, len(argv))
	for i, arg := range append([]string{path}, argv[1:]...) {
		if strings.ContainsAny(arg, "\"%\r\n\x00") {
			return nil, fmt.Errorf("unsupported batch argument %q", arg)
		}
		quoted[i] = `"` + arg + `"`
	}
	cmd := exec.CommandContext(ctx, "cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd.exe /d /v:off /s /c "` + strings.Join(quoted, " ") + `"`}
	return cmd, nil
}
