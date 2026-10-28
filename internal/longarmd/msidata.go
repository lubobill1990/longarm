package longarmd

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/sched"
)

// The MSI's elevated custom actions (`longarmd install-helper msi <step>`)
// get their values as data, never as command-line options: Windows
// Installer formats public properties into the command line without any
// quoting, so a property such as HUB='x" --exe "C:\evil.exe' would add
// options of its own. The command line ends with MSIDataMarker followed by
// the step's fields, "key=value" separated by "|"; the helper takes
// everything after the first marker verbatim from the raw command line
// (GetCommandLineW) and parses it here:
//
//   - every key the step expects appears exactly once (the template has
//     them all, so an injected "|key=..." is always a duplicate) and no
//     other key appears;
//   - every value matches its key's strict pattern (no quotes, spaces,
//     "|" or other separators);
//   - nothing in the data names a program: the helper runs and registers
//     binaries only from its own, administrator-only directory.

// MSISteps are the install-helper msi steps and the fields each gets
// (packaging/windows/longarm.wxs passes exactly these).
var MSISteps = map[string][]string{
	"check":          {"installed", "usersid"},
	"setup":          {"datadir", "user", "hub", "code", "ca", "listen", "name", "isolation", "login", "usersid"},
	"rollback":       {"datadir"},
	"remove":         {"datadir", "keep"},
	"session":        {"sid", "usersid"},
	"session-join":   {"hub", "code", "ca", "name"},
	"session-remove": {"sid", "leave"},
	"launch-tray":    {"sid", "usersid"},
	"admin-setup":    {"hub", "ca", "sid", "usersid"},
	"admin-remove":   {"keep"},
	"admin-drop":     {"usersid"},
}

// MSIDataMarker starts the data on an install-helper msi command line.
const MSIDataMarker = "#LONGARM-MSI#"

var msiValue = map[string]*regexp.Regexp{
	// hub: host[:port], the bare domain the MSI documents
	"hub": regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?(:[0-9]{1,5})?)?$`),
	// ca: a sha256 fingerprint, hex with optional colons
	"ca":        regexp.MustCompile(`^([0-9A-Fa-f]{2}:?){0,32}$`),
	"sid":       regexp.MustCompile(`^(S-1-[0-9]+(-[0-9]+){1,14})?$`),
	"usersid":   regexp.MustCompile(`^(S-1-[0-9]+(-[0-9]+){1,14})?$`),
	"admin":     regexp.MustCompile(`^1?$`),
	"installed": regexp.MustCompile(`^[^|"\\]{0,64}$`),
	"code":      regexp.MustCompile(`^[A-Za-z0-9-]{0,32}$`),
	"name":      regexp.MustCompile(`^[A-Za-z0-9._-]{0,63}$`),
	"labels":    regexp.MustCompile(`^[A-Za-z0-9._/@:+,=-]{0,512}$`),
	"listen":    regexp.MustCompile(`^[A-Za-z0-9._:/?=&,\[\]%@-]{0,1000}$`),
	"isolation": regexp.MustCompile(`^(per-user)?$`),
	"login":     regexp.MustCompile(`^1?$`),
	"keep":      regexp.MustCompile(`^1?$`),
	"leave":     regexp.MustCompile(`^(1|keep)?$`),
	// a service account: DOMAIN\name, .\name, name@domain, LocalSystem
	"user": regexp.MustCompile(`^[A-Za-z0-9._@\\ -]{0,104}$`),
	// a data dir: checked further by MSIDataDir
	"datadir": regexp.MustCompile(`^[A-Za-z]:\\[A-Za-z0-9._\\ ()-]{0,240}$`),
}

// MSIData returns the fields after MSIDataMarker in raw (a command line),
// checked against keys (each exactly once, nothing else, every value
// matching its pattern).
func MSIData(raw string, keys ...string) (map[string]string, error) {
	i := strings.Index(raw, MSIDataMarker)
	if i < 0 {
		return nil, errors.New("no installer data on the command line")
	}
	data := strings.TrimRight(raw[i+len(MSIDataMarker):], " \t\r\n")
	want := map[string]bool{}
	for _, k := range keys {
		if msiValue[k] == nil {
			return nil, fmt.Errorf("internal: no pattern for %q", k)
		}
		want[k] = true
	}
	out := map[string]string{}
	for _, f := range strings.Split(data, "|") {
		if f == "" {
			continue
		}
		k, v, ok := strings.Cut(f, "=")
		if !ok || !want[k] {
			return nil, fmt.Errorf("unexpected installer data %q", trunc(f))
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("installer data %q given twice (a property with a separator in it?)", k)
		}
		if !msiValue[k].MatchString(v) {
			return nil, fmt.Errorf("installer property for %q is invalid: %q", k, trunc(v))
		}
		out[k] = v
	}
	for k := range want {
		if _, ok := out[k]; !ok {
			return nil, fmt.Errorf("installer data without %q", k)
		}
	}
	if v := out["ca"]; v != "" && len(pki.NormalizeFingerprint(v)) != 64 {
		return nil, fmt.Errorf("CA_FINGERPRINT %q is not a sha256 fingerprint", v)
	}
	if v := out["labels"]; v != "" {
		if _, err := sched.ParseLabels([]string{v}, false); err != nil {
			return nil, fmt.Errorf("LABELS: %w", err)
		}
	}
	if v := out["name"]; v != "" && !pki.ValidName(v) {
		return nil, fmt.Errorf("NODE_NAME %q is invalid", v)
	}
	return out, nil
}

func trunc(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// MSIDataDir checks a data dir the MSI passes: absolute, no "..", below
// the Longarm dir in ProgramData (the helper secures it as SYSTEM).
func MSIDataDir(dir, programDataLongarm string) (string, error) {
	if dir == "" {
		return "", errors.New("no data dir")
	}
	c := filepath.Clean(dir)
	if c != strings.TrimRight(dir, `\`) || strings.Contains(dir, "..") {
		return "", fmt.Errorf("data dir %q is not a clean path", dir)
	}
	base := filepath.Clean(programDataLongarm) + `\`
	if !strings.HasPrefix(strings.ToLower(c+`\`), strings.ToLower(base)) || len(c) <= len(base) {
		return "", fmt.Errorf("data dir %q must be below %s", dir, base)
	}
	return c, nil
}

// MSIRequester is who asked for the install (the MSI's UserSID, a private
// property the command line can't set) and the checks of the policy.
type MSIRequester struct {
	SID string
	// IsAdmin: the account is a local administrator (platform.IsLocalAdmin).
	IsAdmin func(sid string) (bool, error)
}

func (r MSIRequester) admin() error {
	if r.SID == "S-1-5-18" {
		return nil // LocalSystem: a deployment tool
	}
	if r.SID == "" || r.IsAdmin == nil {
		return errors.New("the installer did not say who asked for the install")
	}
	ok, err := r.IsAdmin(r.SID)
	if err != nil {
		return fmt.Errorf("checking that %s is an administrator: %w", r.SID, err)
	}
	if !ok {
		return fmt.Errorf("%s is not an administrator", r.SID)
	}
	return nil
}

// RequireAdmin: the step changes what runs as SYSTEM (a service, the admin
// worker) or, during maintenance (a repair or change, which Windows lets a
// non-administrator run elevated for a per-machine product), anything at
// all: only an administrator may ask for it.
func (r MSIRequester) RequireAdmin(what string) error {
	if err := r.admin(); err != nil {
		return fmt.Errorf("%s needs an administrator: %w", what, err)
	}
	return nil
}

// ForUser: a step acting for user sid (the user-session worker's task, the
// tray's start) may name the requester itself; another user only for an
// administrator.
func (r MSIRequester) ForUser(sid, what string) error {
	if sid == "" {
		return fmt.Errorf("%s: no user", what)
	}
	if strings.EqualFold(sid, r.SID) {
		return nil
	}
	return r.RequireAdmin(what + " for another user (" + sid + ")")
}
