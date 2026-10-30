package hub

import (
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/update"
)

// Update notices: nodes that can't update themselves (MSI installs, clients)
// learn about the newest release of their channel whose files the hub has
// (stable unless they chose another, update.Channels). Workers get it with
// CheckUpdate, clients with Whoami (stable), the tray and the panel from
// GET /releases/latest.json[?channel=preview|dev].

// latestTTL: how long the newest release is cached (Whoami runs on every
// hub session of the CLI).
const latestTTL = time.Minute

type latestCache struct {
	mu   sync.Mutex
	byCh map[string]*latestEntry // by normalized channel
}

type latestEntry struct {
	at      time.Time
	version string
	files   map[string]store.Release
}

// latestNotice is the newest stable release (latestRelease), cached for
// latestTTL.
func (h *Hub) latestNotice() (string, map[string]store.Release) {
	return h.latestNoticeIn(update.ChannelStable)
}

// latestNoticeIn is latestReleaseIn(channel), cached for latestTTL.
func (h *Hub) latestNoticeIn(channel string) (string, map[string]store.Release) {
	channel = normChannel(channel)
	c := &h.latest
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byCh == nil {
		c.byCh = map[string]*latestEntry{}
	}
	e := c.byCh[channel]
	if e == nil {
		if len(c.byCh) >= 16 { // custom channels: keep the cache small
			clear(c.byCh)
		}
		e = &latestEntry{}
		c.byCh[channel] = e
	}
	if !e.at.IsZero() && time.Since(e.at) < latestTTL {
		return e.version, e.files
	}
	v, files, err := h.latestReleaseIn(channel)
	if err != nil {
		h.log.Warn("latest release", "channel", channel, "err", err)
		return e.version, e.files // the last good answer
	}
	e.version, e.files, e.at = v, files, time.Now()
	return v, files
}

// normChannel is update.NormalizeChannel for known channels; other names
// stay as they are.
func normChannel(c string) string {
	n, _ := update.NormalizeChannel(c)
	return n
}

// latestReleaseIn is the newest complete release a node on channel takes
// (update.ChannelIncludes): latestRelease for stable, else the newest of
// that and the complete versions of the channel and the more tested ones.
func (h *Hub) latestReleaseIn(channel string) (string, map[string]store.Release, error) {
	stableV, stableFiles, err := h.latestRelease()
	if err != nil || normChannel(channel) == update.ChannelStable {
		return stableV, stableFiles, err
	}
	rels, err := h.store.Releases()
	if err != nil {
		return "", nil, err
	}
	byVersion := map[string][]store.Release{}
	for _, r := range rels {
		byVersion[r.Version] = append(byVersion[r.Version], r)
	}
	best, bestFiles := stableV, stableFiles
	for v, files := range byVersion {
		if normChannel(files[0].Channel) == update.ChannelStable || !update.ChannelIncludes(channel, files[0].Channel) {
			continue // stable versions: latestRelease decided
		}
		if best != "" && update.Compare(v, best) <= 0 {
			continue
		}
		if len(h.rel.missingFiles(files)) > 0 {
			continue
		}
		best, bestFiles = v, map[string]store.Release{}
		for _, r := range files {
			bestFiles[r.File] = r
		}
	}
	return best, bestFiles, nil
}

// latestPath is where a client on goos/goarch gets version: its MSI when the
// release has one, else the install script.
func latestPath(version string, files map[string]store.Release, goos, goarch string) string {
	if version == "" {
		return ""
	}
	if goos == "windows" {
		if goarch == "" {
			goarch = "amd64"
		}
		if msi := "longarm-windows-" + goarch + ".msi"; files[msi].File != "" {
			return "/releases/" + version + "/" + msi
		}
		return "/install.ps1"
	}
	return "/install.sh"
}

// latestFile is a release file in GET /releases/latest.json.
type latestFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	OS     string `json:"os,omitempty"`
	Arch   string `json:"arch,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Signed bool   `json:"signed"`
}

// latestJSON serves GET /releases/latest.json[?channel=C] (public, like the
// install scripts): {"version", "channel", "files": [...]} of the newest
// release a machine on channel C takes (default stable); version "" = no
// release. The tray checks a downloaded MSI against this sha256 and
// against the .minisig.
func (h *Hub) latestJSON(w http.ResponseWriter, r *http.Request) {
	ch := r.URL.Query().Get("channel")
	if ch != "" && !channelRE.MatchString(ch) {
		httpError(w, http.StatusBadRequest, "invalid channel")
		return
	}
	v, files := h.latestNoticeIn(ch)
	out := struct {
		Version string       `json:"version"`
		Channel string       `json:"channel,omitempty"`
		Files   []latestFile `json:"files"`
	}{Version: v, Files: []latestFile{}}
	for _, r := range files {
		out.Channel = normChannel(r.Channel)
		out.Files = append(out.Files, latestFile{Name: r.File, SHA256: r.SHA256, Size: r.Size, OS: r.OS, Arch: r.Arch, Kind: r.Kind, Signed: len(r.Sig) > 0})
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Name < out.Files[j].Name })
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, out)
}
