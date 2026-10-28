package longarmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The tests never touch HKLM: provenance lives in memory.
func init() { provenance = &memProvenance{} }

// TestMSIDataInjection: the MSI formats public properties into the custom
// actions' command lines unquoted. The helper reads the data after the
// marker only, each key exactly once, each value by its pattern: a
// property carrying quotes, options, separators or extra fields is refused,
// never parsed as an option (the HUB='x" --exe "C:\evil.exe" ...' repair).
func TestMSIDataInjection(t *testing.T) {
	line := func(hub, sid string) string {
		return `"C:\Program Files\Longarm\longarmd.exe" install-helper msi admin-setup ` + MSIDataMarker +
			"hub=" + hub + "|ca=|sid=" + sid + "|usersid=S-1-5-21-1-2-3-1001"
	}
	keys := MSISteps["admin-setup"]
	d, err := MSIData(line("longarm.example.com:7443", "S-1-5-21-1-2-3-1001"), keys...)
	if err != nil || d["hub"] != "longarm.example.com:7443" || d["sid"] != "S-1-5-21-1-2-3-1001" {
		t.Fatalf("plain data: %v %v", d, err)
	}
	for name, raw := range map[string]string{
		"quotes and options": line(`x" --exe "C:\Users\attacker\evil.exe" --hub "x`, "S-1-5-21-1-2-3-1001"),
		"an extra field":     line("x|sid=S-1-5-21-9-9-9-500", "S-1-5-21-1-2-3-1001"),
		"an unknown field":   line("x|exe=C:\\evil.exe", "S-1-5-21-1-2-3-1001"),
		"a space":            line("x y", "S-1-5-21-1-2-3-1001"),
		"a URL":              line("https://evil.example.com/", "S-1-5-21-1-2-3-1001"),
		"a bad SID":          line("longarm.example.com", `S-1-5-21-1"><Exec>`),
		"no marker":          strings.Replace(line("longarm.example.com", "S-1-5-21-1-2-3-1001"), MSIDataMarker, "", 1),
		"a missing field":    `x ` + MSIDataMarker + "hub=a|ca=|sid=S-1-5-21-1",
	} {
		if d, err := MSIData(raw, keys...); err == nil {
			t.Errorf("%s: accepted %v", name, d)
		}
	}
	// a second marker inside a value is just part of that value (refused)
	if _, err := MSIData(line("x"+MSIDataMarker+"hub=y", "S-1-5-21-1-2-3-1001"), keys...); err == nil {
		t.Error("a marker inside a value accepted")
	}
	// the other steps' patterns
	ok := map[string]string{"labels": "pool=devbox,os.flavor=ltsc", "listen": "tcp://127.0.0.1:7701,longnet://hub:7443?name=w1",
		"user": `.\alice`, "datadir": `C:\ProgramData\Longarm\longarmd`, "ca": strings.Repeat("ab", 32), "code": "K7P-Q2M", "leave": "keep"}
	for k, v := range ok {
		if _, err := MSIData("x "+MSIDataMarker+k+"="+v, k); err != nil {
			t.Errorf("%s=%s: %v", k, v, err)
		}
	}
	bad := map[string]string{"labels": "pool=dev box", "listen": `tcp://x" --y`, "user": `alice"`, "datadir": `\\server\share`,
		"ca": "abc", "code": "K7P Q2M", "leave": "all", "name": "../x"}
	for k, v := range bad {
		if _, err := MSIData("x "+MSIDataMarker+k+"="+v, k); err == nil {
			t.Errorf("%s=%s accepted", k, v)
		}
	}
}

func TestMSIDataDir(t *testing.T) {
	base := `C:\ProgramData\Longarm`
	if d, err := MSIDataDir(`C:\ProgramData\Longarm\longarmd`, base); err != nil || d != `C:\ProgramData\Longarm\longarmd` {
		t.Errorf("default: %q %v", d, err)
	}
	for _, bad := range []string{`C:\Users\attacker\data`, `C:\ProgramData\Longarm`, `C:\ProgramData\Longarm\..\x`, `C:\ProgramData\LongarmX\d`, ""} {
		if _, err := MSIDataDir(bad, base); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestMSIRequester: a repair or change, a service, the admin worker need
// an administrator; a user's own task does not, another user's does.
func TestMSIRequester(t *testing.T) {
	admins := map[string]bool{"S-1-5-21-1-2-3-500": true}
	isAdmin := func(sid string) (bool, error) {
		if sid == "S-1-5-21-1-2-3-404" {
			return false, errors.New("lookup failed")
		}
		return admins[sid], nil
	}
	user := MSIRequester{SID: "S-1-5-21-1-2-3-1001", IsAdmin: isAdmin}
	admin := MSIRequester{SID: "S-1-5-21-1-2-3-500", IsAdmin: isAdmin}
	system := MSIRequester{SID: "S-1-5-18", IsAdmin: isAdmin}
	if user.RequireAdmin("x") == nil || admin.RequireAdmin("x") != nil || system.RequireAdmin("x") != nil {
		t.Error("RequireAdmin")
	}
	if (MSIRequester{SID: "S-1-5-21-1-2-3-404", IsAdmin: isAdmin}).RequireAdmin("x") == nil || (MSIRequester{IsAdmin: isAdmin}).RequireAdmin("x") == nil {
		t.Error("unknown requester passed")
	}
	if user.ForUser("S-1-5-21-1-2-3-1001", "x") != nil || user.ForUser("S-1-5-21-1-2-3-1002", "x") == nil || admin.ForUser("S-1-5-21-1-2-3-1002", "x") != nil {
		t.Error("ForUser")
	}
}

// TestFreshSystemDir: a machine-wide data dir that was there before the
// install (any user may create it in ProgramData, with a CA and
// credentials of their own) is moved aside and starts empty, unless the
// installer recorded creating it and only administrators can still write
// it.
func TestFreshSystemDir(t *testing.T) {
	old := trustedDir
	t.Cleanup(func() { trustedDir = old })
	trusted := true
	trustedDir = func(string, ...string) error {
		if trusted {
			return nil
		}
		return errors.New("owned by attacker")
	}
	plant := func() string {
		dir := filepath.Join(t.TempDir(), "longarmd-admin")
		os.MkdirAll(filepath.Join(dir, TLSDir), 0o700)
		os.WriteFile(filepath.Join(dir, TLSDir, pki.CACertFile), []byte("attacker CA"), 0o600)
		return dir
	}
	planted := func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, TLSDir, pki.CACertFile))
		return err == nil
	}

	// the admin worker's: no record = not ours
	dir := plant()
	if err := FreshSystemDir(dir, true, nil); err != nil {
		t.Fatal(err)
	}
	if planted(dir) {
		t.Fatal("planted admin data dir kept")
	}
	if m, _ := filepath.Glob(dir + ".untrusted-*"); len(m) != 0 {
		t.Errorf("moved-aside copy left: %v", m)
	}
	// recorded and still protected: kept (an upgrade)
	dir = plant()
	RecordSystemDir(dir)
	if err := FreshSystemDir(dir, true, nil); err != nil || !planted(dir) {
		t.Fatalf("recorded dir: %v", err)
	}
	// recorded, but writable by somebody else now: moved aside
	trusted = false
	if err := FreshSystemDir(dir, true, nil); err != nil || planted(dir) {
		t.Fatalf("recorded but untrusted dir kept: %v", err)
	}
	// a service's data dir of an older installer: kept when protected
	trusted = true
	dir = plant()
	if err := FreshSystemDir(dir, false, nil); err != nil || !planted(dir) {
		t.Fatalf("protected legacy dir: %v", err)
	}
	trusted = false
	if err := FreshSystemDir(dir, false, nil); err != nil || planted(dir) {
		t.Fatalf("user-owned legacy dir kept: %v", err)
	}
	// a link instead of a dir: removed, its target untouched
	target := plant()
	link := filepath.Join(t.TempDir(), "d")
	if err := os.Symlink(target, link); err == nil {
		trusted = true
		if err := FreshSystemDir(link, false, nil); err != nil {
			t.Fatal(err)
		}
		if !planted(target) {
			t.Fatal("the link's target was removed")
		}
	}
}

// TestAdminProvenance: the admin worker serves only with the hub CA its
// own sign-in recorded; credentials planted before the install (no record)
// or another CA are refused.
func TestAdminProvenance(t *testing.T) {
	provenance = &memProvenance{}
	ca, err := pki.InitCA(t.TempDir(), "hub", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := pki.InitCA(t.TempDir(), "attacker", time.Hour)
	dir := t.TempDir()
	if err := verifyAdminProvenance(dir); err != nil {
		t.Fatalf("not signed in yet: %v", err)
	}
	os.MkdirAll(filepath.Join(dir, TLSDir), 0o700)
	os.WriteFile(filepath.Join(dir, TLSDir, pki.NodeCertFile), []byte("cert"), 0o600)
	os.WriteFile(filepath.Join(dir, TLSDir, pki.CACertFile), other.CertPEM, 0o600)
	if err := verifyAdminProvenance(dir); err == nil {
		t.Fatal("planted credentials without a record accepted")
	}
	provenance.Set(ProvAdminCA, pki.Fingerprint(ca.Cert))
	if err := verifyAdminProvenance(dir); err == nil {
		t.Fatal("another CA accepted")
	}
	os.WriteFile(filepath.Join(dir, TLSDir, pki.CACertFile), ca.CertPEM, 0o600)
	if err := verifyAdminProvenance(dir); err != nil {
		t.Fatalf("the recorded CA: %v", err)
	}
	if !IsAdminWorker(Config{ServiceName: AdminServiceName}) || IsAdminWorker(Config{ServiceName: "longarmd", DataDir: dir}) {
		t.Error("IsAdminWorker")
	}
}

// TestAdminIgnoresHub: the admin worker ignores what the hub starts by
// itself (toolchains, updates, port links, purges, relayed job requests),
// and keeps what only narrows.
func TestAdminIgnoresHub(t *testing.T) {
	for _, c := range []struct {
		cmd    *pb.HubCommand
		ignore bool
	}{
		{&pb.HubCommand{Msg: &pb.HubCommand_Toolchain{Toolchain: &pb.ToolchainDesired{Profile: "p"}}}, true},
		{&pb.HubCommand{Msg: &pb.HubCommand_Update{Update: &pb.UpdateHint{}}}, true},
		{&pb.HubCommand{Msg: &pb.HubCommand_PortLinks{PortLinks: &pb.PortLinks{}}}, true},
		{&pb.HubCommand{Msg: &pb.HubCommand_Tenancy{Tenancy: &pb.TenancyPolicy{Purge: []string{"bob"}}}}, true},
		{&pb.HubCommand{Msg: &pb.HubCommand_Tenancy{Tenancy: &pb.TenancyPolicy{}}}, false},
		{&pb.HubCommand{Msg: &pb.HubCommand_Drain{Drain: &pb.DrainCommand{Draining: true}}}, false},
		{&pb.HubCommand{Msg: &pb.HubCommand_Revocations{Revocations: &pb.Revocations{}}}, false},
	} {
		if got := adminIgnores(c.cmd); got != c.ignore {
			t.Errorf("%T: ignored %v, want %v", c.cmd.Msg, got, c.ignore)
		}
	}
	// relayed job requests (submit, kill, logs, toolchain apply): refused
	a := &hubAgent{admin: true, remoteJobs: "all"}
	if a.allowRemote("submit") || a.allowRemote("control") {
		t.Error("the admin worker allows relayed job requests")
	}
}

// TestApplyMSISettings: the sign-in defaults (where sign-ins go without a
// hub, which CA they pin) reach HKLM only through the helper: a first
// install records them; changing recorded values - a non-administrator's
// repair with HUB / CA_FINGERPRINT of their choosing (also as REMOVE=ALL
// REINSTALL=ALL) - needs an administrator and writes nothing otherwise.
func TestApplyMSISettings(t *testing.T) {
	isAdmin := func(sid string) (bool, error) { return sid == "S-1-5-21-1-2-3-500", nil }
	user := MSIRequester{SID: "S-1-5-21-1-2-3-1001", IsAdmin: isAdmin}
	admin := MSIRequester{SID: "S-1-5-21-1-2-3-500", IsAdmin: isAdmin}
	good := map[string]string{"hub": "longarm.example.com", "ca": "", "login": "1", "labels": "pool=devbox", "mode": "default"}
	s := &memSettings{}
	if err := ApplyMSISettings(s, good, user); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if v, _ := s.Get("Hub"); v != "longarm.example.com" {
		t.Fatalf("Hub %q", v)
	}
	if err := ApplyMSISettings(s, good, user); err != nil {
		t.Fatalf("a repair that changes nothing: %v", err)
	}
	evil := map[string]string{"hub": "evil.example.com", "ca": strings.Repeat("ee", 32), "login": "1", "labels": "pool=devbox", "mode": "custom"}
	if err := ApplyMSISettings(s, evil, user); err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("a non-admin's change: %v", err)
	}
	if v, _ := s.Get("Hub"); v != "longarm.example.com" {
		t.Fatalf("Hub changed to %q", v)
	}
	if v, _ := s.Get("CaFingerprint"); v != "" {
		t.Fatalf("CaFingerprint changed to %q", v)
	}
	if err := ApplyMSISettings(s, evil, admin); err != nil {
		t.Fatalf("an administrator's change: %v", err)
	}
	RemoveMSISettings(s)
	if _, ok := s.Get("Hub"); ok {
		t.Fatal("not removed")
	}
}
