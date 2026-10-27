// Package azauth gets Microsoft Entra ID tokens for the hub's managed
// identity (deploy/azure: a user-assigned identity, AZURE_CLIENT_ID), for
// the Azure services the hub talks to itself: Application Insights
// ingestion (internal/telemetry) and Blob Storage for diagnostic bundles
// (internal/diag). Storage account keys and instrumentation keys are not
// used: the deployment's subscription may deny shared keys, and a token
// expires.
//
// No Azure SDK: the IMDS protocol is one GET, and the hub image stays small.
package azauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Resources (token audiences).
const (
	ResourceStorage = "https://storage.azure.com/"
	ResourceMonitor = "https://monitor.azure.com/"
)

// TokenSource returns a bearer token for a resource.
type TokenSource interface {
	Token(ctx context.Context, resource string) (string, error)
}

// Static is a fixed token (tests, a token from `az account get-access-token`).
type Static string

// Token returns s.
func (s Static) Token(context.Context, string) (string, error) { return string(s), nil }

// ManagedIdentity gets tokens from the instance metadata service (Azure VMs,
// Container Instances) or, when IDENTITY_ENDPOINT is set, from the
// App Service / Container Apps identity endpoint. Tokens are cached until
// five minutes before they expire.
type ManagedIdentity struct {
	// ClientID selects a user-assigned identity (default: $AZURE_CLIENT_ID;
	// empty = the system-assigned one).
	ClientID string
	// Endpoint overrides the token endpoint (tests).
	Endpoint string
	Client   *http.Client

	mu    sync.Mutex
	cache map[string]token
}

type token struct {
	value   string
	expires time.Time
}

const imdsEndpoint = "http://169.254.169.254/metadata/identity/oauth2/token"

// Available reports whether a managed identity endpoint is configured or
// likely (AZURE_CLIENT_ID set, as deploy/azure does).
func Available() bool {
	return os.Getenv("IDENTITY_ENDPOINT") != "" || os.Getenv("AZURE_CLIENT_ID") != ""
}

// Token returns a token for resource.
func (m *ManagedIdentity) Token(ctx context.Context, resource string) (string, error) {
	m.mu.Lock()
	if t, ok := m.cache[resource]; ok && time.Until(t.expires) > 5*time.Minute {
		m.mu.Unlock()
		return t.value, nil
	}
	m.mu.Unlock()

	clientID := m.ClientID
	if clientID == "" {
		clientID = os.Getenv("AZURE_CLIENT_ID")
	}
	q := url.Values{"resource": {resource}}
	endpoint, header, val := m.Endpoint, "Metadata", "true"
	if ie := os.Getenv("IDENTITY_ENDPOINT"); ie != "" && endpoint == "" {
		endpoint, header, val = ie, "X-IDENTITY-HEADER", os.Getenv("IDENTITY_HEADER")
		q.Set("api-version", "2019-08-01")
	} else {
		q.Set("api-version", "2018-02-01")
	}
	if endpoint == "" {
		endpoint = imdsEndpoint
	}
	if clientID != "" {
		q.Set("client_id", clientID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set(header, val)
	c := m.Client
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("managed identity: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("managed identity: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var r struct {
		AccessToken string          `json:"access_token"`
		ExpiresOn   json.RawMessage `json:"expires_on"` // a string (IMDS) or a number
		ExpiresIn   json.RawMessage `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.AccessToken == "" {
		return "", errors.New("managed identity: no access_token in the answer")
	}
	exp := time.Now().Add(10 * time.Minute)
	if n := jsonInt(r.ExpiresOn); n > 0 {
		exp = time.Unix(n, 0)
	} else if n := jsonInt(r.ExpiresIn); n > 0 {
		exp = time.Now().Add(time.Duration(n) * time.Second)
	}
	m.mu.Lock()
	if m.cache == nil {
		m.cache = map[string]token{}
	}
	m.cache[resource] = token{r.AccessToken, exp}
	m.mu.Unlock()
	return r.AccessToken, nil
}

func jsonInt(raw json.RawMessage) int64 {
	s := strings.Trim(string(raw), `"`)
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
