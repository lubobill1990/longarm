package tray

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"longarm/internal/update"
	"longarm/internal/version"
)

// The Workers tab's "Add a machine" page (web/src/tray/AddMachine.tsx)
// shows the commands that install Longarm on another machine and connect
// it to this machine's hub: the installer from the GitHub Release of the
// hub's newest stable version, then a sign-in to the hub.
//
//	GET /trayapi/addmachine   {hub_host, version, tenant, repo}
//
// The installers come from the GitHub repository the hub names ([web]
// repo_url), else the one this tray was built from (version.RepoURL).

// addMachineTTL: how long the hub's answers are reused.
const addMachineTTL = 5 * time.Minute

type addMachineInfo struct {
	HubHost string `json:"hub_host"`          // the hub's domain ("" = no hub configured)
	Version string `json:"version,omitempty"` // the hub's newest stable release ("" = unknown)
	Tenant  string `json:"tenant,omitempty"`  // the hub's Entra tenant (an older hub: "")
	Repo    string `json:"repo"`              // where to download from: github.com/OWNER/REPO (gh -R takes it too; "" = unknown)
}

type addMachineCache struct {
	mu   sync.Mutex
	at   time.Time
	base string
	info addMachineInfo
}

// hubGetJSON fetches base+path from the hub into out.
func (s *Server) hubGetJSON(ctx context.Context, base, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	resp, err := s.upd.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &url.Error{Op: "GET", URL: base + path, Err: io.ErrUnexpectedEOF}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func (s *Server) addMachineInfo(ctx context.Context) addMachineInfo {
	base := strings.TrimRight(s.hubURL(), "/")
	c := &s.addMachine
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.base == base && !c.at.IsZero() && time.Since(c.at) < addMachineTTL {
		return c.info
	}
	info := addMachineInfo{Repo: githubRepo(version.RepoURL)}
	if u, err := url.Parse(base); err == nil {
		info.HubHost = u.Hostname()
	}
	if base == "" || info.HubHost == "" {
		return info
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var auth struct {
		Tenant  string `json:"tenant"`
		RepoURL string `json:"repo_url"` // [web] repo_url (an older hub: "")
	}
	if s.hubGetJSON(cctx, base, "/auth/info", &auth) == nil {
		info.Tenant = strings.ToLower(auth.Tenant)
	}
	var latest struct {
		Version string `json:"version"`
	}
	if s.hubGetJSON(cctx, base, "/releases/latest.json", &latest) == nil && update.ValidVersion(latest.Version) {
		info.Version = latest.Version // stable: the channels' builds have no GitHub Release
	}
	// the hub's repository when it names a GitHub one
	if r := githubRepo(auth.RepoURL); r != "" {
		info.Repo = r
	}
	c.base, c.at, c.info = base, time.Now(), info
	return info
}

// githubRepo is u as github.com/OWNER/REPO when it is a GitHub repository
// URL (the installers' release downloads), else "".
func githubRepo(u string) string {
	p, err := url.Parse(strings.TrimRight(u, "/"))
	if err != nil || p.Scheme != "https" || !strings.EqualFold(p.Host, "github.com") {
		return ""
	}
	if parts := strings.Split(strings.Trim(p.Path, "/"), "/"); len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return "github.com/" + parts[0] + "/" + parts[1]
	}
	return ""
}

func (s *Server) addMachineHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.addMachineInfo(r.Context()))
}
