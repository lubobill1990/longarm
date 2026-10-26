package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func readAuditLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func verifyFile(t *testing.T, path string) AuditVerification {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	v, err := VerifyAudit(f)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func record(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("%v: %s", err, line)
	}
	return m
}

func hashOf(line string) string {
	s := sha256.Sum256([]byte(line))
	return hex.EncodeToString(s[:])
}

func TestAuditChainConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	a := NewAudit(path)
	var wg sync.WaitGroup
	for g := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 25 {
				a.LogAs(fmt.Sprintf("g%d", g), "test", map[string]any{"i": i})
			}
		}()
	}
	wg.Wait()
	lines := readAuditLines(t, path)
	if len(lines) != 500 {
		t.Fatalf("%d lines", len(lines))
	}
	if r := record(t, lines[0]); r["seq"] != 1.0 || r["prev"] != "" {
		t.Fatalf("first record %s", lines[0])
	}
	if r := record(t, lines[1]); r["seq"] != 2.0 || r["prev"] != hashOf(lines[0]) {
		t.Fatalf("second record %s", lines[1])
	}
	if v := verifyFile(t, path); v.Broken != nil || v.Records != 500 || len(v.Starts) != 0 || v.LinksOut {
		t.Fatalf("%+v", v)
	}
}

func TestAuditChainResumes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	a := NewAudit(path)
	for range 3 {
		a.LogAs("x", "test", nil)
	}
	a = NewAudit(path) // a restart
	a.LogAs("y", "test", nil)
	lines := readAuditLines(t, path)
	if r := record(t, lines[3]); r["seq"] != 4.0 || r["prev"] != hashOf(lines[2]) {
		t.Fatalf("not continued: %s", lines[3])
	}
	if v := verifyFile(t, path); v.Broken != nil || v.Records != 4 || len(v.Starts) != 0 {
		t.Fatalf("%+v", v)
	}
}

func TestAuditChainStartsAfterOldLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	old := `{"method":"a","time":"2026-01-01T00:00:00Z","who":"x"}` + "\n" + `{"method":"b","time":"2026-01-01T00:00:01Z","who":"x"}` + "\n"
	os.WriteFile(path, []byte(old), 0o600)
	NewAudit(path).LogAs("y", "test", nil)
	lines := readAuditLines(t, path)
	if len(lines) != 4 {
		t.Fatalf("%q", lines)
	}
	if r := record(t, lines[2]); r["method"] != AuditChainStart || r["seq"] != 1.0 || r["prev"] != hashOf(lines[1]) {
		t.Fatalf("chain start %s", lines[2])
	}
	if r := record(t, lines[3]); r["seq"] != 2.0 {
		t.Fatalf("record %s", lines[3])
	}
	if v := verifyFile(t, path); v.Broken != nil || v.Records != 2 || v.Unchained != 2 || len(v.Starts) != 1 || v.Starts[0] != 3 {
		t.Fatalf("%+v", v)
	}
}

func TestAuditIncompleteLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	NewAudit(path).LogAs("x", "test", nil)
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"seq":2,"prev":"ab`) // a crash mid-write
	f.Close()
	NewAudit(path).LogAs("y", "test", nil)
	lines := readAuditLines(t, path)
	if len(lines) != 4 {
		t.Fatalf("%q", lines)
	}
	if r := record(t, lines[2]); r["method"] != AuditChainStart || r["prev"] != hashOf(lines[1]) || r["reason"] != "incomplete last line" {
		t.Fatalf("chain start %s", lines[2])
	}
	if v := verifyFile(t, path); v.Broken != nil || v.Records != 3 || len(v.Starts) != 1 {
		t.Fatalf("%+v", v)
	}
}

func TestAuditNoticesOtherWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	a := NewAudit(path)
	a.LogAs("x", "test", nil)
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString("forged\n")
	f.Close()
	a.LogAs("x", "test", nil)
	lines := readAuditLines(t, path)
	if r := record(t, lines[2]); r["method"] != AuditChainStart || !strings.Contains(r["reason"].(string), "another writer") {
		t.Fatalf("not noticed: %s", lines[2])
	}
	if v := verifyFile(t, path); v.Broken != nil || len(v.Starts) != 1 {
		t.Fatalf("%+v", v)
	}
}

func TestAuditTruncatedRecordChained(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	a := NewAudit(path)
	a.LogAs("x", "test", nil)
	a.LogAs("x", "exec", map[string]any{"argv": strings.Repeat("A", 200<<10)})
	a.LogAs("x", "test", nil)
	lines := readAuditLines(t, path)
	if len(lines[1]) > maxAuditRecord || !strings.Contains(lines[1], `"truncated":`) {
		t.Fatalf("not truncated: %d bytes", len(lines[1]))
	}
	if r := record(t, lines[1]); r["seq"] != 2.0 || r["prev"] != hashOf(lines[0]) {
		t.Fatalf("truncated record not chained: %.200s", lines[1])
	}
	if v := verifyFile(t, path); v.Broken != nil || v.Records != 3 {
		t.Fatalf("%+v", v)
	}
}

func TestVerifyAuditDetectsTampering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	a := NewAudit(path)
	for i := range 6 {
		a.LogAs("alice", "test", map[string]any{"i": i})
	}
	lines := readAuditLines(t, path)
	check := func(name string, ls []string, want string) {
		t.Helper()
		v, err := VerifyAudit(strings.NewReader(strings.Join(ls, "\n") + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		if v.Broken == nil || !strings.HasPrefix(*v.Broken, want) {
			t.Errorf("%s: got %+v, want %q", name, v, want)
		}
	}
	edited := slicesClone(lines)
	edited[2] = strings.Replace(edited[2], "alice", "mallory", 1)
	check("edited", edited, "line 4 (seq 4): prev is not the hash of line 3")

	deleted := append(slicesClone(lines[:2]), lines[3:]...)
	check("deleted", deleted, "line 3 (seq 4): prev is not the hash of line 2")

	swapped := slicesClone(lines)
	swapped[2], swapped[3] = swapped[3], swapped[2]
	check("reordered", swapped, "line 3 (seq 4)")

	// A deleted line with the chain recomputed but seq not renumbered.
	resealed := append(slicesClone(lines[:2]), lines[3:]...)
	for i := 2; i < len(resealed); i++ {
		var m map[string]any
		json.Unmarshal([]byte(resealed[i]), &m)
		m["prev"] = hashOf(resealed[i-1])
		b, _ := json.Marshal(m)
		resealed[i] = string(b)
	}
	check("seq gap", resealed, "line 3 (seq 4): seq 4 follows 2")

	junk := append(slicesClone(lines[:3]), "junk")
	junk = append(junk, lines[3:]...)
	check("inserted", junk, "line 4 (seq 0): not a chained record")
}

func slicesClone(s []string) []string { return append([]string(nil), s...) }

func TestVerifyAuditPartialAndCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	a := NewAudit(path)
	for range 4 {
		a.LogAs("x", "test", nil)
	}
	b, _ := os.ReadFile(path)
	// A sink's copy starts mid-chain; a line being written is no break.
	i := bytes.IndexByte(b, '\n')
	v, err := VerifyAudit(bytes.NewReader(append(b[i+1:], `{"seq":5`...)))
	if err != nil || v.Broken != nil || !v.LinksOut || v.FirstSeq != 2 || !v.Partial || v.Records != 3 {
		t.Fatalf("%+v %v", v, err)
	}
}
