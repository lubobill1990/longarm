package gitauth

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The test binary doubles as fake az, gh and git (copied under those
// names into directories put on PATH): no real tool, account or network.
// FAKE_LOG receives each call's arguments and stdin; FAKE_<TOOL>_OUT is
// what it prints, FAKE_<TOOL>_FAIL makes it fail.
func TestMain(m *testing.M) {
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe")
	switch name {
	case "az", "gh", "git":
		os.Exit(fake(name))
	}
	code := m.Run()
	for _, d := range fakeDirs {
		os.RemoveAll(d)
	}
	os.Exit(code)
}

func fake(name string) int {
	in, _ := io.ReadAll(os.Stdin)
	if p := os.Getenv("FAKE_LOG"); p != "" {
		f, _ := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		fmt.Fprintf(f, "%s %s\n%s--\n", name, strings.Join(os.Args[1:], " "), in)
		f.Close()
	}
	key := "FAKE_" + strings.ToUpper(name)
	if msg := os.Getenv(key + "_FAIL"); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
		return 1
	}
	fmt.Print(os.Getenv(key + "_OUT"))
	return 0
}

var fakeDirs = map[string]string{}

// fakes puts the named fake tools (and nothing else) on PATH.
func fakes(t *testing.T, names ...string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var path []string
	for _, n := range names {
		d := fakeDirs[n]
		if d == "" {
			d = filepath.Join(os.TempDir(), fmt.Sprintf("gitauth-fake-%d-%s", os.Getpid(), n))
			os.MkdirAll(d, 0o755)
			exe := n
			if runtime.GOOS == "windows" {
				exe += ".exe"
			}
			b, err := os.ReadFile(self)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(d, exe), b, 0o755); err != nil {
				t.Fatal(err)
			}
			fakeDirs[n] = d
		}
		path = append(path, d)
	}
	if len(path) == 0 {
		path = []string{t.TempDir()}
	}
	t.Setenv("PATH", strings.Join(path, string(os.PathListSeparator)))
	log := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("FAKE_LOG", log)
	for _, k := range []string{"FAKE_AZ_OUT", "FAKE_AZ_FAIL", "FAKE_GH_OUT", "FAKE_GH_FAIL", "FAKE_GIT_OUT", "FAKE_GIT_FAIL"} {
		t.Setenv(k, "")
	}
	return log
}

func readLog(t *testing.T, p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

const tenant = "72f988bf-86f1-41af-91ab-2d7cd011db47"

// adoServer answers like an Azure DevOps organization URL to an
// anonymous request: 401 (or a redirect) naming the tenant.
func adoServer(t *testing.T, header string, status int) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("tenant discovery sent credentials")
		}
		if header != "" {
			w.Header().Set(TenantHeader, header)
		}
		if status == http.StatusFound {
			w.Header().Set("Location", "https://login.example/")
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestDiscoverTenant(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		header string
		status int
		want   string
	}{
		{strings.ToUpper(tenant), 401, tenant},
		{tenant, 302, tenant}, // not followed
		{"", 401, ""},
		{"00000000-0000-0000-0000-000000000000", 401, ""},
		{"not-a-guid", 401, ""},
		{"x, " + tenant, 200, tenant},
	} {
		s := adoServer(t, c.header, c.status)
		got, err := DiscoverTenant(ctx, nil, s.URL+"/org")
		if err != nil || got != c.want {
			t.Errorf("%q/%d: %q %v, want %q", c.header, c.status, got, err, c.want)
		}
	}
	if _, err := DiscoverTenant(ctx, nil, "http://127.0.0.1:1/org"); err == nil {
		t.Error("unreachable: no error")
	}
}

func TestOrgURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://dev.azure.com/myorg/proj/_git/repo":                "https://dev.azure.com/myorg",
		"https://myorg@dev.azure.com/myorg/proj/_git/r":             "https://dev.azure.com/myorg",
		"https://myorg.visualstudio.com/proj/_git/repo":             "https://myorg.visualstudio.com",
		"https://myorg.visualstudio.com/DefaultCollection/p/_git/r": "https://myorg.visualstudio.com",
	} {
		u, _ := url.Parse(in)
		if got := OrgURL(u); got != want {
			t.Errorf("OrgURL(%s) = %s, want %s", in, got, want)
		}
	}
}

func acquirer(t *testing.T, disc *httptest.Server) (*Acquirer, *bytes.Buffer) {
	var notes bytes.Buffer
	a := &Acquirer{Log: &notes}
	if disc != nil {
		a.OrgURL = func(*url.URL) string { return disc.URL + "/org" }
	}
	return a, &notes
}

const fakeToken = "FAKE-access-token-not-real-0001"

// TestAcquireADO: az in the organization's tenant (not az's default),
// bearer; without az, or when az fails, git credential fill.
func TestAcquireADO(t *testing.T) {
	ctx := context.Background()
	repo := "https://dev.azure.com/myorg/proj/_git/repo"
	disc := adoServer(t, tenant, 401)

	log := fakes(t, "az", "git")
	t.Setenv("FAKE_AZ_OUT", `{"accessToken": "`+fakeToken+`", "expiresOn": "2026-10-28 12:00:00", "tenant": "`+tenant+`", "tokenType": "Bearer"}`)
	a, notes := acquirer(t, disc)
	c, err := a.Acquire(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if c.Scheme != Bearer || string(c.Secret) != fakeToken || !strings.Contains(c.Source, tenant) {
		t.Fatalf("cred: %s %q %s", c.Scheme, c.Secret, c.Source)
	}
	calls := readLog(t, log)
	if !strings.Contains(calls, "az account get-access-token --resource "+ADOResource+" --output json --tenant "+tenant) || strings.Contains(calls, "git ") {
		t.Fatalf("calls:\n%s", calls)
	}
	if strings.Contains(notes.String(), fakeToken) {
		t.Fatal("token in notes")
	}
	c.Clear()
	if c.Secret != nil {
		t.Fatal("Clear kept the secret")
	}

	// no tenant learned: az's default
	log = fakes(t, "az")
	t.Setenv("FAKE_AZ_OUT", `{"accessToken": "`+fakeToken+`"}`)
	a, _ = acquirer(t, adoServer(t, "", 401))
	if c, err := a.Acquire(ctx, repo); err != nil || string(c.Secret) != fakeToken || strings.Contains(readLog(t, log), "--tenant") {
		t.Fatalf("no tenant: %v %s", err, readLog(t, log))
	}

	// az fails (not signed in to that tenant): git credential fill, noted
	log = fakes(t, "az", "git")
	t.Setenv("FAKE_AZ_FAIL", "ERROR: AADSTS50076: interaction required")
	t.Setenv("FAKE_GIT_OUT", "protocol=https\nhost=dev.azure.com\nusername=me\npassword="+fakeToken+"\n")
	a, notes = acquirer(t, disc)
	c, err = a.Acquire(ctx, repo)
	if err != nil || c.Scheme != Basic || c.Username != "me" || string(c.Secret) != fakeToken || c.Source != "git credential fill" {
		t.Fatalf("fallback: %+v %v", c, err)
	}
	if !strings.Contains(notes.String(), "az login --tenant "+tenant) || !strings.Contains(notes.String(), "AADSTS50076") {
		t.Fatalf("notes: %s", notes.String())
	}
	if calls := readLog(t, log); !strings.Contains(calls, "git credential fill\nprotocol=https\nhost=dev.azure.com\npath=myorg/proj/_git/repo\n") {
		t.Fatalf("calls:\n%s", calls)
	}

	// no az at all
	fakes(t, "git")
	t.Setenv("FAKE_GIT_OUT", "username=me\npassword="+fakeToken+"\n")
	a, _ = acquirer(t, disc)
	if c, err := a.Acquire(ctx, "https://myorg.visualstudio.com/proj/_git/repo"); err != nil || string(c.Secret) != fakeToken {
		t.Fatalf("no az: %+v %v", c, err)
	}
}

// TestAcquireGitHub: gh auth token (basic, x-access-token), else git
// credential fill.
func TestAcquireGitHub(t *testing.T) {
	ctx := context.Background()
	log := fakes(t, "gh", "git")
	t.Setenv("FAKE_GH_OUT", fakeToken+"\n")
	a, _ := acquirer(t, nil)
	c, err := a.Acquire(ctx, "https://github.com/o/r.git")
	if err != nil || c.Scheme != Basic || c.Username != "x-access-token" || string(c.Secret) != fakeToken || c.Source != "gh" {
		t.Fatalf("gh: %+v %v", c, err)
	}
	if calls := readLog(t, log); !strings.Contains(calls, "gh auth token --hostname github.com") {
		t.Fatalf("calls: %s", calls)
	}
	t.Setenv("FAKE_GH_FAIL", "not logged in")
	t.Setenv("FAKE_GIT_OUT", "username=bob\npassword="+fakeToken+"\n")
	if c, err := a.Acquire(ctx, "https://github.com/o/r"); err != nil || c.Username != "bob" {
		t.Fatalf("gh failing: %+v %v", c, err)
	}
	fakes(t, "git")
	t.Setenv("FAKE_GIT_OUT", "username=bob\npassword="+fakeToken+"\n")
	if c, err := a.Acquire(ctx, "https://github.com/o/r"); err != nil || c.Source != "git credential fill" {
		t.Fatalf("no gh: %+v %v", c, err)
	}
}

// TestAcquireGeneric: other hosts use git credential fill (with the
// URL's user name); failures and refusals are errors that never carry
// the tool's stdout.
func TestAcquireGeneric(t *testing.T) {
	ctx := context.Background()
	log := fakes(t, "git", "az", "gh")
	t.Setenv("FAKE_GIT_OUT", "username=u\npassword="+fakeToken+"\n")
	a, _ := acquirer(t, nil)
	c, err := a.Acquire(ctx, "https://alice@git.example.com:8443/team/repo.git")
	if err != nil || c.Username != "u" || string(c.Secret) != fakeToken {
		t.Fatalf("%+v %v", c, err)
	}
	calls := readLog(t, log)
	if !strings.Contains(calls, "host=git.example.com:8443\npath=team/repo.git\nusername=alice\n") ||
		strings.Contains(calls, "az ") || strings.Contains(calls, "gh ") {
		t.Fatalf("calls:\n%s", calls)
	}
	t.Setenv("FAKE_GIT_OUT", "username=u\n")
	if _, err := a.Acquire(ctx, "https://git.example.com/r"); err == nil || !strings.Contains(err.Error(), "no password") {
		t.Fatalf("no password: %v", err)
	}
	t.Setenv("FAKE_GIT_FAIL", "fatal: could not read Username: terminal prompts disabled")
	if _, err := a.Acquire(ctx, "https://git.example.com/r"); err == nil || !strings.Contains(err.Error(), "terminal prompts disabled") {
		t.Fatalf("fill failing: %v", err)
	}
	for _, bad := range []string{"http://git.example.com/r", "git@github.com:o/r", "not a url"} {
		if _, err := a.Acquire(ctx, bad); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

func TestHTTPSURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/o/r.git":                   "https://github.com/o/r.git",
		"git@github.com:o/r.git":                       "https://github.com/o/r.git",
		"ssh://git@github.com/o/r":                     "https://github.com/o/r",
		"git@ssh.dev.azure.com:v3/org/proj/repo":       "https://dev.azure.com/org/proj/_git/repo",
		"ssh://git@ssh.dev.azure.com/v3/org/proj/repo": "https://dev.azure.com/org/proj/_git/repo",
		"org@vs-ssh.visualstudio.com:v3/org/proj/repo": "https://dev.azure.com/org/proj/_git/repo",
		"https://org@dev.azure.com/org/proj/_git/repo": "https://org@dev.azure.com/org/proj/_git/repo",
		"git@gitlab.example.com:o/r.git":               "",
		"http://github.com/o/r":                        "",
		"C:/src/repo":                                  "",
		"/home/me/repo":                                "",
	} {
		got, ok := HTTPSURL(in)
		if got != want || ok != (want != "") {
			t.Errorf("HTTPSURL(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
}

func TestKindOf(t *testing.T) {
	for h, want := range map[string]Kind{"dev.azure.com": ADO, "Org.VisualStudio.com": ADO, "github.com": GitHub,
		"gitlab.com": Generic, "evil-dev.azure.com.example": Generic, "notvisualstudio.com": Generic} {
		if got := KindOf(h); got != want {
			t.Errorf("KindOf(%s) = %v", h, got)
		}
	}
}
