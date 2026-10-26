package main

import (
	"reflect"
	"testing"
)

func TestUnitExecStart(t *testing.T) {
	unit := "[Unit]\nDescription=Longarm worker (longarmd)\n\n[Service]\nType=simple\n" +
		`ExecStart=/usr/bin/longarmd run --data-dir "/var/lib/long arm/a\"b\\c$$x%%y" --service-name longarmd` + "\n" +
		"Restart=always\n"
	args, user := unitExecStart(unit)
	want := []string{"/usr/bin/longarmd", "run", "--data-dir", `/var/lib/long arm/a"b\c$x%y`, "--service-name", "longarmd"}
	if !reflect.DeepEqual(args, want) || user != "" {
		t.Fatalf("args %q user %q", args, user)
	}
	if d := flagValue(args, "--data-dir"); d != want[3] {
		t.Fatalf("--data-dir %q", d)
	}
	if d := flagValue([]string{"x", "--data-dir=/d"}, "--data-dir"); d != "/d" {
		t.Fatalf("--data-dir= %q", d)
	}
	if _, user := unitExecStart("[Service]\nExecStart=/x run\nUser=alice\n"); user != "alice" {
		t.Fatalf("user %q", user)
	}
	if perUserLoopbackCleanup("longarmd-no-such-service-xyz") != nil {
		t.Fatal("cleanup for a missing unit")
	}
}
