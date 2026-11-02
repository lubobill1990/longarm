package tray

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAddMachineInfo(t *testing.T) {
	for _, c := range []struct {
		name, tenant string
		internal     bool
		repo         string
	}{
		{"microsoft", "72F988BF-86F1-41AF-91AB-2D7CD011DB47", true, InternalRepo},
		{"other tenant", "11111111-2222-3333-4444-555555555555", false, PublicRepo},
		{"no entra", "", false, PublicRepo},
	} {
		t.Run(c.name, func(t *testing.T) {
			calls := 0
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				switch r.URL.Path {
				case "/auth/info":
					m := map[string]any{"entra": c.tenant != ""}
					if c.tenant != "" {
						m["tenant"] = c.tenant
					}
					_ = json.NewEncoder(w).Encode(m)
				case "/releases/latest.json":
					if r.URL.RawQuery != "" {
						t.Errorf("latest.json?%s: want the stable release", r.URL.RawQuery)
					}
					_, _ = io.WriteString(w, `{"version":"0.1.28"}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer hub.Close()
			s := &Server{opts: Options{HubURL: hub.URL + "/", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
			rec := httptest.NewRecorder()
			s.addMachineHandler(rec, httptest.NewRequest("GET", "/trayapi/addmachine", nil))
			var got addMachineInfo
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.HubHost != "127.0.0.1" || got.Version != "0.1.28" || got.Internal != c.internal || got.Repo != c.repo ||
				got.PublicRepo != PublicRepo || got.InternalRepo != InternalRepo {
				t.Fatalf("got %+v", got)
			}
			s.addMachineInfo(t.Context()) // cached: no more hub calls
			if calls != 2 {
				t.Fatalf("%d hub calls, want 2", calls)
			}
		})
	}
}

func TestAddMachineInfoNoHub(t *testing.T) {
	s := &Server{opts: Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	got := s.addMachineInfo(t.Context())
	if got.HubHost != "" || got.Version != "" || got.Repo != PublicRepo {
		t.Fatalf("got %+v", got)
	}
}
