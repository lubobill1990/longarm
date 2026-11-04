package updatesvc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type releaseTransport func(*http.Request) (*http.Response, error)

func (f releaseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Exercise net/http's real redirect handling, without contacting GitHub.
func TestHTTPSourceRedirects(t *testing.T) {
	const base = "https://hub.example.com"
	const path = "/releases/0.1.32/longarm-windows-amd64.msi"
	for _, tc := range []struct {
		name  string
		chain []string
		want  string
	}{
		{"same host", []string{base + "/asset"}, ""},
		{"objects", []string{"https://github.com/o/r/releases/download/v0.1.32/file", "https://objects.githubusercontent.com/file"}, ""},
		{"release assets", []string{"https://github.com/file", "https://release-assets.githubusercontent.com/file"}, ""},
		{"legacy release host", []string{"https://github-releases.githubusercontent.com/file"}, ""},
		{"objects origin", []string{"https://objects-origin.githubusercontent.com/file"}, ""},
		{"case insensitive", []string{"https://GITHUB.COM/file", "https://OBJECTS.GITHUBUSERCONTENT.COM/file"}, ""},
		{"other host", []string{"https://github.com/file", "https://evil.example.com/file"}, "host \"evil.example.com\""},
		{"host suffix", []string{"https://github.com.evil.example.com/file"}, "host \"github.com.evil.example.com\""},
		{"other github subdomain", []string{"https://untrusted.githubusercontent.com/file"}, "host \"untrusted.githubusercontent.com\""},
		{"http github", []string{"http://github.com/file"}, "want HTTPS"},
		{"http hub", []string{"http://hub.example.com/file"}, "want HTTPS"},
		{"http asset", []string{"https://github.com/file", "http://objects.githubusercontent.com/file"}, "want HTTPS"},
		{"userinfo", []string{"https://user@github.com/file"}, "without userinfo"},
		{"four redirects", []string{base + "/1", base + "/2", base + "/3", base + "/4"}, ""},
		{"five redirects", []string{base + "/1", base + "/2", base + "/3", base + "/4", "https://github.com/5"}, "too many redirects"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := httpClient(base)
			calls := 0
			client.Transport = releaseTransport(func(r *http.Request) (*http.Response, error) {
				i := calls
				calls++
				wantURL := base + path
				if i > 0 {
					wantURL = tc.chain[i-1]
				}
				if r.URL.String() != wantURL {
					t.Fatalf("requested %s, want %s", r.URL, wantURL)
				}
				resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader("msi")), Request: r}
				if i < len(tc.chain) {
					resp.StatusCode = http.StatusFound
					resp.Header.Set("Location", tc.chain[i])
				}
				return resp, nil
			})
			src := &HTTPSource{Base: base, Client: client}
			var data strings.Builder
			err := src.Download(context.Background(), "0.1.32", "longarm-windows-amd64.msi", &data, 3)
			if tc.want == "" {
				if err != nil || data.String() != "msi" || calls != len(tc.chain)+1 {
					t.Fatalf("download: %q, %v (%d requests)", data.String(), err, calls)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if calls != len(tc.chain) || data.Len() != 0 {
				t.Fatalf("refused target was requested: %d requests, %q", calls, data.String())
			}
			target, parseErr := http.NewRequest(http.MethodGet, tc.chain[len(tc.chain)-1], nil)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			// net/http's outer URL error names the redirect source. Our
			// inner error must identify the actual refused target.
			want := fmt.Sprintf("refusing a redirect to %s", target.URL.Redacted())
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("refused target not correctly reported: %v", err)
			}
		})
	}
}
