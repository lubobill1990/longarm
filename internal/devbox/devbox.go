// Package devbox talks to the Microsoft Dev Box data plane (Dev Center
// API) for `longarm desktop`: it lists the signed-in user's Dev Boxes and
// fetches the link that opens one in Windows App ("ms-avd:connect?...").
//
// Longarm never carries a Dev Box's desktop: Windows App connects through
// Azure Virtual Desktop (RDP Shortpath, Entra sign-in, Conditional Access),
// which is faster and is what the Dev Box's policies expect.
//
// Credentials come from the Azure CLI (`az account get-access-token`), so
// Longarm stores no Azure tokens. A Dev Center's endpoint is
// https://<tenant>-<devcenter>.<region>.devcenter.azure.com; users can
// rarely read the Dev Center resource in ARM, so the region is found by
// resolving that name in each region Dev Box runs in (only one exists).
package devbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Resource is the token audience of the Dev Center data plane.
const Resource = "https://devcenter.azure.com"

const apiVersion = "2024-02-01"

// Regions are the Azure regions a Dev Center may live in.
var Regions = []string{
	"australiaeast", "brazilsouth", "canadacentral", "centralindia", "centralus", "eastasia",
	"eastus", "eastus2", "francecentral", "germanywestcentral", "israelcentral", "italynorth",
	"japaneast", "japanwest", "koreacentral", "mexicocentral", "northcentralus", "northeurope",
	"norwayeast", "polandcentral", "southafricanorth", "southcentralus", "southeastasia",
	"spaincentral", "swedencentral", "switzerlandnorth", "uaenorth", "uksouth", "ukwest",
	"westcentralus", "westeurope", "westus", "westus2", "westus3",
}

var (
	nameRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
	tenantRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// Host is a Dev Center endpoint's host name in region.
func Host(tenant, devCenter, region string) string {
	return fmt.Sprintf("%s-%s.%s.devcenter.azure.com", tenant, devCenter, region)
}

// ValidEndpoint reports whether e is a Dev Center data-plane endpoint
// (tokens are only ever sent to one).
func ValidEndpoint(e string) bool {
	u, err := url.Parse(e)
	return err == nil && u.Scheme == "https" && u.Path == "" && u.RawQuery == "" && u.User == nil &&
		strings.HasSuffix(u.Host, ".devcenter.azure.com") && !strings.Contains(u.Host, ":")
}

// Resolver looks a host name up (net.DefaultResolver; a fake in tests).
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// FindEndpoint finds the endpoint of devCenter (in tenant) by resolving its
// host name in every region at once.
func FindEndpoint(ctx context.Context, r Resolver, tenant, devCenter string) (string, error) {
	tenant, devCenter = strings.ToLower(tenant), strings.ToLower(devCenter)
	if !tenantRE.MatchString(tenant) || !nameRE.MatchString(devCenter) {
		return "", fmt.Errorf("bad Dev Center %q (tenant %q)", devCenter, tenant)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var mu sync.Mutex
	var found []string
	var wg sync.WaitGroup
	for _, region := range Regions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if addrs, err := r.LookupHost(ctx, Host(tenant, devCenter, region)); err == nil && len(addrs) > 0 {
				mu.Lock()
				found = append(found, region)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(found) == 0 {
		return "", fmt.Errorf("Dev Center %s not found in any region (is DNS working?)", devCenter)
	}
	slices.Sort(found)
	return "https://" + Host(tenant, devCenter, found[0]), nil
}

// DevBox is one of the user's Dev Boxes.
type DevBox struct {
	Name              string `json:"name"`
	ProjectName       string `json:"projectName"`
	PoolName          string `json:"poolName"`
	PowerState        string `json:"powerState"`
	OSType            string `json:"osType"`
	Location          string `json:"location"`
	UniqueID          string `json:"uniqueId"`
	ProvisioningState string `json:"provisioningState"`
}

// RemoteConnection is how a Dev Box is opened.
type RemoteConnection struct {
	// WebURL opens it in the browser (the AVD web client).
	WebURL string `json:"webUrl"`
	// RDPConnectionURL opens it in Windows App ("ms-avd:connect?...").
	RDPConnectionURL string `json:"rdpConnectionUrl"`
}

// Client calls one Dev Center's data plane.
type Client struct {
	Endpoint string // https://<tenant>-<devcenter>.<region>.devcenter.azure.com
	Token    string
	HTTP     *http.Client // nil = a default one
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	if !ValidEndpoint(c.Endpoint) {
		return fmt.Errorf("not a Dev Center endpoint: %q", c.Endpoint)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Endpoint+path+"?api-version="+apiVersion, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
			return fmt.Errorf("Dev Center: %s: %s", e.Error.Code, e.Error.Message)
		}
		return fmt.Errorf("Dev Center: %s", resp.Status)
	}
	return json.Unmarshal(body, out)
}

// DevBoxes lists the signed-in user's Dev Boxes in all projects.
func (c *Client) DevBoxes(ctx context.Context) ([]DevBox, error) {
	var out struct {
		Value []DevBox `json:"value"`
	}
	if err := c.get(ctx, "/users/me/devboxes", &out); err != nil {
		return nil, err
	}
	return out.Value, nil
}

// RemoteConnection fetches how to open the Dev Box name of project.
func (c *Client) RemoteConnection(ctx context.Context, project, name string) (RemoteConnection, error) {
	var rc RemoteConnection
	err := c.get(ctx, "/projects/"+url.PathEscape(project)+"/users/me/devboxes/"+url.PathEscape(name)+"/remoteConnection", &rc)
	if err == nil && !ValidLaunchURL(rc.RDPConnectionURL) && !ValidLaunchURL(rc.WebURL) {
		err = errors.New("Dev Center returned no usable link for this Dev Box")
	}
	return rc, err
}

// ValidLaunchURL reports whether u may be handed to the shell to open:
// a Windows App link (ms-avd:) or an https URL.
func ValidLaunchURL(u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	switch p.Scheme {
	case "ms-avd":
		return strings.HasPrefix(p.Opaque, "connect")
	case "https":
		return p.Host != "" && p.User == nil
	}
	return false
}

// AzToken gets a Dev Center token for tenant from the Azure CLI.
func AzToken(ctx context.Context, tenant string) (string, error) {
	if !tenantRE.MatchString(tenant) {
		return "", fmt.Errorf("bad tenant id %q", tenant)
	}
	az, err := exec.LookPath("az")
	if err != nil {
		return "", errors.New("the Azure CLI (az) is not installed: https://aka.ms/installazurecli")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, az, "account", "get-access-token", "--tenant", tenant,
		"--resource", Resource, "--query", "accessToken", "-o", "tsv")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	tok := strings.TrimSpace(string(out))
	if err != nil || tok == "" {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			msg = msg[:i]
		}
		return "", fmt.Errorf("az account get-access-token: %s (sign in with: az login --tenant %s)", nonEmpty(msg, fmt.Sprint(err)), tenant)
	}
	return tok, nil
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// DefaultResolver is net.DefaultResolver.
var DefaultResolver Resolver = net.DefaultResolver

// PortalURL is the Dev Box portal (the fallback when the API can't be used).
const PortalURL = "https://devbox.microsoft.com"

// CloudPCPortalURL is the Windows 365 portal.
const CloudPCPortalURL = "https://windows365.microsoft.com"
