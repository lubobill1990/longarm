// Package gitauth gets, on the client, a short-lived credential for a hosted
// git remote that `longarm run / sync --git-auth forward` forwards to the
// worker for one fetch (docs/usage.md "Fetching with your credential"):
//
//   - Azure DevOps (dev.azure.com, *.visualstudio.com): an Entra access
//     token for Azure DevOps (resource 499b84ac-1321-427f-aa17-267ca6975798)
//     from `az account get-access-token`, in the organization's tenant -
//     learned from the X-VSS-ResourceTenant header of an unauthenticated
//     request to the organization's URL, not az's default tenant; without
//     az (or when az has no token for that tenant), git credential fill
//     (Git Credential Manager gives an Azure DevOps token).
//   - GitHub (github.com): `gh auth token`, else git credential fill. Unlike
//     Azure DevOps tokens (about an hour) this token is long-lived.
//   - Any other https host: git credential fill.
//
// The credential is never printed, logged or put on a command line: tools
// write it to a pipe, and it leaves here only in the worker-direct mTLS
// request.
package gitauth

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ADOResource is the Entra resource (audience) of Azure DevOps.
const ADOResource = "499b84ac-1321-427f-aa17-267ca6975798"

// TenantHeader names the organization's tenant in Azure DevOps responses.
const TenantHeader = "X-VSS-ResourceTenant"

// Schemes (jobs.GitAuthBearer / GitAuthBasic).
const (
	Bearer = "bearer"
	Basic  = "basic"
)

// Cred is a credential for one remote.
type Cred struct {
	Scheme   string // Bearer or Basic
	Username string // Basic
	Secret   []byte
	// Source says where it came from, e.g. "az (tenant <id>)", for a note
	// on stderr; never the credential.
	Source string
}

// Clear zeroes the secret.
func (c *Cred) Clear() {
	if c != nil {
		clear(c.Secret)
		c.Secret = nil
	}
}

// Kind of a remote host.
type Kind int

const (
	Generic Kind = iota
	ADO
	GitHub
)

// KindOf classifies host (no port).
func KindOf(host string) Kind {
	h := strings.ToLower(host)
	switch {
	case h == "dev.azure.com" || strings.HasSuffix(h, ".visualstudio.com"):
		return ADO
	case h == "github.com":
		return GitHub
	}
	return Generic
}

// Acquirer gets credentials. The zero value uses the real tools on PATH.
type Acquirer struct {
	// HTTP makes the tenant discovery request (default: 15 s timeout, no
	// redirects followed).
	HTTP *http.Client
	// OrgURL maps an Azure DevOps repository URL to the URL whose response
	// names the tenant (default: OrgURL).
	OrgURL func(*url.URL) string
	// Log gets one-line notes (fallbacks); nil = none. Never the credential.
	Log io.Writer
}

func (a *Acquirer) logf(format string, args ...any) {
	if a.Log != nil {
		fmt.Fprintf(a.Log, "longarm: "+format+"\n", args...)
	}
}

// Acquire gets a credential for the https remote rawURL.
func (a *Acquirer) Acquire(ctx context.Context, rawURL string) (*Cred, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("--git-auth forward: %q is not a url", redactURL(rawURL))
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("--git-auth forward: only https remotes, not %s", redactURL(rawURL))
	}
	switch KindOf(u.Hostname()) {
	case ADO:
		return a.ado(ctx, u)
	case GitHub:
		if _, err := exec.LookPath("gh"); err == nil {
			c, err := ghToken(ctx, u.Hostname())
			if err == nil {
				return c, nil
			}
			a.logf("gh auth token: %v; trying git credential fill", err)
		}
	}
	return credentialFill(ctx, u)
}

// OrgURL is the organization URL of an Azure DevOps repository URL:
// https://dev.azure.com/<org> or https://<org>.visualstudio.com.
func OrgURL(u *url.URL) string {
	if strings.EqualFold(u.Hostname(), "dev.azure.com") {
		org, _, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
		return "https://dev.azure.com/" + url.PathEscape(org)
	}
	return "https://" + u.Host
}

var guidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// DiscoverTenant asks orgURL, unauthenticated, which Entra tenant the
// organization belongs to (the X-VSS-ResourceTenant response header; any
// status). "" when the response names none (e.g. an organization backed
// by Microsoft accounts).
func DiscoverTenant(ctx context.Context, hc *http.Client, orgURL string) (string, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, orgURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	for _, v := range strings.Split(resp.Header.Get(TenantHeader), ",") {
		v = strings.TrimSpace(v)
		if guidRE.MatchString(v) && v != "00000000-0000-0000-0000-000000000000" {
			return strings.ToLower(v), nil
		}
	}
	return "", nil
}

func (a *Acquirer) ado(ctx context.Context, u *url.URL) (*Cred, error) {
	org := a.OrgURL
	if org == nil {
		org = OrgURL
	}
	tenant, err := DiscoverTenant(ctx, a.HTTP, org(u))
	if err != nil {
		a.logf("could not learn the tenant of %s (%v)", org(u), err)
	}
	if _, err := exec.LookPath("az"); err == nil {
		c, err := azToken(ctx, tenant)
		if err == nil {
			return c, nil
		}
		hint := "az login"
		if tenant != "" {
			hint = "az login --tenant " + tenant
		}
		a.logf("az could not get an Azure DevOps token (%v; %s); trying git credential fill", err, hint)
	}
	return credentialFill(ctx, u)
}

// azToken runs az account get-access-token for Azure DevOps (in tenant, if
// known).
func azToken(ctx context.Context, tenant string) (*Cred, error) {
	args := []string{"account", "get-access-token", "--resource", ADOResource, "--output", "json"}
	if tenant != "" {
		args = append(args, "--tenant", tenant)
	}
	out, err := runTool(ctx, nil, nil, "az", args...)
	defer clear(out)
	if err != nil {
		return nil, err
	}
	var r struct {
		AccessToken string `json:"accessToken"`
		Tenant      string `json:"tenant"`
	}
	if err := json.Unmarshal(out, &r); err != nil || r.AccessToken == "" {
		return nil, errors.New("az printed no access token")
	}
	src := "az"
	if tenant != "" {
		src = "az (tenant " + tenant + ")"
	}
	return &Cred{Scheme: Bearer, Secret: []byte(r.AccessToken), Source: src}, nil
}

// ghToken runs gh auth token.
func ghToken(ctx context.Context, host string) (*Cred, error) {
	out, err := runTool(ctx, nil, nil, "gh", "auth", "token", "--hostname", host)
	defer clear(out)
	if err != nil {
		return nil, err
	}
	tok := bytes.TrimSpace(out)
	if len(tok) == 0 || bytes.ContainsAny(tok, " \t\r\n") {
		return nil, errors.New("gh printed no token")
	}
	return &Cred{Scheme: Basic, Username: "x-access-token", Secret: bytes.Clone(tok), Source: "gh"}, nil
}

// credentialFill asks git's credential helpers (git credential fill;
// terminal prompts off, a helper's own sign-in window is fine).
func credentialFill(ctx context.Context, u *url.URL) (*Cred, error) {
	var in strings.Builder
	fmt.Fprintf(&in, "protocol=https\nhost=%s\n", u.Host)
	if p := strings.TrimPrefix(u.Path, "/"); p != "" {
		fmt.Fprintf(&in, "path=%s\n", p)
	}
	if u.User != nil && u.User.Username() != "" {
		fmt.Fprintf(&in, "username=%s\n", u.User.Username())
	}
	in.WriteString("\n")
	out, err := runTool(ctx, strings.NewReader(in.String()), []string{"GIT_TERMINAL_PROMPT=0"}, "git", "credential", "fill")
	defer clear(out)
	if err != nil {
		return nil, fmt.Errorf("--git-auth forward: no credential for %s (git credential fill: %v)", u.Host, err)
	}
	c := &Cred{Scheme: Basic, Source: "git credential fill"}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, _ := bytes.Cut(sc.Bytes(), []byte("="))
		switch string(k) {
		case "username":
			c.Username = string(v)
		case "password":
			c.Secret = bytes.Clone(v)
		}
	}
	if len(c.Secret) == 0 {
		return nil, fmt.Errorf("--git-auth forward: git credential fill gave no password for %s", u.Host)
	}
	return c, nil
}

// runTool runs a credential tool and returns its stdout (which holds the
// credential: callers clear it). Errors carry the first line of its stderr,
// never its stdout.
func runTool(ctx context.Context, stdin io.Reader, env []string, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		clear(out.Bytes())
		msg, _, _ := strings.Cut(strings.TrimSpace(errb.String()), "\n")
		if msg == "" {
			return nil, err
		}
		return nil, fmt.Errorf("%v: %s", err, strings.TrimSpace(msg))
	}
	return out.Bytes(), nil
}

var userinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^\s/?#@'"]+@`)

func redactURL(s string) string { return userinfo.ReplaceAllString(s, "${1}xxxxx@") }

// HTTPSURL turns a remote URL into the https URL of the same repository:
// https URLs as they are, ssh remotes of GitHub and Azure DevOps rewritten
// (git@github.com:o/r.git -> https://github.com/o/r.git,
// git@ssh.dev.azure.com:v3/org/proj/repo ->
// https://dev.azure.com/org/proj/_git/repo). ok is false for anything else.
func HTTPSURL(remote string) (string, bool) {
	remote = strings.TrimSpace(remote)
	if u, err := url.Parse(remote); err == nil && u.Host != "" {
		switch u.Scheme {
		case "https":
			return remote, true
		case "ssh":
			return sshToHTTPS(u.Hostname(), strings.TrimPrefix(u.Path, "/"))
		}
		return "", false
	}
	// scp-like: [user@]host:path
	at := strings.Index(remote, "@")
	colon := strings.Index(remote, ":")
	if colon < 0 || at > colon {
		return "", false
	}
	return sshToHTTPS(remote[at+1:colon], remote[colon+1:])
}

func sshToHTTPS(host, path string) (string, bool) {
	host = strings.ToLower(host)
	switch {
	case host == "github.com":
		return "https://github.com/" + path, path != ""
	case host == "ssh.dev.azure.com" || strings.HasSuffix(host, "vs-ssh.visualstudio.com"):
		p := strings.Split(strings.TrimSuffix(path, "/"), "/")
		if len(p) == 4 && p[0] == "v3" {
			return "https://dev.azure.com/" + p[1] + "/" + p[2] + "/_git/" + p[3], true
		}
	}
	return "", false
}
