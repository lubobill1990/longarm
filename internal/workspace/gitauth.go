package workspace

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/redact"
)

// Fetch credentials (longarm run / sync --git-auth forward, docs/usage.md
// "Fetching with your credential"; also the job's GH_TOKEN secret).
//
// The credential never enters git's or git-lfs's environment, command line
// or config: git-lfs writes its environment and config into
// <mirror>/lfs/logs on a fatal error, git traces can too. Instead both ask
// a credential helper - longarmd itself (`longarmd git-credential-helper`,
// CredentialHelperMain) - that fetches it for this one fetch from a
// loopback listener of longarmd, which knows a random one-time key (that
// is in the environment; worthless once the fetch is over) and answers
// only for the exact protocol, host and repository path of the fetched URL
// (credential.useHttpPath). So a redirect to another host or to http, an
// LFS endpoint on another host (a hostile .lfsconfig) or a submodule never
// gets it. Redirects are refused outright (http.followRedirects=false),
// the LFS endpoint is pinned to the fetched URL's (lfs.url), git's traces
// are off, and the credential is masked in all output.

// CredHelperArg is the argument that makes longarmd (and the test
// binaries) run CredentialHelperMain.
const CredHelperArg = "git-credential-helper"

// credential-helper environment: where to ask, and the one-time key.
const (
	credAddrEnv = "LONGARM_GITCRED_ADDR"
	credKeyEnv  = "LONGARM_GITCRED_KEY"
)

// HelperExe is the executable git runs as credential helper with
// CredHelperArg (default: this process's executable).
var HelperExe = func() string { p, _ := os.Executable(); return p }

// fetchAuth is what one authenticated fetch adds to git's environment.
type fetchAuth struct {
	kv   [][2]string // git config entries
	vars []string    // other environment (no secret)
	red  *redact.Redactor
	kind authKind
	srv  *credServer
}

type authKind int

const (
	authNone authKind = iota
	authGHToken
	authForwarded
)

// scope is the URL a credential is for.
type scope struct {
	scheme, host string
	paths        []string // repository paths without the leading "/": "o/r" and "o/r.git"
}

// authScope is the scope of remote u: its scheme, host (with port) and
// path - with and without ".git", the form git-lfs's endpoint uses.
func authScope(u string, httpsOnly bool) (scope, string, error) {
	pu, err := url.Parse(u)
	if err != nil || pu.Host == "" {
		return scope{}, "", fmt.Errorf("%q is not a url", RedactURLs(u))
	}
	if pu.Scheme != "https" && (httpsOnly || pu.Scheme != "http") {
		return scope{}, "", fmt.Errorf("a forwarded credential is only sent over https, not to %s", RedactURLs(u))
	}
	p := strings.Trim(pu.Path, "/")
	sc := scope{scheme: pu.Scheme, host: strings.ToLower(pu.Host), paths: []string{p}}
	base := pu.Scheme + "://" + pu.Host + "/" + p
	if strings.HasSuffix(p, ".git") {
		sc.paths = append(sc.paths, strings.TrimSuffix(p, ".git"))
	} else {
		sc.paths = append(sc.paths, p+".git")
		base += ".git"
	}
	return sc, base + "/info/lfs", nil
}

func (sc scope) allows(protocol, host, path string) bool {
	if protocol != sc.scheme || !strings.EqualFold(host, sc.host) {
		return false
	}
	path = strings.Trim(path, "/")
	for _, p := range sc.paths {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// newFetchAuth prepares an authenticated fetch of u: a forwarded
// credential (https only) or else the job's GH_TOKEN secret (as basic auth
// "x-access-token"). It starts the credential listener: close it.
func newFetchAuth(u string, a *jobs.GitAuth, ghToken string) (*fetchAuth, error) {
	var user string
	var secret []byte
	kind := authNone
	switch {
	case a.Valid():
		kind, user, secret = authForwarded, a.Username, a.Secret
		if a.Scheme == jobs.GitAuthBearer || user == "" {
			// Azure DevOps takes an Entra token as the password of basic
			// auth (as Git Credential Manager sends it); GitHub as well
			user = "x-access-token"
		}
	case ghToken != "":
		kind, user, secret = authGHToken, "x-access-token", []byte(ghToken)
	default:
		return &fetchAuth{}, nil
	}
	sc, lfsURL, err := authScope(u, kind == authForwarded)
	if err != nil {
		return nil, err
	}
	srv, err := startCredServer(sc, user, secret)
	if err != nil {
		return nil, err
	}
	exe := filepath.ToSlash(HelperExe())
	kv := [][2]string{
		{"credential.helper", ""}, // none of the worker's own
		{"credential.useHttpPath", "true"},
		{"credential." + sc.scheme + "://" + sc.host + ".helper", `!"` + exe + `" ` + CredHelperArg},
		{"http.followRedirects", "false"},
		{"lfs.url", lfsURL}, // not a .lfsconfig's endpoint
	}
	return &fetchAuth{kv: kv, vars: []string{credAddrEnv + "=" + srv.ln.Addr().String(), credKeyEnv + "=" + srv.key},
		red: redact.New([]string{string(secret)}), kind: kind, srv: srv}, nil
}

// close ends the credential listener (nil-safe).
func (f *fetchAuth) close() {
	if f != nil && f.srv != nil {
		f.srv.close()
	}
}

// traceOff: git's and git-lfs's traces and verbose curl output off,
// whatever the worker's environment says.
var traceOff = []string{"GIT_TRACE=0", "GIT_TRACE_CURL=0", "GIT_TRACE_PACKET=0", "GIT_TRACE_PERFORMANCE=0",
	"GIT_TRACE_SETUP=0", "GIT_TRACE2=0", "GIT_TRACE2_EVENT=0", "GIT_TRACE2_PERF=0", "GIT_CURL_VERBOSE=0",
	"GIT_TRACE_REDACT=1", "GIT_TRANSFER_TRACE=0"}

// env is what the fetch adds to base: traces off, its config entries after
// any the worker's own environment passes (GIT_CONFIG_COUNT in base), and
// the credential helper's address and key.
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
	env := append([]string(nil), traceOff...)
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(first+len(f.kv)))
	for i, e := range f.kv {
		n := strconv.Itoa(first + i)
		env = append(env, "GIT_CONFIG_KEY_"+n+"="+e[0], "GIT_CONFIG_VALUE_"+n+"="+e[1])
	}
	return append(env, f.vars...)
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

// redirected matches git's refusal of a redirect (http.followRedirects=false).
var redirected = regexp.MustCompile(`(?i)(returned error: 30[1-8]\b|redirect)`)

// ---------------------------------------------------------------- listener

type credRequest struct {
	Key      string `json:"key"`
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Path     string `json:"path"`
}

type credAnswer struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// credServer answers the credential helper on 127.0.0.1 for one fetch.
type credServer struct {
	ln     net.Listener
	key    string
	sc     scope
	user   string
	secret []byte
	once   sync.Once
	wg     sync.WaitGroup
	mu     sync.Mutex
	asked  []string // what was asked, for tests ("protocol://host/path ok|refused")
}

func startCredServer(sc scope, user string, secret []byte) (*credServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	k := make([]byte, 32)
	rand.Read(k)
	s := &credServer{ln: ln, key: hex.EncodeToString(k), sc: sc, user: user, secret: secret}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.serve(c)
			}()
		}
	}()
	return s, nil
}

func (s *credServer) serve(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	var req credRequest
	if err := json.NewDecoder(io.LimitReader(c, 64<<10)).Decode(&req); err != nil {
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Key), []byte(s.key)) != 1 {
		return
	}
	ok := s.sc.allows(req.Protocol, req.Host, req.Path)
	s.mu.Lock()
	s.asked = append(s.asked, fmt.Sprintf("%s://%s/%s %v", req.Protocol, req.Host, req.Path, ok))
	s.mu.Unlock()
	if !ok {
		json.NewEncoder(c).Encode(credAnswer{})
		return
	}
	json.NewEncoder(c).Encode(credAnswer{Username: s.user, Password: string(s.secret)})
}

func (s *credServer) close() {
	s.once.Do(func() {
		s.ln.Close()
		s.wg.Wait()
	})
}

// CredentialHelperMain is `longarmd git-credential-helper <action>`: for
// "get" it reads git's request from stdin and asks longarmd's listener
// (credAddrEnv, credKeyEnv); it prints a username and password only when
// the listener has them for that exact URL. Other actions (store, erase)
// do nothing: a forwarded credential is never stored.
func CredentialHelperMain(args []string, stdin io.Reader, stdout io.Writer) int {
	if len(args) == 0 || args[len(args)-1] != "get" {
		io.Copy(io.Discard, stdin)
		return 0
	}
	req := credRequest{Key: os.Getenv(credKeyEnv)}
	sc := bufio.NewScanner(stdin)
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		switch k {
		case "protocol":
			req.Protocol = v
		case "host":
			req.Host = v
		case "path":
			req.Path = v
		}
	}
	addr := os.Getenv(credAddrEnv)
	if addr == "" || req.Key == "" {
		return 0
	}
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return 0
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return 0
	}
	var ans credAnswer
	if err := json.NewDecoder(c).Decode(&ans); err != nil || ans.Password == "" {
		return 0
	}
	fmt.Fprintf(stdout, "username=%s\npassword=%s\n", ans.Username, ans.Password)
	return 0
}
