package main

import (
	"maps"
	"testing"
)

func TestParseLabelEdits(t *testing.T) {
	m, err := parseLabelEdits([]string{"pool=devbox", "gpu-", "zone=a,old-", "x="})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"pool": "devbox", "gpu": "", "zone": "a", "old": "", "x": ""}; !maps.Equal(m, want) {
		t.Fatalf("got %v, want %v", m, want)
	}
	for _, bad := range []string{"novalue", "a b=c", "-", "k=v,,"} {
		if _, err := parseLabelEdits([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
