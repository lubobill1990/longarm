package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/update"
)

// Releases from GitHub: a hub that publishes nothing itself - no CI
// pushing releases to it, no `longarm-hub release add` - offers the
// releases of its source repository instead, with no setup: the install
// scripts, the front page's installers, /releases/latest.json, the tray's
// update notice and worker self-updates all see them. The hub reads the
// release index (names, sizes, sha256 digests, the .minisig signatures)
// from the GitHub API and never stores the files: downloads over HTTP are
// redirected to GitHub; a worker's self-update (gRPC) makes the hub fetch
// the file once into its cache, checked against the digest. Workers verify
// the signatures against the key built into them, as always.
//
// [releases] github: "" = the repository of [web] repo_url when that is on
// github.com (the default: the public repository), "owner/repo", or "off".
// Only while the hub's own release index is empty: the first published
// release turns this off (a hub that publishes keeps doing so).

// EnvReleasesGitHub overrides [releases] github.
const EnvReleasesGitHub = "LONGARM_HUB_RELEASES_GITHUB"

const (
	ghAPI          = "https://api.github.com"
	ghPollEvery    = 15 * time.Minute
	ghRetryEvery   = 2 * time.Minute // after a failed poll
	ghKeepReleases = 5               // newest releases (stable and preview) offered
	ghMaxSig       = 4 << 10
	ghSigFetches   = 8 // signature downloads at once
	ghMaxIndex     = 8 << 20
)

var ghRepoRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,99})/[A-Za-z0-9._-]{1,100}$`)

// ghRepo resolves [releases] github: the owner/repo to read, "" = off.
func ghRepo(c *Config) (string, error) {
	v := strings.TrimSpace(c.Releases.GitHub)
	switch strings.ToLower(v) {
	case "off", "none", "false":
		return "", nil
	case "":
		u, err := url.Parse(c.Web.RepoURL)
		if err != nil || !strings.EqualFold(u.Host, "github.com") {
			return "", nil // not a GitHub repository: nothing to read
		}
		v = strings.Trim(u.Path, "/")
		if !ghRepoRE.MatchString(v) {
			return "", nil
		}
		return v, nil
	}
	v = strings.TrimSuffix(strings.TrimPrefix(v, "https://github.com/"), ".git")
	if !ghRepoRE.MatchString(v) {
		return "", fmt.Errorf("[releases] github %q: want owner/repo or off", c.Releases.GitHub)
	}
	return v, nil
}

// ghReleases is the release index read from GitHub.
type ghReleases struct {
	repo   string
	api    string // https://api.github.com (tests: a fake)
	client *http.Client
	log    *slog.Logger
	// changed is called after a poll changed the index (the hub drops its
	// cached latest release)
	changed func()

	mu     sync.Mutex
	rels   []store.Release
	latest string            // newest stable version
	sigs   map[string][]byte // .minisig by download URL (they never change)
	polled bool              // a poll succeeded
}

func newGHReleases(c *Config, log *slog.Logger) (*ghReleases, error) {
	repo, err := ghRepo(c)
	if err != nil || repo == "" {
		return nil, err
	}
	return &ghReleases{repo: repo, api: cmpOr(c.Releases.githubAPI, ghAPI), client: &http.Client{Timeout: 30 * time.Second},
		log: log, sigs: map[string][]byte{}}, nil
}

// snapshot is the index: the files of the offered releases and the newest
// stable version.
func (g *ghReleases) snapshot() ([]store.Release, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rels, g.latest
}

// run polls GitHub until ctx ends.
func (g *ghReleases) run(ctx context.Context, wanted func() bool) {
	for {
		wait := ghPollEvery
		if wanted() {
			if err := g.poll(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				g.log.Warn("releases from GitHub: poll failed", "repo", g.repo, "err", err)
				wait = ghRetryEvery
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

type ghRelease struct {
	Tag         string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		Digest string `json:"digest"` // "sha256:<hex>"
		URL    string `json:"browser_download_url"`
	} `json:"assets"`
}

// poll reads the newest releases. The index changes only when everything
// was read (a half-read release never shows).
func (g *ghReleases) poll(ctx context.Context) error {
	var list []ghRelease
	if err := g.getJSON(ctx, g.api+"/repos/"+g.repo+"/releases?per_page=20", &list); err != nil {
		return err
	}
	if err := g.prefetchSigs(ctx, list); err != nil {
		return err
	}
	var rels []store.Release
	latest, n := "", 0
	for _, gr := range list {
		version := strings.TrimPrefix(gr.Tag, "v")
		if gr.Draft || !versionRE.MatchString(version) || n >= ghKeepReleases {
			continue
		}
		channel := update.ChannelStable
		if gr.Prerelease {
			channel = update.ChannelPreview
		}
		sigURL := map[string]string{}
		for _, a := range gr.Assets {
			if base, ok := strings.CutSuffix(a.Name, update.SigExt); ok {
				sigURL[base] = a.URL
			}
		}
		var files []store.Release
		for _, a := range gr.Assets {
			osName, arch, kind, ok := releaseFileKind(a.Name)
			sum, okSum := strings.CutPrefix(a.Digest, "sha256:")
			if !ok || !okSum || len(sum) != 64 || !strings.HasPrefix(a.URL, "https://") && !strings.HasPrefix(g.api, "http://") {
				continue
			}
			r := store.Release{Version: version, File: a.Name, OS: osName, Arch: arch, Kind: kind, SHA256: strings.ToLower(sum),
				Size: a.Size, AddedAt: gr.PublishedAt, Channel: channel, URL: a.URL}
			if u := sigURL[a.Name]; u != "" {
				sig, err := g.sig(ctx, u)
				if err != nil {
					return fmt.Errorf("%s %s: %w", gr.Tag, a.Name+update.SigExt, err)
				}
				r.Sig = sig
			}
			files = append(files, r)
		}
		if len(files) == 0 {
			continue
		}
		n++
		rels = append(rels, files...)
		if channel == update.ChannelStable && (latest == "" || update.Compare(version, latest) > 0) {
			latest = version
		}
	}
	g.mu.Lock()
	changed := !g.polled || len(rels) != len(g.rels) || latest != g.latest
	g.rels, g.latest, g.polled = rels, latest, true
	g.mu.Unlock()
	if changed && g.changed != nil {
		g.changed()
	}
	if changed {
		g.log.Info("releases from GitHub", "repo", g.repo, "latest", latest, "files", len(rels), "releases", n)
	}
	return nil
}

func (g *ghReleases) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "longarm-hub")
	if tok := os.Getenv("LONGARM_HUB_GITHUB_TOKEN"); tok != "" { // optional: a higher API rate limit
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API: %s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, ghMaxIndex)).Decode(v)
}

// prefetchSigs downloads the signatures of the releases a poll offers,
// ghSigFetches at a time (a release has some 40; the first poll would
// take minutes one by one).
func (g *ghReleases) prefetchSigs(ctx context.Context, list []ghRelease) error {
	var urls []string
	n := 0
	for _, gr := range list {
		if gr.Draft || !versionRE.MatchString(strings.TrimPrefix(gr.Tag, "v")) || n >= ghKeepReleases {
			continue
		}
		n++
		for _, a := range gr.Assets {
			if base, ok := strings.CutSuffix(a.Name, update.SigExt); ok {
				if _, _, _, ok := releaseFileKind(base); ok {
					urls = append(urls, a.URL)
				}
			}
		}
	}
	sem := make(chan struct{}, ghSigFetches)
	errs := make(chan error, len(urls))
	var wg sync.WaitGroup
	for _, u := range urls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if _, err := g.sig(ctx, u); err != nil {
				errs <- fmt.Errorf("%s: %w", u, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	return <-errs // nil when none failed
}

// sig downloads a .minisig (once per URL: release assets don't change).
func (g *ghReleases) sig(ctx context.Context, u string) ([]byte, error) {
	g.mu.Lock()
	s, ok := g.sigs[u]
	g.mu.Unlock()
	if ok {
		return s, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "longarm-hub")
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, ghMaxSig+1))
	if err != nil {
		return nil, err
	}
	if len(b) > ghMaxSig {
		return nil, errors.New("signature too large")
	}
	g.mu.Lock()
	g.sigs[u] = b
	g.mu.Unlock()
	return b, nil
}

// releases is the release index the hub offers: its own, or - while it has
// none - GitHub's ([releases] github).
func (h *Hub) releases() ([]store.Release, error) {
	rels, err := h.store.Releases()
	if err != nil || len(rels) > 0 || h.gh == nil {
		return rels, err
	}
	gh, _ := h.gh.snapshot()
	return gh, nil
}

// latestStable is latest_release, or GitHub's newest stable version while
// the hub has no releases of its own.
func (h *Hub) latestStable() (string, error) {
	latest, err := h.store.Meta("latest_release")
	if err != nil || latest != "" || h.gh == nil {
		return latest, err
	}
	if rels, err := h.store.Releases(); err != nil || len(rels) > 0 {
		return "", err
	}
	_, latest = h.gh.snapshot()
	return latest, nil
}

// ghWanted: the hub has no releases of its own (GitHub's are offered).
func (h *Hub) ghWanted() bool {
	rels, err := h.store.Releases()
	return err == nil && len(rels) == 0
}

// remoteReleaseURL is where an HTTP download of version's file goes
// instead of the hub (a GitHub release file the hub has no copy of); ""
// = served by the hub.
func (h *Hub) remoteReleaseURL(version, name string) string {
	if h.gh == nil || !versionRE.MatchString(version) || !servableFile(name) || strings.HasSuffix(name, update.SigExt) {
		return ""
	}
	files, err := h.releaseFiles(version)
	if err != nil {
		return ""
	}
	for _, r := range files {
		if r.File == name && r.URL != "" && !regularFile(h.rel.localPath(version, name)) {
			return r.URL
		}
	}
	return ""
}
