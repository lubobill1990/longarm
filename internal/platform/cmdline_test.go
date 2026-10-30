package platform

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestFirewallFixCommand(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Program Files")
	longarm := filepath.Join(dir, "longarm.exe")
	longarmd := filepath.Join(dir, "longarmd.exe")
	exe, _ := CommandLine(longarm)
	prog, _ := CommandLine("x", longarmd)
	prog = prog[2:] // the quoting is CommandLine's (TestCommandLine)
	for _, c := range []struct{ longarm, program, want string }{
		{longarm, longarm, exe + ` firewall allow`},
		{longarm, longarmd, exe + ` firewall allow --program ` + prog},
		{"", longarmd, `longarm firewall allow --program ` + prog},
	} {
		if got, err := FirewallFixCommand(c.longarm, c.program); err != nil || got != c.want {
			t.Errorf("FirewallFixCommand(%q, %q) = %q, %v; want %q", c.longarm, c.program, got, err, c.want)
		}
	}
	if got, _ := FirewallFixCommand(longarm, longarm); runtime.GOOS == "windows" && !strings.HasPrefix(got, `& '`) {
		t.Errorf("a quoted path needs PowerShell's call operator: %s", got)
	}
	if got := FirewallAllowArgs(longarm, longarmd); len(got) != 4 || got[3] != longarmd {
		t.Errorf("FirewallAllowArgs: %q", got)
	}
	bad := filepath.Join(dir, "a\nb", "longarm.exe")
	if _, err := FirewallFixCommand(longarm, bad); !errors.Is(err, ErrUnsupportedChars) {
		t.Errorf("newline in the path: %v", err)
	}
	if got := FirewallFixText(longarm, bad); !strings.Contains(got, "unsupported characters") || strings.Contains(got, "\n") {
		t.Errorf("FirewallFixText: %q", got)
	}
}

// TestCommandLine: the commands people paste (PowerShell on Windows, often
// elevated). Paths come from the file system: any character a directory
// name can have must stay inside its argument.
func TestCommandLine(t *testing.T) {
	for _, c := range []struct {
		goos, exe string
		args      []string
		want      string
	}{
		// the user's report: a quoted path needs `& ` in PowerShell
		{"windows", `C:\Program Files\Longarm\longarm.exe`, []string{"firewall", "allow"}, `& 'C:\Program Files\Longarm\longarm.exe' firewall allow`},
		{"windows", `C:\Users\me\tmia-desktop-live\longarm-1.2.3\longarm.exe`, []string{"firewall", "allow"}, `C:\Users\me\tmia-desktop-live\longarm-1.2.3\longarm.exe firewall allow`},
		{"windows", "longarm", []string{"firewall", "allow", "--program", `C:\Program Files\Longarm\longarmd.exe`}, `longarm firewall allow --program 'C:\Program Files\Longarm\longarmd.exe'`},
		{"windows", `C:\a b\$x\longarm.exe`, nil, `& 'C:\a b\$x\longarm.exe'`},
		{"windows", "C:\\a`b\\longarm.exe", nil, "& 'C:\\a`b\\longarm.exe'"},
		{"windows", `C:\it's\longarm.exe`, []string{""}, `& 'C:\it''s\longarm.exe' ''`},
		{"windows", `C:\x(86);y\longarm.exe`, nil, `& 'C:\x(86);y\longarm.exe'`},
		// Unicode quotes: PowerShell's single quotes are doubled, its double
		// quotes are plain characters inside single quotes
		{"windows", "C:\\a\u2018b\u2019c\u201Ad\u201Be\\longarm.exe", nil, "& 'C:\\a\u2018\u2018b\u2019\u2019c\u201A\u201Ad\u201B\u201Be\\longarm.exe'"},
		{"windows", "C:\\x\u201D; calc; #\\longarm.exe", nil, "& 'C:\\x\u201D; calc; #\\longarm.exe'"},
		{"windows", "C:\\x\u2019; calc; #\\longarm.exe", nil, "& 'C:\\x\u2019\u2019; calc; #\\longarm.exe'"},
		{"windows", "longarm", []string{"--%"}, `longarm '--%'`},
		{"linux", "/usr/local/bin/longarm", []string{"firewall", "allow"}, "/usr/local/bin/longarm firewall allow"},
		{"darwin", "/Users/me/my apps/longarm", []string{"--program", "it's"}, `'/Users/me/my apps/longarm' --program 'it'\''s'`},
	} {
		if got, err := commandLine(c.goos, c.exe, c.args...); err != nil || got != c.want {
			t.Errorf("%s %q %q:\n got %s (%v)\nwant %s", c.goos, c.exe, c.args, got, err, c.want)
		}
	}
	for _, s := range []string{"a\nb", "a\rb", "a\x00b", "a\tb", "a\u2028b", "a\u0085b", "a\x1bb"} {
		for _, goos := range []string{"windows", "linux"} {
			if _, err := commandLine(goos, `C:\x\longarm.exe`, "--program", s); !errors.Is(err, ErrUnsupportedChars) {
				t.Errorf("%s %q: %v, want ErrUnsupportedChars", goos, s, err)
			}
		}
	}
}

// hostilePaths are directory names that must stay one argument.
var hostilePaths = []string{
	`C:\Program Files\Longarm\longarm.exe`,
	`C:\a;b\longarm.exe`,
	`C:\a b\$env:TEMP\longarm.exe`,
	`C:\a$(calc)\longarm.exe`,
	"C:\\a`$x`n\\longarm.exe",
	`C:\it's\longarm.exe`,
	`C:\a"; calc; #\longarm.exe`,
	"C:\\a\u201C; calc; #\\longarm.exe",
	"C:\\a\u201D; calc; #\\longarm.exe",
	"C:\\a\u201E; calc; #\\longarm.exe",
	"C:\\a\u2018; calc; #\\longarm.exe",
	"C:\\a\u2019; calc; #\\longarm.exe",
	"C:\\a\u201A; calc; #\\longarm.exe",
	"C:\\a\u201B; calc; #\\longarm.exe",
	"C:\\a'\u2019\u2018'; calc; #\\longarm.exe",
	`C:\a & calc\longarm.exe`,
	`C:\a | calc\longarm.exe`,
	`C:\a{1}@(2),3\longarm.exe`,
	"C:\\a\u2013b \u2014c\\longarm.exe", // en/em dashes (PowerShell's -)
	`-file`,                             // arguments only
	`--%`,
	`--%x`,
}

// TestCommandLinePowerShellParser parses the generated lines with
// PowerShell's own parser (pwsh; windows-latest has it): one statement, one
// command, every element a constant string equal to the original - no
// variable, subexpression or second command.
func TestCommandLinePowerShellParser(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		if os.Getenv("CI") != "" && runtime.GOOS == "windows" {
			t.Fatal("pwsh not found on the Windows runner")
		}
		t.Skip("pwsh not installed")
	}
	type tc struct {
		Line string
		Want []string
	}
	var cases []tc
	for _, p := range hostilePaths {
		argvs := [][]string{{"longarm", "firewall", "allow", "--program", p}}
		if !strings.HasPrefix(p, "-") { // as the executable too (a "-..." one is refused)
			argvs = append(argvs, []string{p, "firewall", "allow"})
		}
		for _, argv := range argvs {
			line, err := commandLine("windows", argv[0], argv[1:]...)
			if err != nil {
				t.Fatalf("%q: %v", argv, err)
			}
			cases = append(cases, tc{line, argv})
		}
	}
	in, _ := json.Marshal(cases)
	f := filepath.Join(t.TempDir(), "cases.json")
	if err := os.WriteFile(f, in, 0o600); err != nil {
		t.Fatal(err)
	}
	script := `$ErrorActionPreference = 'Stop'
$cases = Get-Content -Raw -Encoding utf8 $env:LA_CASES | ConvertFrom-Json
$out = foreach ($c in $cases) {
  $toks = $null; $errs = $null
  $ast = [System.Management.Automation.Language.Parser]::ParseInput($c.Line, [ref]$toks, [ref]$errs)
  $cmds = @($ast.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] }, $true))
  $els = @()
  if ($cmds.Count -eq 1) {
    $els = @($cmds[0].CommandElements | ForEach-Object {
      if ($_ -is [System.Management.Automation.Language.StringConstantExpressionAst]) { $_.Value }
      elseif ($_ -is [System.Management.Automation.Language.CommandParameterAst] -and $null -eq $_.Argument) { $_.Extent.Text }
      else { '<' + $_.GetType().Name + '>' }
    })
  }
  [pscustomobject]@{ Line = $c.Line; Errors = @($errs).Count; Statements = $ast.EndBlock.Statements.Count; Commands = $cmds.Count; Elements = $els }
}
ConvertTo-Json -InputObject @($out) -Depth 4 -EscapeHandling EscapeNonAscii | Set-Content -Encoding utf8 $env:LA_OUT`
	outFile := filepath.Join(t.TempDir(), "out.json")
	cmd := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), "LA_CASES="+f, "LA_OUT="+outFile)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pwsh: %v\n%s", err, b)
	}
	b, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	var got []struct {
		Line                         string
		Errors, Statements, Commands int
		Elements                     []string
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(string(b), "\ufeff")), &got); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	if len(got) != len(cases) {
		t.Fatalf("pwsh parsed %d lines, want %d", len(got), len(cases))
	}
	for i, g := range got {
		if g.Line != cases[i].Line {
			t.Errorf("line %d came back as %q, sent %q", i, g.Line, cases[i].Line)
		}
		if g.Errors != 0 || g.Statements != 1 || g.Commands != 1 || !reflect.DeepEqual(g.Elements, cases[i].Want) {
			t.Errorf("%s\n  errors %d, statements %d, commands %d, elements %q; want 0, 1, 1, %q",
				g.Line, g.Errors, g.Statements, g.Commands, g.Elements, cases[i].Want)
		}
	}
}
