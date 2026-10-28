package sched

import (
	"fmt"
	"regexp"
	"strings"
)

// Node labels are scheduler constraints (longarm run --require region=eu). Keys
// and values are short tokens: a value can't hold "," or "=", so a label
// list like "region=eu,os=windows" can never end up as one label.
var (
	labelKeyRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$`)
	labelValueRe = regexp.MustCompile(`^[A-Za-z0-9._:/@+-]{0,128}$`)
)

// AdminLabel marks an admin worker (AdminLabel=true): a worker that runs
// commands with administrator rights on its machine (Windows: a longarmd
// running as LocalSystem, the MSI's ADMIN_WORKER). Only the hub sets it,
// when it enrolls an admin worker; nobody can set, change or remove it
// (ReservedLabel). Such a worker is never chosen by placement (Pick): it is
// reachable by its name only (docs/security.md "Admin worker (Windows)").
const AdminLabel = "longarm.io/admin"

// reservedLabelPrefix: label keys only the hub sets.
const reservedLabelPrefix = "longarm.io/"

// ReservedLabel reports whether k is a hub-set label key (longarm.io/...)
// that users, admins, join codes and sign-ins may not set or remove.
func ReservedLabel(k string) bool { return strings.HasPrefix(strings.ToLower(k), reservedLabelPrefix) }

// IsAdminWorker reports whether labels mark an admin worker.
func IsAdminWorker(labels map[string]string) bool { return labels[AdminLabel] == "true" }

// CheckUserLabels refuses reserved label keys (ReservedLabel) in labels a
// user, an admin, a join code or a sign-in asks for.
func CheckUserLabels(m map[string]string) error {
	for k := range m {
		if ReservedLabel(k) {
			return fmt.Errorf("label %s is reserved: only the hub sets %s* labels", k, reservedLabelPrefix)
		}
	}
	return nil
}

// ValidLabel checks one label; an empty value is allowed only with
// allowEmpty (SetLabels: "key=" deletes the label).
func ValidLabel(k, v string, allowEmpty bool) error {
	if !labelKeyRe.MatchString(k) {
		return fmt.Errorf("label key %q: want letters, digits, '.', '_', '/', '-' (max 63, starting with a letter or digit)", k)
	}
	if v == "" && !allowEmpty {
		return fmt.Errorf("label %q has an empty value", k)
	}
	if !labelValueRe.MatchString(v) {
		return fmt.Errorf("label %s=%q: the value may only hold letters, digits and . _ : / @ + - (max 128)", k, v)
	}
	return nil
}

// ValidateLabels checks a whole label map (see ValidLabel).
func ValidateLabels(m map[string]string, allowEmpty bool) error {
	for k, v := range m {
		if err := ValidLabel(k, v, allowEmpty); err != nil {
			return err
		}
	}
	return nil
}

// ParseLabels parses KEY=VALUE labels from command-line arguments: each
// argument may hold a comma-separated list ("region=eu,os=windows"), and
// arguments may repeat (--labels region=eu --labels os=windows). Malformed
// items (no "=", empty key, empty items, a key given twice with different
// values, invalid characters) are errors. allowEmpty permits "key=" (used
// to delete a label).
func ParseLabels(args []string, allowEmpty bool) (map[string]string, error) {
	if len(args) == 0 {
		return nil, nil
	}
	m := map[string]string{}
	for _, arg := range args {
		for _, item := range strings.Split(arg, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				return nil, fmt.Errorf("labels %q: empty item (want KEY=VALUE[,KEY=VALUE...])", arg)
			}
			k, v, ok := strings.Cut(item, "=")
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if !ok || k == "" {
				return nil, fmt.Errorf("label %q: want KEY=VALUE", item)
			}
			if err := ValidLabel(k, v, allowEmpty); err != nil {
				return nil, err
			}
			if old, dup := m[k]; dup && old != v {
				return nil, fmt.Errorf("label %q given twice (%q and %q)", k, old, v)
			}
			m[k] = v
		}
	}
	return m, nil
}
