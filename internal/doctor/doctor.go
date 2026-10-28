// Package doctor is the checklist of `longarm doctor` and `longarmd doctor`:
// each check says ok / warn / fail and, when it isn't ok, the exact command
// to run next. --json prints the same report for programs (the tray); its
// contract is documented in docs/usage.md#doctor---json.
package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// Status of one check.
type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn" // works, but something should be done
	Fail Status = "fail" // broken: Fix says what to run
	Skip Status = "skip" // not applicable here, or not checkable (Detail says why)
	Info Status = "info" // facts (paths, versions)
)

// Check is one line of the checklist.
type Check struct {
	// ID is stable for programs: "signed-in", "certificate", "hub",
	// "daemon", "firewall", "worker:<name>" (longarm); "signed-in",
	// "certificate", "running", "hub", "run-mode", "logs", "errors"
	// (longarmd).
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Fix is the exact next command when Status is warn or fail ("" when
	// there is nothing to run).
	Fix string `json:"fix,omitempty"`
	// Data carries machine-readable facts of the check (e.g. not_after,
	// last_seen, legs), all optional.
	Data map[string]any `json:"data,omitempty"`
}

// SchemaVersion of the --json report; additions keep it, changes bump it.
const SchemaVersion = 1

// Report is the whole checklist.
type Report struct {
	Schema  int       `json:"schema"`
	Tool    string    `json:"tool"` // "longarm" | "longarmd"
	Version string    `json:"version"`
	Time    time.Time `json:"time"`
	// OK: no check failed (warnings allowed).
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

// New starts a report.
func New(tool, version string, now time.Time) *Report {
	return &Report{Schema: SchemaVersion, Tool: tool, Version: version, Time: now.UTC().Truncate(time.Second), OK: true, Checks: []Check{}}
}

// Add appends checks.
func (r *Report) Add(cs ...Check) {
	for _, c := range cs {
		if c.Status == Fail {
			r.OK = false
		}
		r.Checks = append(r.Checks, c)
	}
}

// Failed counts the failing checks.
func (r *Report) Failed() int {
	n := 0
	for _, c := range r.Checks {
		if c.Status == Fail {
			n++
		}
	}
	return n
}

// WriteJSON writes the report as indented JSON.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// label is the status column of the text report.
func label(s Status) string {
	switch s {
	case Fail:
		return "FAIL"
	case Warn:
		return "warn"
	case Skip:
		return "-"
	case Info:
		return "info"
	}
	return "ok"
}

// Line is one check as text: "FAIL  hub  unreachable: ...; next: longarm login".
func Line(c Check) string {
	s := label(c.Status) + "\t" + c.Title + "\t" + c.Detail
	if c.Fix != "" && (c.Status == Fail || c.Status == Warn) {
		if c.Detail != "" {
			s += "; "
		}
		s += "next: " + c.Fix
	}
	return s
}

// WriteText writes the checklist for people.
func (r *Report) WriteText(w io.Writer) error {
	fmt.Fprintf(w, "%s doctor (%s)\n", r.Tool, r.Version)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range r.Checks {
		fmt.Fprintln(tw, "  "+strings.ReplaceAll(Line(c), "\n", " "))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	switch n := r.Failed(); n {
	case 0:
		_, err := fmt.Fprintln(w, "no problems found")
		return err
	case 1:
		_, err := fmt.Fprintln(w, "1 problem: run the command after \"next:\" on the FAIL line")
		return err
	default:
		_, err := fmt.Fprintf(w, "%d problems: run the commands after \"next:\" on the FAIL lines, top first\n", n)
		return err
	}
}

// Ago is a coarse duration for people ("45s", "12m", "3h", "2d").
func Ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// CertCheck is the "certificate" check of a client or worker certificate:
// fail when expired, warn when it ends within warnWithin (fix: renew).
func CertCheck(notAfter, now time.Time, warnWithin time.Duration, renew string) Check {
	c := Check{ID: "certificate", Title: "certificate", Data: map[string]any{"not_after": notAfter.UTC().Format(time.RFC3339)}}
	date := notAfter.Local().Format("2006-01-02 15:04")
	switch left := notAfter.Sub(now); {
	case left <= 0:
		c.Status, c.Detail, c.Fix = Fail, "expired on "+date, renew
	case left < warnWithin:
		c.Status, c.Detail, c.Fix = Warn, fmt.Sprintf("valid until %s (%s left; renewal is automatic while the hub is reachable)", date, Ago(left)), renew
	default:
		c.Status, c.Detail = OK, fmt.Sprintf("valid until %s (%s left)", date, Ago(left))
	}
	return c
}
