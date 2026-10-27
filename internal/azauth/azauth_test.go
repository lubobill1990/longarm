package azauth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagedIdentity(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Metadata") != "true" || r.URL.Query().Get("client_id") != "cid" ||
			r.URL.Query().Get("resource") != ResourceMonitor {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, `{"access_token":"tok-%d","expires_on":"%d"}`, calls.Load(), time.Now().Add(time.Hour).Unix())
	}))
	defer srv.Close()
	m := &ManagedIdentity{ClientID: "cid", Endpoint: srv.URL}
	t.Setenv("IDENTITY_ENDPOINT", "")
	for range 3 {
		tok, err := m.Token(context.Background(), ResourceMonitor)
		if err != nil || tok != "tok-1" {
			t.Fatalf("token %q %v", tok, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("not cached: %d calls", calls.Load())
	}
	if _, err := m.Token(context.Background(), ResourceStorage); err == nil {
		t.Fatal("an error answer gave a token")
	}

	// an expiring token is fetched again
	m2 := &ManagedIdentity{ClientID: "cid", Endpoint: srv.URL}
	m2.cache = map[string]token{ResourceMonitor: {"old", time.Now().Add(time.Minute)}}
	if tok, _ := m2.Token(context.Background(), ResourceMonitor); tok == "old" {
		t.Fatal("a token about to expire was reused")
	}
	if tok, _ := Static("x").Token(context.Background(), ""); tok != "x" {
		t.Fatal("static")
	}
}
