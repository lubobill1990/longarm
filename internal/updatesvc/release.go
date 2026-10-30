package updatesvc

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/update"
)

// ReleaseFile is a file of the hub's release record.
type ReleaseFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Signed bool   `json:"signed,omitempty"`
	OS     string `json:"os,omitempty"`
	Arch   string `json:"arch,omitempty"`
}

// Release is the hub's record of a release (GET /releases/latest.json,
// GET /releases/<version>/release.json).
type Release struct {
	Version string        `json:"version"`
	Channel string        `json:"channel,omitempty"`
	Files   []ReleaseFile `json:"files"`
}

// File returns the record of name (nil when the release has none).
func (r *Release) File(name string) *ReleaseFile {
	for i := range r.Files {
		if r.Files[i].Name == name {
			return &r.Files[i]
		}
	}
	return nil
}

// ErrNoRelease: the hub has no such release (or none yet).
var ErrNoRelease = errors.New("no such release on the hub")

// Source is where releases come from: the recorded hub.
type Source interface {
	// Latest is the hub's newest release for a machine on channel
	// (update.Channels) whose files it has.
	Latest(ctx context.Context, channel string) (*Release, error)
	// Get is version's record.
	Get(ctx context.Context, version string) (*Release, error)
	// Download writes version's file name (or its .minisig) to w, at
	// most limit bytes.
	Download(ctx context.Context, version, name string, w io.Writer, limit int64) error
}

// HubBaseURL is the hub's HTTPS base URL for a hub as the installer
// recorded it - "longarm.example.com", "host:port" (gRPC; the panel is on
// 443), "https://host[:port]" - the same reading as hubclient.ResolveHub
// (a test checks). Plain http is refused.
func HubBaseURL(hub string) (string, error) {
	s := strings.TrimSpace(hub)
	if s == "" {
		return "", errors.New("no hub recorded (HKLM\\Software\\Longarm Hub)")
	}
	lower := strings.ToLower(s)
	switch {
	case strings.HasPrefix(lower, "https://"):
		u, err := url.Parse(s)
		if err != nil || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
			return "", fmt.Errorf("hub %q: not a base URL", hub)
		}
		return "https://" + u.Host, nil
	case strings.HasPrefix(lower, "http://"):
		return "", fmt.Errorf("hub %q: plain http can't be used for updates", hub)
	case strings.HasPrefix(lower, "grpcs://"):
		s = s[len("grpcs://"):]
	}
	s = strings.TrimSuffix(s, "/")
	host := s
	if h, p, err := net.SplitHostPort(s); err == nil {
		if n, perr := strconv.Atoi(p); perr != nil || n <= 0 || n > 65535 {
			return "", fmt.Errorf("hub %q: bad port", hub)
		}
		host = h
	} else {
		host = strings.Trim(s, "[]")
	}
	if host == "" || strings.ContainsAny(host, "/?#@ \\") {
		return "", fmt.Errorf("hub %q: not a host name", hub)
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "https://" + host, nil
}

// HTTPSource reads the hub's public release endpoints over HTTPS (system
// roots). The integrity of a package comes from its signature, checked
// against the key built into the service; the record's sha256 is checked
// too.
type HTTPSource struct {
	Base   string // HubBaseURL
	Client *http.Client
}

// NewHTTPSource for the recorded hub.
func NewHTTPSource(hub string) (*HTTPSource, error) {
	base, err := HubBaseURL(hub)
	if err != nil {
		return nil, err
	}
	return &HTTPSource{Base: base, Client: httpClient(base)}, nil
}

// httpClient: TLS 1.2+, HTTPS redirects on the same host only.
func httpClient(base string) *http.Client {
	u, _ := url.Parse(base)
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Transport: tr, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if r.URL.Scheme != "https" || u == nil || !strings.EqualFold(r.URL.Hostname(), u.Hostname()) {
			return fmt.Errorf("refusing a redirect to %s", r.URL.Redacted())
		}
		return nil
	}}
}

func (s *HTTPSource) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "longarm-update")
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		resp.Body.Close()
		return nil, ErrNoRelease
	case resp.StatusCode != http.StatusOK:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

func (s *HTTPSource) record(ctx context.Context, path string) (*Release, error) {
	resp, err := s.get(ctx, path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	if r.Version == "" {
		return nil, ErrNoRelease
	}
	if !update.ValidVersion(r.Version) {
		return nil, fmt.Errorf("GET %s: invalid version %q", path, r.Version)
	}
	return &r, nil
}

func (s *HTTPSource) Latest(ctx context.Context, channel string) (*Release, error) {
	if ch, _ := update.NormalizeChannel(channel); ch != update.ChannelStable && update.ChannelRE.MatchString(ch) {
		// an older hub ignores the parameter and answers its latest stable
		return s.record(ctx, "/releases/latest.json?channel="+url.QueryEscape(ch))
	}
	return s.record(ctx, "/releases/latest.json")
}

func (s *HTTPSource) Get(ctx context.Context, version string) (*Release, error) {
	if !update.ValidVersion(version) {
		return nil, fmt.Errorf("invalid version %q", version)
	}
	r, err := s.record(ctx, "/releases/"+url.PathEscape(version)+"/release.json")
	if err == nil && r.Version != version {
		return nil, fmt.Errorf("the hub answered %s for %s", r.Version, version)
	}
	return r, err
}

func (s *HTTPSource) Download(ctx context.Context, version, name string, w io.Writer, limit int64) error {
	if !update.ValidVersion(version) || !update.IsReleaseFile(strings.TrimSuffix(name, update.SigExt)) {
		return fmt.Errorf("invalid release file %s/%s", version, name)
	}
	resp, err := s.get(ctx, "/releases/"+url.PathEscape(version)+"/"+url.PathEscape(name))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("%s: larger than %d bytes", name, limit)
	}
	return nil
}
