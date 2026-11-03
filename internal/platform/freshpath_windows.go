//go:build windows

package platform

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"golang.org/x/sys/windows/registry"
)

// FreshPath: this platform rebuilds PATH before a job starts (RefreshPath).
const FreshPath = true

var (
	freshMu      sync.Mutex
	startNoted   bool
	startPrefix  []string
	registryPath = RegistryPath // tests replace it
	freshWarned  string         // the registry error last logged
)

// NoteStartPath remembers what longarmd's PATH has in front of the
// registry's entries (StartPrefix), for RefreshPath to keep in front.
// longarmd calls it as it starts; else the first RefreshPath does.
func NoteStartPath() {
	freshMu.Lock()
	defer freshMu.Unlock()
	noteStartPath()
}

func noteStartPath() {
	if startNoted {
		return
	}
	startNoted = true
	if reg, err := registryPath(); err == nil {
		startPrefix = StartPrefix(os.Getenv("PATH"), reg, ";")
	}
}

// RefreshPath sets this process's PATH to MergePath of it and the
// registry's (RegistryPath): what a process started now from Explorer
// would get, after what was put in front of longarmd's PATH when it
// started and followed by the rest of longarmd's PATH. Processes started
// afterwards (shims, exec, ssh sessions) inherit it.
func RefreshPath() {
	if FreshPathOff() {
		return
	}
	freshMu.Lock()
	defer freshMu.Unlock()
	noteStartPath()
	reg, err := registryPath()
	if msg := fmt.Sprint(err); err != nil && msg != freshWarned {
		// once per error; what could be read is still used
		freshWarned = msg
		slog.Warn("fresh-path: can't read the registry's Path; jobs may miss tools installed since longarmd started", "err", err)
	}
	if len(reg) == 0 {
		return
	}
	cur := os.Getenv("PATH")
	if p := MergePath(cur, startPrefix, reg, ";"); p != cur {
		os.Setenv("PATH", p)
	}
}

// RegistryPath is the Machine and then the User Path of the registry
// (HKLM\...\Session Manager\Environment, HKCU\Environment), REG_EXPAND_SZ
// expanded with the process's variables overlaid with the registry's own
// (a variable an installer added since, like %PNPM_HOME%). With an error
// (a key it could not read; a missing user environment is not one), what
// it could read.
func RegistryPath() ([]string, error) {
	vars := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			vars[strings.ToUpper(k)] = v
		}
	}
	lookup := func(name string) (string, bool) {
		v, ok := vars[strings.ToUpper(name)]
		return v, ok
	}
	var out []string
	var errs []error
	for i, at := range []struct {
		root registry.Key
		path string
	}{
		{registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`},
		{registry.CURRENT_USER, `Environment`},
	} {
		k, err := registry.OpenKey(at.root, at.path, registry.QUERY_VALUE)
		if err != nil {
			if i == 0 {
				return nil, fmt.Errorf("the machine's environment (HKLM\\%s): %w", at.path, err)
			}
			if !errors.Is(err, registry.ErrNotExist) { // else no user environment (a service account without a profile)
				errs = append(errs, fmt.Errorf("the user's environment (HKCU\\%s): %w", at.path, err))
			}
			continue
		}
		names, err := k.ReadValueNames(0)
		if err != nil {
			errs = append(errs, fmt.Errorf("listing %s: %w", at.path, err))
		}
		var path string
		var pathExpand bool
		for _, n := range names {
			v, typ, err := k.GetStringValue(n)
			if err != nil {
				continue // not a string
			}
			if strings.EqualFold(n, "Path") {
				path, pathExpand = v, typ == registry.EXPAND_SZ
				continue
			}
			if typ == registry.EXPAND_SZ {
				v = ExpandWindowsEnv(v, lookup)
			}
			vars[strings.ToUpper(n)] = v
		}
		k.Close()
		if pathExpand {
			path = ExpandWindowsEnv(path, lookup)
		}
		for _, p := range strings.Split(path, ";") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out, errors.Join(errs...)
}
