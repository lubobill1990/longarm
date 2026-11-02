package devbox

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const tenant = "72f988bf-86f1-41af-91ab-2d7cd011db47"

type fakeResolver map[string]bool

func (f fakeResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	if f[host] {
		return []string{"10.0.0.1"}, nil
	}
	return nil, errors.New("no such host")
}

func TestFindEndpoint(t *testing.T) {
	r := fakeResolver{Host(tenant, "dc1", "westus3"): true}
	e, err := FindEndpoint(context.Background(), r, strings.ToUpper(tenant), "DC1")
	if err != nil || e != "https://"+tenant+"-dc1.westus3.devcenter.azure.com" {
		t.Fatalf("got %q, %v", e, err)
	}
	if !ValidEndpoint(e) {
		t.Fatalf("%s not valid", e)
	}
	if _, err := FindEndpoint(context.Background(), fakeResolver{}, tenant, "dc1"); err == nil {
		t.Fatal("found an endpoint that does not exist")
	}
	for _, bad := range [][2]string{{"not-a-tenant", "dc1"}, {tenant, "a.evil.com"}, {tenant, "x/y"}} {
		if _, err := FindEndpoint(context.Background(), r, bad[0], bad[1]); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestValidEndpoint(t *testing.T) {
	for e, want := range map[string]bool{
		"https://t-dc.westus3.devcenter.azure.com":         true,
		"http://t-dc.westus3.devcenter.azure.com":          false,
		"https://t-dc.westus3.devcenter.azure.com.evil.io": false,
		"https://evil.io/x.devcenter.azure.com":            false,
		"https://t-dc.westus3.devcenter.azure.com/path":    false,
		"https://u@t-dc.westus3.devcenter.azure.com":       false,
		"https://t-dc.westus3.devcenter.azure.com:8443":    false,
		"": false,
	} {
		if got := ValidEndpoint(e); got != want {
			t.Errorf("ValidEndpoint(%q) = %v", e, got)
		}
	}
}

func TestValidLaunchURL(t *testing.T) {
	for u, want := range map[string]bool{
		"ms-avd:connect?env=avdarm&resourceid=70dbacfd-19fa-4416-9818-08dec151a051&username=a%40b.com&version=0&workspaceId=30e58361-6bbb-4118-9f13-932445b5eafd": true,
		"https://devbox.microsoft.com":   true,
		"ms-avd:subscribe?url=x":         false,
		"file:///C:/Windows/notepad.exe": false,
		"http://devbox.microsoft.com":    false,
		`C:\Windows\System32\calc.exe`:   false,
		"https://user@evil.io":           false,
	} {
		if got := ValidLaunchURL(u); got != want {
			t.Errorf("ValidLaunchURL(%q) = %v", u, got)
		}
	}
}

// devCenter serves the Dev Center API calls longarm desktop makes, in the
// shapes the real service returns.
func devCenter(t *testing.T) (*Client, *httptest.Server) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/me/devboxes", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Query().Get("api-version") != apiVersion {
			http.Error(w, `{"error":{"code":"Unauthorized","message":"no token"}}`, http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"value":[{"name":"arm1","projectName":"ODSP","poolName":"SPO_southeastasia","powerState":"Running","osType":"Windows","uniqueId":"fa6b35f7-a922-44ba-a34d-fc6091b3399d"},{"name":"bot1","projectName":"ODSP","powerState":"Stopped","osType":"Windows"}]}`))
	})
	mux.HandleFunc("GET /projects/ODSP/users/me/devboxes/arm1/remoteConnection", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"webUrl":"https://deschutes.microsoft.com/webclient/ent/86698800","rdpConnectionUrl":"ms-avd:connect?env=avdarm&resourceid=186b00f0&username=a%40b.com&version=0&workspaceId=3be7469e"}`))
	})
	mux.HandleFunc("GET /projects/ODSP/users/me/devboxes/bot1/remoteConnection", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"webUrl":"javascript:alert(1)","rdpConnectionUrl":"file:///x"}`))
	})
	srv := httptest.NewTLSServer(mux)
	hc := srv.Client()
	tr := hc.Transport.(*http.Transport)
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	return &Client{Endpoint: "https://" + Host(tenant, "dc1", "westus3"), Token: "tok", HTTP: hc}, srv
}

func TestClient(t *testing.T) {
	c, srv := devCenter(t)
	defer srv.Close()
	ctx := context.Background()
	boxes, err := c.DevBoxes(ctx)
	if err != nil || len(boxes) != 2 || boxes[0].Name != "arm1" || boxes[0].ProjectName != "ODSP" || boxes[1].PowerState != "Stopped" {
		t.Fatalf("DevBoxes: %+v, %v", boxes, err)
	}
	rc, err := c.RemoteConnection(ctx, "ODSP", "arm1")
	if err != nil || !strings.HasPrefix(rc.RDPConnectionURL, "ms-avd:connect?") || !ValidLaunchURL(rc.WebURL) {
		t.Fatalf("RemoteConnection: %+v, %v", rc, err)
	}
	if _, err := c.RemoteConnection(ctx, "ODSP", "bot1"); err == nil {
		t.Fatal("unusable links accepted")
	}
	bad := *c
	bad.Token = "other"
	if _, err := bad.DevBoxes(ctx); err == nil || !strings.Contains(err.Error(), "no token") {
		t.Fatalf("error not passed on: %v", err)
	}
	bad = *c
	bad.Endpoint = "https://evil.io"
	if _, err := bad.DevBoxes(ctx); err == nil {
		t.Fatal("token sent to a host that is not a Dev Center")
	}
}

func TestAzTokenRejectsBadTenant(t *testing.T) {
	if _, err := AzToken(context.Background(), "--query=x"); err == nil || !strings.Contains(err.Error(), "bad tenant") {
		t.Fatalf("got %v", err)
	}
}

func ts(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestMatch: the owner's real Dev Boxes and workers (2026-11-02): each
// worker is told by when Windows was installed on it; arm1 and arm2 were
// created 15 s apart, so their workers are told by the last remote logon.
func TestMatch(t *testing.T) {
	box := func(name, created, lastConn string) DevBox {
		b := DevBox{Name: name, ProjectName: "ODSP", CreatedTime: ts(t, created)}
		if lastConn != "" {
			b.LastConnectedTime = ts(t, lastConn)
		}
		b.HardwareProfile.VCPUs, b.HardwareProfile.MemoryGB = 32, 128
		return b
	}
	boxes := []DevBox{
		box("bot1", "2026-07-09T03:03:57Z", "2026-11-01T12:32:33Z"),
		box("bot2", "2026-07-24T17:45:28Z", "2026-11-01T10:47:16Z"),
		box("bot3", "2026-07-24T17:46:07Z", "2026-10-25T09:43:34Z"),
		box("bot4", "2026-10-10T02:31:39Z", "2026-10-30T17:57:02Z"),
		box("ghrunner", "2026-10-19T17:00:22Z", "2026-11-01T10:23:02Z"),
		box("arm1", "2026-10-26T06:07:52Z", "2026-10-29T03:56:44Z"),
		box("arm2", "2026-10-26T06:08:07Z", "2026-10-27T05:12:30Z"),
		box("arm3", "2026-10-27T06:43:42Z", "2026-10-29T05:33:02Z"),
		box("arm4", "2026-10-30T14:19:10Z", "2026-11-01T02:46:12Z"),
	}
	for _, c := range []struct {
		worker, installed, lastLogon, want string
	}{
		{"cpc-alice-252m5", "2026-10-27T06:51:55Z", "2026-10-29T05:24:36Z", "arm3"},
		{"cpc-alice-hucaa", "2026-10-30T14:28:59Z", "", "arm4"},
		{"cpc-alice-nyg7h", "2026-10-26T06:14:59Z", "2026-10-29T03:52:51Z", "arm1"},
		{"cpc-alice-xefcn", "2026-10-26T06:17:07Z", "2026-10-27T05:07:01Z", "arm2"},
	} {
		f := Facts{Installed: ts(t, c.installed), CPUs: 32}
		if c.lastLogon != "" {
			f.LastLogon = ts(t, c.lastLogon)
		}
		ranked, sure, why := Match(boxes, f)
		if !sure || ranked[0].Name != c.want {
			t.Errorf("%s: got %s (sure %v, %q), want %s", c.worker, ranked[0].Name, sure, why, c.want)
		}
	}
	// arm1 / arm2 without a remote logon: not sure, but the two rank first
	// (created 15 s apart: either may be the closer one)
	ranked, sure, _ := Match(boxes, Facts{Installed: ts(t, "2026-10-26T06:14:59Z")})
	if top := ranked[0].Name + "," + ranked[1].Name; sure || (top != "arm1,arm2" && top != "arm2,arm1") {
		t.Fatalf("no logon: sure %v, ranked %s %s", sure, ranked[0].Name, ranked[1].Name)
	}
	// the wrong size never matches
	if _, sure, _ := Match(boxes, Facts{Installed: ts(t, "2026-10-27T06:51:55Z"), CPUs: 16}); sure {
		t.Fatal("a 16-CPU worker matched a 32-vCPU Dev Box")
	}
	// an older worker (no facts): the list as it was, nothing sure
	ranked, sure, _ = Match(boxes, Facts{})
	if sure || ranked[0].Name != "bot1" {
		t.Fatalf("no facts: sure %v, first %s", sure, ranked[0].Name)
	}
	// Windows installed long after any creation (a rebuilt machine): no guess
	if _, sure, _ := Match(boxes, Facts{Installed: ts(t, "2026-10-31T09:00:00Z")}); sure {
		t.Fatal("matched outside the provisioning window")
	}
}

// The Dev Center's timestamps decode (fractional seconds, offset; absent
// lastConnectedTime).
func TestDevBoxJSON(t *testing.T) {
	var b DevBox
	if err := json.Unmarshal([]byte(`{"name":"arm3","createdTime":"2026-10-27T06:43:42.5531906+00:00","hardwareProfile":{"vCPUs":32,"memoryGB":128}}`), &b); err != nil {
		t.Fatal(err)
	}
	if !b.CreatedTime.Equal(ts(t, "2026-10-27T06:43:42Z").Add(553190600)) || !b.LastConnectedTime.IsZero() || b.HardwareProfile.VCPUs != 32 {
		t.Fatalf("got %+v", b)
	}
}
