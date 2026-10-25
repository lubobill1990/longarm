// Package fakets is a fake of the parts of the Tailscale API the hub uses
// (OAuth client-credentials token, auth key creation and deletion), for
// unit and end-to-end tests. Like the real service it checks the client
// credentials, the token's scope, the tailnet, the request body and the
// tags the OAuth client may use.
package fakets

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"time"
)

// Server is a fake Tailscale API.
type Server struct {
	// ClientID / Secret of the one OAuth client.
	ClientID, Secret string
	// Scopes granted to its tokens (default "auth_keys").
	Scopes []string
	// Tags of the OAuth client / trust credential (default tag:longarm-hub).
	Tags []string
	// TagOwners is the policy's tagOwners (tag -> owning tags); default:
	// tag:longarm-hub owns tag:longarm-worker and tag:longarm-client. Like the real
	// service, a key must carry exactly the client's tags, or only tags
	// owned by them.
	TagOwners map[string][]string
	// Tailnet accepted besides "-" (default "example.com").
	Tailnet string
	// TokenTTL is expires_in of tokens (default 1h).
	TokenTTL time.Duration
	// EchoSecret makes key errors echo the request's credentials (to test
	// redaction).
	EchoSecret bool

	*httptest.Server

	mu       sync.Mutex
	tokens   map[string]time.Time // access token -> expiry
	nTokens  int
	keys     []KeyRequest
	deleted  []string
	failNext int // status of the next key request (0 = none)
}

// KeyRequest is a recorded, accepted key creation.
type KeyRequest struct {
	ID            string
	Key           string
	Tailnet       string
	Reusable      bool
	Ephemeral     bool
	Preauthorized bool
	Tags          []string
	ExpirySeconds int64
	Description   string
}

// Start starts the fake (closed at test cleanup by the caller).
func Start(clientID, secret string) *Server {
	s := &Server{ClientID: clientID, Secret: secret, tokens: map[string]time.Time{}}
	s.Server = httptest.NewServer(s)
	return s
}

// SecretFor builds an OAuth client secret the way Tailscale formats them.
func SecretFor(clientID string) string { return "tskey-client-" + clientID + "-" + randHex(12) }

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) scopes() []string {
	if len(s.Scopes) == 0 {
		return []string{"auth_keys"}
	}
	return s.Scopes
}

func (s *Server) tags() []string {
	if len(s.Tags) == 0 {
		return []string{"tag:longarm-hub"}
	}
	return s.Tags
}

func (s *Server) tagOwners() map[string][]string {
	if s.TagOwners == nil {
		return map[string][]string{"tag:longarm-worker": {"tag:longarm-hub"}, "tag:longarm-client": {"tag:longarm-hub"}}
	}
	return s.TagOwners
}

// tagsAllowed: exactly the client's tags, or only tags owned by one of them.
func (s *Server) tagsAllowed(req []string) bool {
	own := s.tags()
	if slices.Equal(sorted(req), sorted(own)) {
		return true
	}
	owners := s.tagOwners()
	for _, t := range req {
		if !slices.ContainsFunc(owners[t], func(o string) bool { return slices.Contains(own, o) }) {
			return false
		}
	}
	return true
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return slices.Compact(s)
}

func apiErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v2/oauth/token":
		s.token(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/v2/tailnet/"):
		rest := strings.TrimPrefix(r.URL.Path, "/api/v2/tailnet/")
		tailnet, rest, _ := strings.Cut(rest, "/")
		if !s.authorized(w, r) {
			return
		}
		if tailnet != "-" && tailnet != s.tailnetName() {
			apiErr(w, http.StatusNotFound, "tailnet not found")
			return
		}
		switch {
		case r.Method == http.MethodPost && rest == "keys":
			s.createKey(w, r, tailnet)
		case r.Method == http.MethodDelete && strings.HasPrefix(rest, "keys/"):
			s.deleted = append(s.deleted, strings.TrimPrefix(rest, "keys/"))
			w.WriteHeader(http.StatusOK)
		default:
			apiErr(w, http.StatusNotFound, "not found")
		}
	default:
		apiErr(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) tailnetName() string {
	if s.Tailnet == "" {
		return "example.com"
	}
	return s.Tailnet
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		apiErr(w, http.StatusBadRequest, "bad form")
		return
	}
	if r.PostForm.Get("client_id") != s.ClientID || r.PostForm.Get("client_secret") != s.Secret {
		apiErr(w, http.StatusUnauthorized, "invalid client credentials")
		return
	}
	if gt := r.PostForm.Get("grant_type"); gt != "" && gt != "client_credentials" {
		apiErr(w, http.StatusBadRequest, "unsupported grant_type")
		return
	}
	ttl := s.TokenTTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	s.nTokens++
	tok := fmt.Sprintf("tskey-api-fake%d-%s", s.nTokens, randHex(8))
	s.tokens[tok] = time.Now().Add(ttl)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "token_type": "Bearer",
		"expires_in": int64(ttl / time.Second), "scope": strings.Join(s.scopes(), " ")})
}

func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	exp, known := s.tokens[tok]
	if !ok || !known || time.Now().After(exp) {
		apiErr(w, http.StatusUnauthorized, "API token invalid")
		return false
	}
	return true
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request, tailnet string) {
	if f := s.failNext; f != 0 {
		s.failNext = 0
		apiErr(w, f, "injected failure")
		return
	}
	hasScope := false
	for _, sc := range s.scopes() {
		if sc == "auth_keys" || sc == "all" {
			hasScope = true
		}
	}
	if !hasScope {
		apiErr(w, http.StatusForbidden, "calling actor does not have enough permissions to perform this function")
		return
	}
	var body struct {
		Capabilities struct {
			Devices struct {
				Create struct {
					Reusable      bool     `json:"reusable"`
					Ephemeral     bool     `json:"ephemeral"`
					Preauthorized bool     `json:"preauthorized"`
					Tags          []string `json:"tags"`
				} `json:"create"`
			} `json:"devices"`
		} `json:"capabilities"`
		ExpirySeconds int64  `json:"expirySeconds"`
		Description   string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apiErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	c := body.Capabilities.Devices.Create
	if len(c.Tags) == 0 {
		apiErr(w, http.StatusBadRequest, "keys created by an OAuth client must have tags")
		return
	}
	if !s.tagsAllowed(c.Tags) {
		// The real answer (400), seen with a credential tagged
		// tag:longarm-worker + tag:longarm-client asked for tag:longarm-worker alone.
		msg := fmt.Sprintf("requested tags [%s] are invalid or not permitted", strings.Join(c.Tags, ", "))
		if s.EchoSecret {
			msg += " (client " + s.Secret + ", token " + r.Header.Get("Authorization") + ")"
		}
		apiErr(w, http.StatusBadRequest, msg)
		return
	}
	if body.ExpirySeconds <= 0 || body.ExpirySeconds > 90*24*3600 {
		apiErr(w, http.StatusBadRequest, "expirySeconds out of range")
		return
	}
	if len(body.Description) > 50 || strings.IndexFunc(body.Description, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == ' ')
	}) >= 0 {
		apiErr(w, http.StatusBadRequest, "description: up to 50 alphanumeric characters, hyphens and spaces")
		return
	}
	id := fmt.Sprintf("k%d%s", len(s.keys)+1, "CNTRL")
	k := KeyRequest{ID: id, Key: "tskey-auth-" + id + "-" + randHex(12), Tailnet: tailnet, Reusable: c.Reusable,
		Ephemeral: c.Ephemeral, Preauthorized: c.Preauthorized, Tags: c.Tags, ExpirySeconds: body.ExpirySeconds,
		Description: body.Description}
	s.keys = append(s.keys, k)
	now := time.Now().UTC()
	out := map[string]any{"id": id, "key": k.Key, "created": now, "expires": now.Add(time.Duration(body.ExpirySeconds) * time.Second),
		"description": body.Description, "capabilities": body.Capabilities}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// Keys returns the accepted key requests.
func (s *Server) Keys() []KeyRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]KeyRequest(nil), s.keys...)
}

// TokenCount is how many access tokens were issued.
func (s *Server) TokenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nTokens
}

// Deleted lists the ids of deleted keys.
func (s *Server) Deleted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleted...)
}

// RevokeTokens invalidates every issued access token.
func (s *Server) RevokeTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.tokens)
}

// FailNext makes the next key request fail with status code.
func (s *Server) FailNext(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = code
}

// SetTags changes the OAuth client's tags and the policy's tagOwners while
// the fake serves (nil = the defaults).
func (s *Server) SetTags(tags []string, owners map[string][]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Tags, s.TagOwners = tags, owners
}
