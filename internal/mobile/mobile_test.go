package mobile

import (
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
)

// The API answers only with the token: other apps on the phone reach
// 127.0.0.1 too. The page's URL carries it once, as a cookie from then on.
func TestTokenGate(t *testing.T) {
	c, err := Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	base := "http://" + c.ln.Addr().String()

	get := func(cl *http.Client, url string, hdr ...string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", url, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	plain := &http.Client{}
	for _, p := range []string{"/mapi/status", "/mapi/machines", "/local/longarm/v1/workers/w/sessions", "/mapi/status?t=wrong"} {
		if code := get(plain, base+p); code != http.StatusForbidden {
			t.Errorf("%s without the token: %d, want 403", p, code)
		}
	}
	if code := get(plain, base+"/mapi/status", "X-Longarm-Token", c.token); code == http.StatusForbidden {
		t.Errorf("the header token was refused")
	}

	jar, _ := cookiejar.New(nil)
	page := &http.Client{Jar: jar}
	if !strings.Contains(c.URL(), "/m?t="+c.token) {
		t.Fatalf("URL %q has no token", c.URL())
	}
	get(page, c.URL()) // sets the cookie
	if code := get(page, base+"/mapi/status"); code == http.StatusForbidden {
		t.Errorf("the cookie from the page's URL was refused")
	}
	// a wrong ?t= sets no cookie
	jar2, _ := cookiejar.New(nil)
	other := &http.Client{Jar: jar2}
	get(other, base+"/m?t=wrong")
	if code := get(other, base+"/mapi/status"); code != http.StatusForbidden {
		t.Errorf("a wrong page token got a cookie: %d", code)
	}
}
