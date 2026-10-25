package redact

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
)

func TestBytes(t *testing.T) {
	r := New([]string{"s3cr3t-token", "abc", "hunter22", "hunter22"})
	cases := map[string]string{
		"":                          "",
		"no secrets here":           "no secrets here",
		"tok=s3cr3t-token;":         "tok=***;",
		"s3cr3t-tokens3cr3t-token":  "******",
		"abc is too short to mask":  "abc is too short to mask",
		"pw hunter22 and hunter2 x": "pw *** and hunter2 x",
	}
	for in, want := range cases {
		if got := string(r.Bytes([]byte(in))); got != want {
			t.Errorf("Bytes(%q) = %q, want %q", in, got, want)
		}
	}
	var nilR *Redactor
	if string(nilR.Bytes([]byte("x"))) != "x" {
		t.Fatal("nil redactor must pass through")
	}
	if New([]string{"", "ab"}) != nil {
		t.Fatal("expected nil redactor for short values")
	}
}

func TestOverlappingSecretsLongestWins(t *testing.T) {
	r := New([]string{"abcd", "abcdefgh"})
	if got := string(r.Bytes([]byte("xabcdefghy abcdz"))); got != "x***y ***z" {
		t.Fatalf("got %q", got)
	}
}

// Every way of splitting the stream into chunks must yield the same output as
// redacting it in one go, and no secret may ever be written.
func TestWriterChunkBoundaries(t *testing.T) {
	secrets := []string{"ghp_ABCDEFGHIJ", "sk-ant-xyz123", "pass1234"}
	r := New(secrets)
	input := "start ghp_ABCDEFGHIJ mid sk-ant-xyz12 sk-ant-xyz123 ghp_ABC pass1234pass1234 end ghp_"
	want := string(r.Bytes([]byte(input)))
	if strings.Contains(want, "ghp_ABCDEFGHIJ") || !strings.HasSuffix(want, "end ghp_") {
		t.Fatalf("one-shot redaction wrong: %q", want)
	}
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 2000; iter++ {
		var out bytes.Buffer
		w := NewWriter(&out, r)
		rest := input
		for len(rest) > 0 {
			n := 1 + rng.Intn(6)
			if n > len(rest) {
				n = len(rest)
			}
			if _, err := w.Write([]byte(rest[:n])); err != nil {
				t.Fatal(err)
			}
			rest = rest[n:]
		}
		w.Close()
		if out.String() != want {
			t.Fatalf("iter %d: got %q want %q", iter, out.String(), want)
		}
	}
}

func TestWriterHoldsOnlyPossiblePrefix(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, New([]string{"SECRETVALUE"}))
	w.Write([]byte("hello SECR"))
	if out.String() != "hello " {
		t.Fatalf("expected prefix held back, got %q", out.String())
	}
	w.Write([]byte("X more"))
	if out.String() != "hello SECRX more" {
		t.Fatalf("got %q", out.String())
	}
	w.Write([]byte(" line\n"))
	if out.String() != "hello SECRX more line\n" {
		t.Fatalf("plain output must not be delayed, got %q", out.String())
	}
}

// A shorter secret that is a prefix of a longer one must not mask only the
// head of the longer one when it arrives split across writes.
func TestWriterPrefixSecretAcrossWrites(t *testing.T) {
	r := New([]string{"abcd", "abcdefgh"})
	for _, chunks := range [][]string{{"abcdef", "gh\n"}, {"abcd", "efgh\n"}, {"ab", "cdefg", "h\n"}, {"abcd", "x\n"}, {"abcdefg", "\n"}} {
		var out bytes.Buffer
		w := NewWriter(&out, r)
		for _, c := range chunks {
			w.Write([]byte(c))
		}
		w.Close()
		want := string(r.Bytes([]byte(strings.Join(chunks, ""))))
		if out.String() != want || strings.Contains(out.String(), "efgh") {
			t.Errorf("chunks %q: got %q want %q", chunks, out.String(), want)
		}
	}
}
