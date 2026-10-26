package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lubobill1990/longarm/internal/longarmd"
	"github.com/lubobill1990/longarm/internal/loopguard"
	"github.com/lubobill1990/longarm/internal/tenancy"
)

// perUserLoopbackCleanup returns what `longarmd service uninstall` does
// after removing service name (nil = nothing): when the service is a
// per-user longarmd - its systemd unit runs `longarmd run --data-dir D` as
// root and D's longarmd.toml says [isolation] mode = "per-user" - remove
// its loopback isolation table (inet longarm; Linux), which would otherwise
// stay behind. Other services (shared mode, another account) leave it
// alone: it may belong to a per-user longarmd running next to them.
func perUserLoopbackCleanup(name string) func() {
	if !loopguard.Supported || os.Geteuid() != 0 {
		return nil
	}
	b, err := os.ReadFile(filepath.Join("/etc/systemd/system", name+".service"))
	if err != nil {
		return nil
	}
	args, user := unitExecStart(string(b))
	if user != "" && user != "root" {
		return nil
	}
	dataDir := flagValue(args, "--data-dir")
	if dataDir == "" {
		return nil
	}
	cfg, err := longarmd.LoadConfig(dataDir, nil)
	if err != nil || cfg.Isolation.Mode != tenancy.PerUser {
		return nil
	}
	return func() {
		if err := loopguard.Remove(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: remove the loopback isolation rules (nftables table inet %s): %v\n", loopguard.Table, err)
			return
		}
		fmt.Printf("removed the loopback isolation rules (nftables table inet %s)\n", loopguard.Table)
	}
}

// unitExecStart returns the ExecStart command line (split as systemd does
// for what platform's UnitFile writes: bare words and "quoted" ones with
// \\ \" escapes; $$ and %% unescaped) and the User= of a systemd unit.
func unitExecStart(unit string) (args []string, user string) {
	for _, l := range strings.Split(unit, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "ExecStart="):
			args = splitExec(strings.TrimPrefix(l, "ExecStart="))
		case strings.HasPrefix(l, "User="):
			user = strings.TrimSpace(strings.TrimPrefix(l, "User="))
		}
	}
	return args, user
}

func splitExec(s string) []string {
	var out []string
	var cur strings.Builder
	in, quoted := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quoted && c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
		case c == '"':
			quoted = !quoted
			in = true
		case !quoted && (c == ' ' || c == '\t'):
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		case (c == '$' || c == '%') && i+1 < len(s) && s[i+1] == c:
			i++
			cur.WriteByte(c)
			in = true
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

// flagValue is the value of --name v / --name=v in args ("" = none).
func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
	}
	return ""
}
