package hub

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// [web] feedback_url: the project's issues by default, hub.toml, the
// environment winning over it, and only http(s) links.
func TestConfigFeedbackURL(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hub.toml")
	os.WriteFile(p, []byte(""), 0o600)
	c, err := LoadConfig(p)
	if err != nil || c.Web.FeedbackURL != DefaultFeedbackURL {
		t.Fatalf("default: %q %v", c.Web.FeedbackURL, err)
	}
	os.WriteFile(p, []byte("[web]\nfeedback_url = \"https://forms.example.com/longarm\"\n"), 0o600)
	if c, err = LoadConfig(p); err != nil || c.Web.FeedbackURL != "https://forms.example.com/longarm" {
		t.Fatalf("toml: %q %v", c.Web.FeedbackURL, err)
	}
	t.Setenv(EnvFeedbackURL, " https://chat.example.com/team ")
	if c, err = LoadConfig(p); err != nil || c.Web.FeedbackURL != "https://chat.example.com/team" {
		t.Fatalf("env: %q %v", c.Web.FeedbackURL, err)
	}
	t.Setenv(EnvFeedbackURL, "javascript:alert(1)")
	if _, err = LoadConfig(p); err == nil || !strings.Contains(err.Error(), "feedback_url") {
		t.Fatalf("a non-http feedback_url was accepted: %v", err)
	}
}

// The feedback link is public (/auth/info for the front page, /healthz for
// longarm-tray) and in /api/me.
func TestFeedbackURLEndpoints(t *testing.T) {
	th := newTenancyHub(t)
	base := "http://" + th.h.Addr("http")
	for _, path := range []string{"/auth/info", "/healthz"} {
		if code, body := plainGet(t, base+path); code != 200 || !strings.Contains(body, `"feedback_url": "`+DefaultFeedbackURL+`"`) {
			t.Fatalf("%s default: %d %s", path, code, body)
		}
	}
	th.h.cfg.Web.FeedbackURL = "https://chat.example.com/team"
	for _, path := range []string{"/auth/info", "/healthz"} {
		if _, body := plainGet(t, base+path); !strings.Contains(body, `"feedback_url": "https://chat.example.com/team"`) {
			t.Fatalf("%s configured: %s", path, body)
		}
	}
	if m := th.h.meInfo(httptest.NewRequest("GET", base+"/api/me", nil), "root"); m["feedback_url"] != "https://chat.example.com/team" {
		t.Fatalf("/api/me: %v", m["feedback_url"])
	}
}

// [web] repo_url: the public repository by default, a company's copy when
// set (trailing slash dropped), only http(s); in /auth/info and /api/me.
func TestRepoURL(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hub.toml")
	os.WriteFile(p, []byte(""), 0o600)
	if c, err := LoadConfig(p); err != nil || c.Web.RepoURL != DefaultRepoURL {
		t.Fatalf("default: %q %v", c.Web.RepoURL, err)
	}
	t.Setenv(EnvRepoURL, "https://git.example.com/team/longarm/")
	if c, err := LoadConfig(p); err != nil || c.Web.RepoURL != "https://git.example.com/team/longarm" ||
		c.Web.FeedbackURL != "https://git.example.com/team/longarm/issues" { // no feedback_url: that repository's issues
		t.Fatalf("env: %q %q %v", c.Web.RepoURL, c.Web.FeedbackURL, err)
	}
	t.Setenv(EnvRepoURL, "javascript:alert(1)")
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "repo_url") {
		t.Fatalf("a non-http repo_url was accepted: %v", err)
	}

	th := newTenancyHub(t)
	base := "http://" + th.h.Addr("http")
	if _, body := plainGet(t, base+"/auth/info"); !strings.Contains(body, `"repo_url": "`+DefaultRepoURL+`"`) {
		t.Fatalf("/auth/info default: %s", body)
	}
	th.h.cfg.Web.RepoURL, th.h.cfg.Web.FeedbackURL = "https://git.example.com/team/longarm", "" // a repo, no feedback_url
	for _, path := range []string{"/auth/info", "/healthz"} {
		if _, body := plainGet(t, base+path); !strings.Contains(body, `"repo_url": "https://git.example.com/team/longarm"`) ||
			!strings.Contains(body, `"feedback_url": "https://git.example.com/team/longarm/issues"`) {
			t.Fatalf("%s configured: %s", path, body)
		}
	}
	if m := th.h.meInfo(httptest.NewRequest("GET", base+"/api/me", nil), "root"); m["repo_url"] != "https://git.example.com/team/longarm" {
		t.Fatalf("/api/me: %v", m["repo_url"])
	}
}
