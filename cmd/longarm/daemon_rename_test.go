package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The tray's Rename goes through POST /v1/workers/{name}/rename: names the
// hub would refuse are refused here already, before any hub call (the
// daemon here has none).
func TestRenameWorkerGatewayChecksName(t *testing.T) {
	d := &daemon{}
	mux := http.NewServeMux()
	d.poolGateway(mux)
	for body, want := range map[string]string{
		``:                        "invalid name",
		`{"new_name": "-x"}`:      "invalid name",
		`{"new_name": "a b"}`:     "invalid name",
		`{"newName": "deadbeef"}`: "looks like a node ID",
		`{"new_name": "id:box2"}`: "invalid name",
		`{"new_name": 12}`:        "",
		`{"new_name": "box2"`:     "",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/workers/box/rename", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
}
