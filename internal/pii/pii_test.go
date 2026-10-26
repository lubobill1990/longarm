package pii

import (
	"strings"
	"testing"
)

func TestScrub(t *testing.T) {
	s := New(Options{Salt: []byte("fleet-1"), NoLocal: true,
		Names: map[string]string{"alice": KindUser, "w1-desk": KindNode, "LAPTOP-7Q2": KindHost}})
	alice, w1 := s.Hash(KindUser, "alice"), s.Hash(KindNode, "w1-desk")
	if !strings.HasPrefix(alice, "<user:") || len(alice) != len("<user:abcdef>") || s.Hash(KindUser, "ALICE") != alice {
		t.Fatalf("hash %q", alice)
	}
	if New(Options{Salt: []byte("fleet-2"), NoLocal: true}).Hash(KindUser, "alice") == alice {
		t.Fatal("the salt doesn't change the hash")
	}
	cases := []struct{ in, want string }{
		// names, as words only
		{"job of alice on w1-desk failed", "job of " + alice + " on " + w1 + " failed"},
		{"alice,alice", alice + "," + alice},
		{"malice and alice2 stay", "malice and alice2 stay"},
		{"host LAPTOP-7Q2.corp", "host " + s.Hash(KindHost, "LAPTOP-7Q2") + ".corp"},
		// home directories
		{`open C:\Users\bob\proj\x.go: denied`, `open C:\Users\` + s.Hash(KindUser, "bob") + `\proj\x.go: denied`},
		{"/home/carol/.longarm/tls", "/home/" + s.Hash(KindUser, "carol") + "/.longarm/tls"},
		// secrets
		{"Authorization: Bearer abcdefghijklmnop", "Authorization: <secret>"},
		{`{"password": "hunter22"}`, `{"password": "<secret>"}`},
		{"token=abc123def&x=1", "token=<secret>&x=1"},
		{"key eyJhbGciOiJIUzI1.eyJzdWIiOiIxMjM0.SflKxwRJSMeKKF2QT4", "key <jwt>"},
		{"use ghp_0123456789abcdefghijABCDEFGHIJ now", "use <secret> now"},
		{"tskey-auth-kAbc123CNTRL-xyzxyzxyz", "<secret>"},
		{"-----BEGIN EC PRIVATE KEY-----\nMHc\n-----END EC PRIVATE KEY-----", "<private-key>"},
		{"join with H62-ZQV now", "join with <join-code> now"},
		{"SHA-256 and AES-128 stay", "SHA-256 and AES-128 stay"},
		// URLs, e-mail, IPs
		{"GET https://acct.blob.core.windows.net/c/b?sv=2021&sig=abc", "GET https://acct.blob.core.windows.net/c/b?<query>"},
		{"https://user:pw@example.com/x", "https://<userinfo>@example.com/x"},
		{"mail bob.smith@contoso.com", "mail <email>"},
		{"dial 10.92.0.5:7443 and 127.0.0.1:7480 and 0.0.0.0", "dial <ip>:7443 and 127.0.0.1:7480 and 0.0.0.0"},
		{"addr [2404:f801:10:128:b8a8:6cc6:6dc8:d9f4]:64965 ::1", "addr [<ip>]:64965 ::1"},
		{"at 12:30:45 v1.2.3", "at 12:30:45 v1.2.3"},
		// what must survive: versions, fingerprints, ids, durations
		{"ca 4eab5ef1e53cd6ef176a2ec8930f00d79dd31789254e41cb9b4aa9fdc022ca32 took 1.5s", "ca 4eab5ef1e53cd6ef176a2ec8930f00d79dd31789254e41cb9b4aa9fdc022ca32 took 1.5s"},
	}
	for _, c := range cases {
		if got := s.String(c.in); got != c.want {
			t.Errorf("%q\n got %q\nwant %q", c.in, got, c.want)
		}
	}
	var nilS *Scrubber
	if nilS.String("alice") != "alice" {
		t.Fatal("nil scrubber")
	}
	m := s.Map(map[string]string{"err": "alice@contoso.com"})
	if m["err"] != "<email>" {
		t.Fatal(m)
	}
}

// The local machine's names are known without being told.
func TestScrubLocal(t *testing.T) {
	s := New(Options{})
	for n, k := range s.kinds {
		if out := s.String("x " + n + " y"); strings.Contains(out, " "+n+" ") {
			t.Errorf("local %s %q not scrubbed: %q", k, n, out)
		}
	}
}

func TestScrubSecrets(t *testing.T) {
	hook := "https://hooks.example.net/services/T000/B000/SECRETCAPABILITY123"
	s := New(Options{NoLocal: true, Secrets: []string{hook, "short"}})
	if got := s.String(`Post "` + hook + `": dial tcp: timeout`); got != `Post "<secret>": dial tcp: timeout` {
		t.Fatalf("got %q", got)
	}
	if got := s.String("a short word"); got != "a short word" {
		t.Fatalf("short secret replaced: %q", got)
	}
}

func TestRegisterSecret(t *testing.T) {
	RegisterSecret("tok-registered-1234")
	if got := New(Options{NoLocal: true}).String("x tok-registered-1234 y"); got != "x <secret> y" {
		t.Fatalf("registered secret: %q", got)
	}
}
