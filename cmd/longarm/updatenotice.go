package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/lubobill1990/longarm/internal/update"
	"github.com/lubobill1990/longarm/internal/version"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Update notices: when the hub has a newer release than this longarm, the
// CLI says so on stderr - once a day, only on a terminal, never with --json
// or in long-lived / program-facing commands (daemon, mcp, proxy).

// updateNoticeFile (in the config dir) remembers the last notice.
const updateNoticeFile = "update-notice.json"

// updateNoticeEvery: at most one notice per this interval.
const updateNoticeEvery = 24 * time.Hour

// noticeOK: this run may print an update notice (set before the command runs).
var noticeOK bool

// noNoticeCommands never print one: their stderr belongs to a program, or
// they run for long.
var noNoticeCommands = map[string]bool{"daemon": true, "mcp": true, "proxy": true, "completion": true,
	"__complete": true, "__completeNoDesc": true, "version": true, "help": true}

// allowUpdateNotice decides noticeOK for cmd.
func allowUpdateNotice(cmd *cobra.Command) bool {
	if os.Getenv("LONGARM_NO_UPDATE_NOTICE") == "1" || !term.IsTerminal(int(os.Stderr.Fd())) {
		return false
	}
	for c := cmd; c != nil; c = c.Parent() {
		if noNoticeCommands[c.Name()] {
			return false
		}
	}
	if f := cmd.Flags().Lookup("json"); f != nil && f.Value.String() == "true" {
		return false
	}
	return true
}

type noticeState struct {
	Version string    `json:"version"`
	At      time.Time `json:"at"`
}

// updateNoticeText is the notice for who ("" = none): the hub has a newer
// release than running.
func updateNoticeText(who *pb.WhoamiResponse, running, hubAddr string) string {
	latest := who.GetLatestVersion()
	if latest == "" || !releaseVersion(running) || !update.ValidVersion(latest) || update.Compare(latest, running) <= 0 {
		return ""
	}
	base := strings.TrimRight(who.GetPublicUrl(), "/")
	if base == "" {
		host, _, err := net.SplitHostPort(hubAddr)
		if err != nil {
			host = hubAddr
		}
		if host == "" {
			return ""
		}
		base = "https://" + host
	}
	path := who.GetLatestPath()
	if path == "" {
		path = "/install.sh"
	}
	msg := fmt.Sprintf("Longarm %s is available: %s%s", latest, base, path)
	if strings.HasSuffix(path, ".msi") {
		msg += fmt.Sprintf(" (or %s/install.ps1)", base)
	}
	return msg + " (you have " + running + ")"
}

func releaseVersion(v string) bool {
	v = strings.TrimPrefix(v, "v")
	return v != "" && v[0] >= '0' && v[0] <= '9' && strings.Contains(v, ".")
}

// versionForNotice is the running version (tests replace it).
var versionForNotice = func() string { return version.Version }

// maybeUpdateNotice prints the notice to w at most once per
// updateNoticeEvery (state in dir).
func maybeUpdateNotice(w io.Writer, dir string, who *pb.WhoamiResponse, hubAddr string, now time.Time) {
	msg := updateNoticeText(who, versionForNotice(), hubAddr)
	if msg == "" {
		return
	}
	p := filepath.Join(dir, updateNoticeFile)
	var st noticeState
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if st.Version == who.GetLatestVersion() && now.Sub(st.At) < updateNoticeEvery && now.After(st.At) {
		return
	}
	b, _ := json.Marshal(noticeState{Version: who.GetLatestVersion(), At: now})
	if os.WriteFile(p, b, 0o600) != nil {
		return // no state: no notice (it would come on every run)
	}
	fmt.Fprintln(w, "longarm:", msg)
}
