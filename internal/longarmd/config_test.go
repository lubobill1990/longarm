package longarmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Invalid [hub] labels of an older longarmd.toml are dropped with a warning, not
// fatal (longarmd, longarmd leave and the uninstaller must still start).
func TestLoadConfigDropsInvalidLabels(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ConfigFile), []byte("[hub]\nlabels = { region = \"eu,os=windows\", gpu = \"RTX 4090\", pool = \"big\" }\n"), 0o600)
	cfg, err := LoadConfig(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Hub.Labels) != 1 || cfg.Hub.Labels["pool"] != "big" {
		t.Fatalf("labels %v", cfg.Hub.Labels)
	}
	if len(cfg.Warnings) != 2 || !strings.Contains(strings.Join(cfg.Warnings, "\n"), "ignored") {
		t.Fatalf("warnings %q", cfg.Warnings)
	}
}

// [access]: validated at load; a restricting policy turns the hub session's
// remote jobs off unless the file sets remote_jobs.
func TestLoadConfigAccess(t *testing.T) {
	load := func(toml string) (Config, error) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, ConfigFile), []byte(toml), 0o600)
		return LoadConfig(dir, nil)
	}
	cfg, err := load("")
	if err != nil || cfg.Access != nil || !cfg.HubJobDetails || cfg.Hub.RemoteJobs != "" {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	cfg, err = load("[access]\nusers = [\"bob\"]\nowners = [\"alice\"]\nadmins = \"observe\"\nhub_job_details = false\n")
	if err != nil || cfg.Access == nil || cfg.Access.Admins != "observe" || cfg.HubJobDetails || cfg.Hub.RemoteJobs != "none" {
		t.Fatalf("restricted: %+v %v", cfg, err)
	}
	if cfg, err = load("[hub]\nremote_jobs = \"control\"\n[access]\nadmins = \"none\"\n"); err != nil || cfg.Hub.RemoteJobs != "control" {
		t.Fatalf("explicit remote_jobs: %q %v", cfg.Hub.RemoteJobs, err)
	}
	if cfg, err = load("[access]\nowners = [\"alice\"]\n"); err != nil || cfg.Access == nil || cfg.Hub.RemoteJobs != "" {
		t.Fatalf("owners only: %+v %v", cfg, err)
	}
	for toml, want := range map[string]string{
		"[access]\nadmins = \"readonly\"\n": `[access] admins "readonly"`,
		"[access]\nusers = [\"Bob\"]\n":     `[access] users: invalid user name "Bob"`,
	} {
		if _, err := load(toml); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", toml, err, want)
		}
	}
}

func TestLoadConfigAudit(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ConfigFile), []byte("[audit]\nfile = \"copy.log\"\nwebhook = \"http://audit.example.com/\"\n"), 0o600)
	cfg, err := LoadConfig(dir, nil) // longarmd leave must still start
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Audit.File != filepath.Join(dir, "copy.log") {
		t.Fatalf("file %q", cfg.Audit.File)
	}
	if err := cfg.Audit.Validate(); err == nil || !strings.Contains(err.Error(), "https:// required") {
		t.Fatalf("validate: %v", err)
	}
}

// [links] max_conns: 64 by default, -1 = no cap (0 inside), other
// negatives are an error.
func TestLoadConfigLinkMaxConns(t *testing.T) {
	for in, want := range map[string]int{"": DefaultLinkMaxConns, "[links]\nmax_conns = 8\n": 8, "[links]\nmax_conns = -1\n": 0, "[links]\nmax_conns = -2\n": -2} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, ConfigFile), []byte(in), 0o600)
		cfg, err := LoadConfig(dir, nil)
		if want < 0 {
			if err == nil {
				t.Errorf("%q: no error", in)
			}
			continue
		}
		if err != nil || cfg.Links.MaxConns != want {
			t.Errorf("%q: %d %v, want %d", in, cfg.Links.MaxConns, err, want)
		}
	}
}
