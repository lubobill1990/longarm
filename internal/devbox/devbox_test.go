package devbox

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
