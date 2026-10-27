package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/diag"
	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/pki"
)

// fakeWorker builds a worker data dir full of things a bundle must not
// leak.
func fakeWorker(t *testing.T) string {
	t.Helper()
	dd := t.TempDir()
	ca, err := pki.InitCA(filepath.Join(t.TempDir(), "ca"), "Longarm CA", time.Hour*24*365)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := ca.Issue(pki.IssueOptions{Role: pki.RoleWorker, Name: "gpu-box-zeta"})
	if err != nil {
		t.Fatal(err)
	}
	if err := pki.WriteBundle(filepath.Join(dd, "tls"), ca.CertPEM, certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		p := filepath.Join(dd, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("longarmd.toml", `listen = ["tcp://127.0.0.1:7701"]
[access]
owners = ["mallory"]
[audit]
webhook = "https://hooks.example.net/services/T000/B000/SECRETHOOK"
webhook_token_file = "webhook.token"
`)
	write("webhook.token", "TOKEN-IN-A-FILE")
	write("hub.toml", "address = \"hub.corp-zeta.example:7443\"\npoll_token = \"POLLTOKEN123\"\n")
	write("trust/node.key", "-----BEGIN PRIVATE KEY-----\nTRUSTNODEKEY\n-----END PRIVATE KEY-----\n")
	write("trust/user.key", "-----BEGIN PRIVATE KEY-----\nTRUSTUSERKEY\n-----END PRIVATE KEY-----\n")
	write("ssh_host_ed25519_key", "SSHHOSTKEY")
	write("tsnet/tailscaled.state", "TSNETSTATE")
	write("update.json", `{"state":"healthy","version":"v1.2.3"}`)
	now := time.Now()
	logLine := func(at time.Time, msg string) string {
		return fmt.Sprintf("time=%s level=INFO msg=%q\n", at.Format(time.RFC3339Nano), msg)
	}
	write("longarmd.log", logLine(now.Add(-30*time.Hour), "OLD-LINE")+
		logLine(now.Add(-time.Hour), "job submitted by mallory from 198.51.100.40 password=hunter22 on gpu-box-zeta")+
		logLine(now.Add(-time.Minute), "hub hub.corp-zeta.example:7443 connected; Authorization: Bearer abcdefghijklmnopqrstu"))
	write("audit.log", `{"seq":1,"argv":["AUDITCMD"]}`+"\n")
	write("telemetry/crash-20261026.txt", "panic: boom at 203.0.113.1\n")
	// a job of mallory with a secret command line and output
	jd := jobs.Dir(filepath.Join(dd, "jobs", "J42"))
	os.MkdirAll(string(jd), 0o700)
	jd.WriteSpec(&jobs.Spec{ID: "J42", Argv: []string{"deploy-tool", "--api-key-xyz", "SECRETARG"}, Cwd: "/srv/project-zeta",
		Env: map[string]string{"DB_PASSWORD": "ENVSECRET"}, Owner: "mallory", CreatedAt: now.Add(-time.Hour)})
	exit := 3
	jd.WriteStatus(&jobs.Status{State: jobs.State("FAILED"), CreatedAt: now.Add(-time.Hour), StartedAt: now.Add(-time.Hour),
		EndedAt: now.Add(-50 * time.Minute), ExitCode: &exit})
	write("jobs/J42/out.log", "OUTPUT-LINE-1\nOUTPUT-SECRET\n")
	return dd
}

func zipFiles(t *testing.T, p string) map[string]string {
	t.Helper()
	zr, err := zip.OpenReader(p)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(b)
	}
	return out
}

func TestWorkerBundle(t *testing.T) {
	dd := fakeWorker(t)
	out := filepath.Join(t.TempDir(), "b.zip")
	b, _ := collectWorker(context.Background(), workerDiagOptions{dataDir: dd, since: 24 * time.Hour})
	if _, err := diag.Finish(context.Background(), b, diag.FinishOptions{Out: out, Stdout: io.Discard, Stderr: io.Discard}); err != nil {
		t.Fatal(err)
	}
	z := zipFiles(t, out)
	all := ""
	for n, c := range z {
		all += "\n=== " + n + "\n" + c
	}
	for _, bad := range []string{
		"PRIVATE KEY", "TRUSTNODEKEY", "TRUSTUSERKEY", "SSHHOSTKEY", "TSNETSTATE", "TOKEN-IN-A-FILE", "POLLTOKEN123", "SECRETHOOK",
		"hunter22", "abcdefghijklmnopqrstu", "mallory", "gpu-box-zeta", "corp-zeta", "198.51.100.40", "203.0.113.1",
		"SECRETARG", "deploy-tool", "ENVSECRET", "DB_PASSWORD", "project-zeta", "OUTPUT-SECRET", "AUDITCMD", "OLD-LINE",
	} {
		if strings.Contains(all, bad) {
			t.Errorf("bundle contains %q", bad)
		}
	}
	if h, _ := os.Hostname(); len(h) >= 3 && strings.Contains(strings.ToLower(all), strings.ToLower(h)) {
		t.Errorf("bundle contains the host name %q", h)
	}
	for _, want := range []string{"manifest.json", "system.json", "status.json", "logs/longarmd.log", "config/longarmd.toml", "config/hub.toml",
		"certs.json", "trust.json", "policy.json", "jobs.json", "state/update.json", "crash/crash-20261026.txt"} {
		if _, ok := z[want]; !ok {
			t.Errorf("%s missing (have %v)", want, keys(z))
		}
	}
	var m diag.Manifest
	if err := json.Unmarshal([]byte(z["manifest.json"]), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != len(z)-1 || m.Component != "longarmd" || !strings.HasPrefix(m.Node, "<node:") {
		t.Fatalf("manifest %+v", m)
	}
	excluded := map[string]bool{}
	for _, e := range m.Excluded {
		excluded[e.Name] = true
	}
	for _, e := range []string{"tls/node.key", "trust/node.key", "trust/user.key", "ssh_host_ed25519_key", "audit.log", "tsnet*/"} {
		if !excluded[e] {
			t.Errorf("not recorded as excluded: %s (%v)", e, m.Excluded)
		}
	}
	if m.Scrub["config_secret"] < 3 || m.Scrub["ip"] < 2 || m.Scrub["user"] < 1 || m.Scrub["node"] < 1 {
		t.Errorf("scrub summary %v", m.Scrub)
	}
	// metadata survives
	if !strings.Contains(z["jobs.json"], `"J42"`) || !strings.Contains(z["jobs.json"], `"exit_code": 3`) || !strings.Contains(z["jobs.json"], "FAILED") {
		t.Errorf("jobs.json: %s", z["jobs.json"])
	}
	if !strings.Contains(z["certs.json"], `"role": "worker"`) || !strings.Contains(z["certs.json"], "issuer_fingerprint") || !strings.Contains(z["certs.json"], `"trust": "none"`) {
		t.Errorf("certs.json: %s", z["certs.json"])
	}
	if !strings.Contains(z["logs/longarmd.log"], "connected") || !strings.Contains(z["state/update.json"], "v1.2.3") {
		t.Errorf("log: %s", z["logs/longarmd.log"])
	}

	// --include-job and --include-audit add what they say, still scrubbed
	b, _ = collectWorker(context.Background(), workerDiagOptions{dataDir: dd, since: 24 * time.Hour, includeAudit: true, includeJobs: []string{"J42", "NOPE"}})
	var buf bytes.Buffer
	if err := b.WriteZip(&buf); err != nil {
		t.Fatal(err)
	}
	if f := b.File("jobs/J42/job.json"); f == nil || !strings.Contains(string(f.Data()), "SECRETARG") || !strings.Contains(string(f.Data()), "DB_PASSWORD") ||
		strings.Contains(string(f.Data()), "ENVSECRET") || strings.Contains(string(f.Data()), "mallory") {
		t.Errorf("included job: %v", f)
	}
	if f := b.File("jobs/J42/out.log"); f == nil || !strings.Contains(string(f.Data()), "OUTPUT-SECRET") {
		t.Errorf("included output: %v", f)
	}
	if f := b.File("audit/audit.log"); f == nil || !strings.Contains(string(f.Data()), "AUDITCMD") {
		t.Errorf("audit: %v", f)
	}
	if len(b.Errors()) == 0 || !strings.Contains(strings.Join(b.Errors(), " "), "NOPE") {
		t.Errorf("unknown job not reported: %v", b.Errors())
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
