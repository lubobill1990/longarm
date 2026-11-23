package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMacOSPostinstall runs the upgrade script with isolated paths and fake
// macOS tools: no installed services, firewall or preferences are touched.
func TestMacOSPostinstall(t *testing.T) {
	bash := findBash(t)
	src, err := os.ReadFile(filepath.Join("..", "packaging", "macos", "scripts", "postinstall"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, processType, fail, warning string
		installed, loaded, otherProgram  bool
		want                             string
	}{
		{name: "loaded old plist", installed: true, loaded: true, want: "add\nbootout\nbootstrap\n"},
		{name: "unloaded old plist", installed: true, want: "add\n"},
		{name: "already migrated", installed: true, loaded: true, processType: "Interactive", want: "kickstart\n"},
		{name: "explicit policy", installed: true, loaded: true, processType: "Background", want: "kickstart\n"},
		{name: "unloaded current plist", installed: true, processType: "Interactive"},
		{name: "no service"},
		{name: "other program", installed: true, loaded: true, otherProgram: true, want: "add\n"},
		{name: "add failure", installed: true, loaded: true, fail: "add", want: "add\nkickstart\n", warning: "could not add ProcessType"},
		{name: "bootout failure", installed: true, loaded: true, fail: "bootout", want: "add\nbootout\nbootstrap\n"},
		{name: "bootstrap failure", installed: true, loaded: true, fail: "bootstrap", want: "add\nbootout\n" + strings.Repeat("bootstrap\n", 5), warning: "could not reload"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tools := filepath.Join(root, "tools")
			if err := os.Mkdir(tools, 0o755); err != nil {
				t.Fatal(err)
			}
			write := func(path, contents string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			plist := filepath.Join(root, "com.longarm.longarmd.plist")
			if tc.installed {
				write(plist, tc.processType)
			}
			write(filepath.Join(tools, "PlistBuddy"), `#!/bin/sh
[ "$1" = -c ] && [ "$3" = "$TEST_ROOT/com.longarm.longarmd.plist" ] || exit 2
case $2 in
'Print :ProcessType') [ -s "$3" ] ;;
'Add :ProcessType string Interactive')
	echo add >> "$TEST_ROOT/actions"
	[ "$TEST_FAIL" != add ] || exit 1
	echo Interactive > "$3"
	;;
*) exit 2 ;;
esac
`)
			write(filepath.Join(tools, "launchctl"), `#!/bin/sh
case $1 in
print)
	[ "$2" = system/com.longarm.longarmd ] && [ "$TEST_LOADED" = true ] || exit 1
	if [ "$TEST_OTHER" = true ]; then
		echo 'program = /other/longarmd'
	else
		echo 'program = /usr/local/bin/longarmd'
	fi
	;;
bootout|kickstart)
	if [ "$1" = kickstart ]; then
		[ "$2" = -k ] && [ "$3" = system/com.longarm.longarmd ] || exit 2
	else
		[ "$2" = system/com.longarm.longarmd ] || exit 2
	fi
	echo "$1" >> "$TEST_ROOT/actions"
	[ "$TEST_FAIL" != "$1" ]
	;;
bootstrap)
	[ "$2" = system ] && [ "$3" = "$TEST_ROOT/com.longarm.longarmd.plist" ] || exit 2
	echo bootstrap >> "$TEST_ROOT/actions"
	[ "$TEST_FAIL" != bootstrap ]
	;;
*) exit 2 ;;
esac
`)
			script := strings.NewReplacer(
				"PATH=/usr/bin:/bin:/usr/sbin:/sbin", `PATH="$TEST_ROOT/tools:/usr/bin:/bin:/usr/sbin:/sbin"`,
				"/usr/libexec/PlistBuddy", `"$TEST_ROOT/tools/PlistBuddy"`,
				"/Library/LaunchDaemons", bashPath(root),
				"/Library/Preferences/com.longarm", bashPath(filepath.Join(root, "no-preferences")),
				"/Library/Managed Preferences/com.longarm", bashPath(filepath.Join(root, "no-managed-preferences")),
				"/usr/libexec/ApplicationFirewall/socketfilterfw", bashPath(filepath.Join(root, "no-firewall")),
			).Replace(string(src))
			path := filepath.Join(root, "postinstall")
			write(path, script)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bash, bashPath(path))
			cmd.Env = append(os.Environ(),
				"TEST_ROOT="+bashPath(root), "TEST_FAIL="+tc.fail,
				"TEST_LOADED="+strconv.FormatBool(tc.loaded), "TEST_OTHER="+strconv.FormatBool(tc.otherProgram))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("installation must not fail: %v\n%s", err, out)
			}
			if tc.warning != "" && !strings.Contains(string(out), tc.warning) {
				t.Fatalf("missing warning %q:\n%s", tc.warning, out)
			}
			actions, err := os.ReadFile(filepath.Join(root, "actions"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if string(actions) != tc.want {
				t.Fatalf("actions = %q, want %q\n%s", actions, tc.want, out)
			}
			if tc.installed {
				want := tc.processType
				if want == "" && tc.fail != "add" {
					want = "Interactive"
				}
				got, err := os.ReadFile(plist)
				if err != nil || strings.TrimSpace(string(got)) != want {
					t.Fatalf("ProcessType = %q, err = %v; want %q", got, err, want)
				}
			}
		})
	}
}
