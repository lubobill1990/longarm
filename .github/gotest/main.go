// Command gotest runs `go test -json` for CI and prints what plain `go test`
// would (failed tests in full, one line per passed top-level test), then
// adds to the job summary ($GITHUB_STEP_SUMMARY) the failed tests, the tests
// still running when a test binary died (a -timeout panic: its goroutine
// dump is printed with them) and the slowest tests; every failed test is
// also an error annotation. -timings writes each top-level test's duration
// ("<package> <test> <seconds>" lines) for the balancing of
// .github/scripts/test-shard.sh (.github/scripts/test-durations.sh).
//
//	go run ./.github/gotest [-title T] [-timings file] -- <go test arguments>
//
// The exit code is go test's.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// event is a test2json event (go doc test2json).
type event struct {
	Time       time.Time
	Action     string
	Package    string
	ImportPath string // build-output, build-fail
	Test       string
	Elapsed    float64
	Output     string
}

type test struct {
	pkg, name string
	out       strings.Builder
	done      bool
}

type result struct {
	pkg, name, action string
	elapsed           float64
}

func main() {
	title := flag.String("title", "go test", "heading in the job summary")
	timings := flag.String("timings", "", "write the top-level tests' durations to this file")
	flag.Parse()

	cmd := exec.Command("go", append([]string{"test", "-json"}, flag.Args()...)...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fatal(err)
	}
	if err := cmd.Start(); err != nil {
		fatal(err)
	}
	running := map[string]*test{} // pkg + " " + top-level test
	var order []string
	var results []result
	var failedPkgs []string
	timedOut := map[string]bool{}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var e event
		if json.Unmarshal(line, &e) != nil || e.Action == "" {
			fmt.Println(string(line)) // not an event (e.g. go's own output)
			continue
		}
		if e.Package == "" {
			e.Package = e.ImportPath
		}
		top, _, sub := strings.Cut(e.Test, "/")
		key := e.Package + " " + top
		switch {
		case e.Test == "":
			switch e.Action {
			case "output", "build-output":
				fmt.Print(e.Output)
				if strings.HasPrefix(e.Output, "panic: test timed out") {
					timedOut[e.Package] = true
				}
			case "fail", "build-fail":
				failedPkgs = append(failedPkgs, e.Package)
			}
			continue
		case e.Action == "run" && !sub:
			running[key] = &test{pkg: e.Package, name: top}
			order = append(order, key)
			continue
		}
		t := running[key]
		if t == nil {
			t = &test{pkg: e.Package, name: top}
			running[key] = t
			order = append(order, key)
		}
		switch e.Action {
		case "output":
			if o := strings.TrimSpace(e.Output); strings.HasPrefix(o, "=== ") {
				continue // RUN / PAUSE / CONT / NAME
			}
			if strings.HasPrefix(e.Output, "panic: test timed out") {
				timedOut[e.Package] = true
			}
			t.out.WriteString(e.Output)
		case "pass", "fail", "skip":
			if sub {
				if e.Action == "fail" {
					results = append(results, result{e.Package, e.Test, "fail", e.Elapsed})
				}
				continue
			}
			t.done = true
			results = append(results, result{e.Package, top, e.Action, e.Elapsed})
			if e.Action == "fail" {
				fmt.Print(t.out.String())
			} else {
				fmt.Printf("--- %s: %s (%.2fs)\n", strings.ToUpper(e.Action), top, e.Elapsed)
			}
			t.out.Reset()
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "gotest: reading go test's output:", err)
	}
	// Tests that never ended: the test binary died (a -timeout panic, a
	// crash). Their output holds the panic and the goroutine dump.
	var unfinished []*test
	for _, k := range order {
		if t := running[k]; !t.done {
			unfinished = append(unfinished, t)
			fmt.Printf("--- UNFINISHED: %s (%s; still running when the test binary ended)\n", t.name, t.pkg)
			fmt.Print(t.out.String())
		}
	}
	code := 0
	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			fatal(err)
		}
		code = ee.ExitCode()
	}

	for _, t := range unfinished {
		fmt.Printf("::error title=%s did not finish::%s %s was still running when the test binary ended (timeout or crash)\n", t.name, t.pkg, t.name)
	}
	for _, r := range results {
		if r.action == "fail" {
			fmt.Printf("::error title=%s failed::%s %s (%.1fs)\n", r.name, r.pkg, r.name, r.elapsed)
		}
	}
	if *timings != "" {
		var b strings.Builder
		for _, r := range results {
			if !strings.Contains(r.name, "/") && r.action != "skip" {
				fmt.Fprintf(&b, "%s %s %.2f\n", r.pkg, r.name, r.elapsed)
			}
		}
		if err := os.WriteFile(*timings, []byte(b.String()), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "gotest:", err)
		}
	}
	if p := os.Getenv("GITHUB_STEP_SUMMARY"); p != "" {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			summary(f, *title, code, results, unfinished, failedPkgs, timedOut)
			f.Close()
		}
	}
	os.Exit(code)
}

func summary(w io.Writer, title string, code int, results []result, unfinished []*test, failedPkgs []string, timedOut map[string]bool) {
	status := "passed"
	if code != 0 {
		status = fmt.Sprintf("**failed** (exit %d)", code)
	}
	passed := 0
	var failed, slow []result
	for _, r := range results {
		switch {
		case r.action == "fail":
			failed = append(failed, r)
		case r.action == "pass" && !strings.Contains(r.name, "/"):
			passed++
			slow = append(slow, r)
		}
	}
	fmt.Fprintf(w, "### %s: %s\n\n%d top-level tests passed, %d tests failed\n\n", title, status, passed, len(failed))
	if len(failed) > 0 {
		fmt.Fprintf(w, "| failed test | package | time |\n|---|---|---|\n")
		for _, r := range failed {
			fmt.Fprintf(w, "| `%s` | %s | %.1fs |\n", r.name, r.pkg, r.elapsed)
		}
		fmt.Fprintln(w)
	}
	for pkg := range timedOut {
		fmt.Fprintf(w, "**%s: test binary timed out** (goroutine dump in the log)\n\n", pkg)
	}
	if len(unfinished) > 0 {
		fmt.Fprintf(w, "Still running when the test binary ended:")
		for _, t := range unfinished {
			fmt.Fprintf(w, " `%s`", t.name)
		}
		fmt.Fprintf(w, "\n\n")
	}
	if len(failed) == 0 && len(failedPkgs) > 0 {
		fmt.Fprintf(w, "Failed packages (no failed test: build error or crash): %s\n\n", strings.Join(failedPkgs, ", "))
	}
	sort.Slice(slow, func(i, j int) bool { return slow[i].elapsed > slow[j].elapsed })
	if len(slow) > 8 {
		slow = slow[:8]
	}
	if len(slow) > 0 {
		fmt.Fprintf(w, "<details><summary>slowest tests</summary>\n\n")
		for _, r := range slow {
			fmt.Fprintf(w, "- `%s` %.1fs\n", r.name, r.elapsed)
		}
		fmt.Fprintf(w, "\n</details>\n\n")
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "gotest:", err)
	os.Exit(2)
}
