package tray

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/updatesvc"
)

type fakeUpdateService struct {
	reqs []*updatesvc.Request
	resp func(*updatesvc.Request) *updatesvc.Response
}

func (f *fakeUpdateService) Do(_ context.Context, r *updatesvc.Request) (*updatesvc.Response, error) {
	f.reqs = append(f.reqs, r)
	return f.resp(r), nil
}

// TestUpdateServiceRoutes: the tray's own updates go through the Longarm
// Update service when the machine has it (installViaService), else the
// banner runs msiexec itself; automatic updates are the service's call.
func TestUpdateServiceRoutes(t *testing.T) {
	mux := http.NewServeMux()
	(&Server{}).updateServiceRoutes(mux)
	old := updateService
	defer func() { updateService = old }()
	do := func(method, path, body string) (int, map[string]any) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		var m map[string]any
		json.Unmarshal(w.Body.Bytes(), &m)
		return w.Code, m
	}

	updateService = func() serviceClient { return nil }
	if code, m := do("GET", "/trayapi/update/service", ""); code != 200 || m["service"] != false {
		t.Fatalf("no service: %d %v", code, m)
	}
	if handled, err := installViaService(context.Background(), "0.1.9"); handled || err != nil {
		t.Fatalf("no service: handled %v %v (the banner falls back to msiexec)", handled, err)
	}

	f := &fakeUpdateService{resp: func(r *updatesvc.Request) *updatesvc.Response {
		switch r.Op {
		case updatesvc.OpInstall:
			return &updatesvc.Response{OK: true, Phase: updatesvc.PhaseInstalling, From: "0.1.8", To: "0.1.9"}
		case updatesvc.OpSetAuto:
			return &updatesvc.Response{OK: false, Code: updatesvc.CodeDenied, Error: "only this machine's owner or a local administrator"}
		}
		return &updatesvc.Response{OK: true, Status: &updatesvc.Status{Installed: "0.1.8", Available: "0.1.9", State: "idle", AutoIntervalHours: 6}}
	}}
	updateService = func() serviceClient { return f }
	if code, m := do("GET", "/trayapi/update/service", ""); code != 200 || m["service"] != true || m["available"] != "0.1.9" {
		t.Fatalf("status: %d %v", code, m)
	}
	if handled, err := installViaService(context.Background(), "0.1.9"); !handled || err != nil {
		t.Fatalf("install through the service: %v %v", handled, err)
	}
	if r := f.reqs[len(f.reqs)-1]; r.Op != updatesvc.OpInstall || r.Version != "0.1.9" || r.Latest {
		t.Fatalf("install request %+v", r)
	}
	if code, m := do("POST", "/trayapi/update/auto", `{"auto":true}`); code != http.StatusForbidden || !strings.Contains(m["error"].(string), "owner") {
		t.Fatalf("auto refused by the service: %d %v", code, m)
	}
}
