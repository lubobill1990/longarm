package doctor

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestReportText(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	r := New("longarm", "0.1.3", now)
	r.Add(Check{ID: "signed-in", Title: "signed in", Status: OK, Detail: "as alice"},
		Check{ID: "hub", Title: "hub", Status: Fail, Detail: "hub:7443 unreachable: timeout", Fix: "longarm diag --upload"},
		Check{ID: "daemon", Title: "client daemon", Status: Warn, Detail: "not running", Fix: "longarm daemon start"},
		Check{ID: "firewall", Title: "firewall", Status: Skip, Detail: "not checked"})
	var b bytes.Buffer
	if err := r.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	want := "longarm doctor (0.1.3)\n" +
		"  ok    signed in      as alice\n" +
		"  FAIL  hub            hub:7443 unreachable: timeout; next: longarm diag --upload\n" +
		"  warn  client daemon  not running; next: longarm daemon start\n" +
		"  -     firewall       not checked\n" +
		"1 problem: run the command after \"next:\" on the FAIL line\n"
	if b.String() != want {
		t.Errorf("text:\n%s\nwant:\n%s", b.String(), want)
	}
	if r.OK || r.Failed() != 1 {
		t.Error("OK with a failure")
	}
	b.Reset()
	r.WriteJSON(&b)
	var back Report
	if err := json.Unmarshal(b.Bytes(), &back); err != nil || back.Schema != SchemaVersion || back.Tool != "longarm" || back.OK ||
		len(back.Checks) != 4 || back.Checks[1].Fix != "longarm diag --upload" || !back.Time.Equal(now) {
		t.Errorf("json: %v %s", err, b.String())
	}
	for _, k := range []string{`"schema": 1`, `"tool": "longarm"`, `"ok": false`, `"checks": [`, `"id": "hub"`, `"status": "fail"`, `"fix": "longarm diag --upload"`} {
		if !strings.Contains(b.String(), k) {
			t.Errorf("json without %s", k)
		}
	}
}

func TestCertCheck(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		left time.Duration
		st   Status
		fix  string
	}{
		{-time.Hour, Fail, "longarm login"},
		{5 * time.Hour, Warn, "longarm login"},
		{20 * 24 * time.Hour, OK, ""},
	} {
		got := CertCheck(now.Add(c.left), now, 72*time.Hour, "longarm login")
		if got.Status != c.st || got.Fix != c.fix || got.Data["not_after"] == nil {
			t.Errorf("%s: %+v", c.left, got)
		}
	}
	if c := CertCheck(now.Add(-time.Hour), now, time.Hour, "x"); !strings.HasPrefix(c.Detail, "expired on ") {
		t.Errorf("expired: %q", c.Detail)
	}
}
