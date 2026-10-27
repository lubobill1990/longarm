package workspace

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/redact"
)

// Forwarded fetch credentials (longarm run / sync --git-auth forward,
// docs/usage.md "Fetching with your credential"). The credential reaches
// git and git-lfs as an Authorization header through GIT_CONFIG_COUNT /
// GIT_CONFIG_KEY_<n> / GIT_CONFIG_VALUE_<n> in their environment - never on
// a command line, in a config file or in a remote URL - and the header is
// scoped to the fetched URL (http.<url>.extraHeader): requests to another
// URL, or a redirect to another host, do not carry it.

// fetchAuth is what one authenticated fetch adds to git's environment.
type fetchAuth struct {
	kv   [][2]string      // git config entries
	red  *redact.Redactor // the secret and the header value
	kind authKind
}

type authKind int

const (
	authNone authKind = iota
	authGHToken
	authForwarded
)

// authScopes are the http.<url> prefixes a credential for remote u is sent
// to: u itself (userinfo, query and fragment dropped) and, when u does not
// end in .git, u + ".git" - git-lfs's endpoint for such a remote is
// <u>.git/info/lfs, and git matches config urls on whole path segments.
func authScopes(u string) ([]string, error) {
	pu, err := url.Parse(u)
	if err != nil || pu.Host == "" {
		return nil, fmt.Errorf("%q is not a url", RedactURLs(u))
	}
	if pu.Scheme != "https" {
		return nil, fmt.Errorf("a forwarded credential is only sent over https, not to %s", RedactURLs(u))
	}
	base := "https://" + pu.Host + strings.TrimRight(pu.EscapedPath(), "/")
	scopes := []string{base}
	if !strings.HasSuffix(base, ".git") {
		scopes = append(scopes, base+".git")
	}
	return scopes, nil
}

// headerValue is the Authorization header value of a.
func headerValue(a *jobs.GitAuth) []byte {
	switch a.Scheme {
	case jobs.GitAuthBearer:
		return append([]byte("Bearer "), a.Secret...)
	default:
		user := a.Username
		if user == "" {
			user = "x-access-token"
		}
		raw := append([]byte(user+":"), a.Secret...)
		enc := make([]byte, base64.StdEncoding.EncodedLen(len(raw)))
		base64.StdEncoding.Encode(enc, raw)
		clear(raw)
		return append([]byte("Basic "), enc...)
	}
}

// newFetchAuth builds the environment that authenticates a fetch of u: a
// forwarded credential (scoped to u, https only) or else the job's GH_TOKEN
// secret (GitHub-style basic auth to u's host, as before). Credential
// helpers are switched off for a forwarded credential: a refused token
// fails the fetch instead of falling back to the worker's own credentials,
// and no helper stores it.
func newFetchAuth(u string, a *jobs.GitAuth, ghToken string) (*fetchAuth, error) {
	switch {
	case a.Valid():
		scopes, err := authScopes(u)
		if err != nil {
			return nil, err
		}
		hv := headerValue(a)
		defer clear(hv)
		kv := [][2]string{{"credential.helper", ""}}
		for _, s := range scopes {
			kv = append(kv, [2]string{"http." + s + ".extraHeader", "Authorization: " + string(hv)})
		}
		vals := []string{string(a.Secret), string(hv)}
		if _, enc, ok := strings.Cut(string(hv), " "); ok {
			vals = append(vals, enc)
		}
		return &fetchAuth{kv: kv, red: redact.New(vals), kind: authForwarded}, nil
	case ghToken != "":
		pu, err := url.Parse(u)
		if err != nil || (pu.Scheme != "https" && pu.Scheme != "http") || pu.Host == "" {
			return nil, fmt.Errorf("GH_TOKEN given but %q is not an http(s) url", RedactURLs(u))
		}
		cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + ghToken))
		kv := [][2]string{{"http." + pu.Scheme + "://" + pu.Host + "/.extraheader", "AUTHORIZATION: basic " + cred}}
		return &fetchAuth{kv: kv, red: redact.New([]string{ghToken, cred}), kind: authGHToken}, nil
	}
	return &fetchAuth{}, nil
}

// env passes the config entries in the environment, after any the
// worker's own environment passes that way (GIT_CONFIG_COUNT in base).
func (f *fetchAuth) env(base []string) []string {
	if f == nil || len(f.kv) == 0 {
		return nil
	}
	first := 0
	for _, e := range base {
		if v, ok := strings.CutPrefix(e, "GIT_CONFIG_COUNT="); ok {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				first = n
			}
		}
	}
	env := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(first+len(f.kv))}
	for i, e := range f.kv {
		n := strconv.Itoa(first + i)
		env = append(env, "GIT_CONFIG_KEY_"+n+"="+e[0], "GIT_CONFIG_VALUE_"+n+"="+e[1])
	}
	return env
}

// redact masks the credential (nil-safe).
func (f *fetchAuth) redact(s string) string {
	if f == nil || f.red == nil {
		return s
	}
	return string(f.red.Bytes([]byte(s)))
}

// authHeader matches Authorization header values in git / curl / git-lfs
// output (GIT_TRACE_CURL, error messages) whatever the credential.
var authHeader = regexp.MustCompile(`(?i)(authorization:\s*(?:bearer|basic|token)\s+)[^\s"']+`)
