package update

import "testing"

func TestChannels(t *testing.T) {
	for in, want := range map[string]string{"": "stable", "Stable": "stable", "beta": "preview", "preview": "preview", " dev ": "dev"} {
		if got, ok := NormalizeChannel(in); !ok || got != want {
			t.Errorf("NormalizeChannel(%q) = %q, %v", in, got, ok)
		}
	}
	if _, ok := NormalizeChannel("canary"); ok {
		t.Error("canary normalized")
	}
	for _, c := range []struct {
		sub, rel string
		want     bool
	}{
		{"stable", "stable", true}, {"stable", "preview", false}, {"stable", "dev", false},
		{"preview", "stable", true}, {"preview", "beta", true}, {"preview", "dev", false},
		{"dev", "stable", true}, {"dev", "preview", true}, {"dev", "dev", true},
		{"", "stable", true}, {"beta", "preview", true},
		{"canary", "canary", true}, {"canary", "stable", false}, {"dev", "canary", false},
	} {
		if got := ChannelIncludes(c.sub, c.rel); got != c.want {
			t.Errorf("ChannelIncludes(%q, %q) = %v", c.sub, c.rel, got)
		}
	}
}

// The versions the release workflow gives each channel sort in release
// order: dev builds of the next patch, then its release candidates, then
// the release; dev builds after a release candidate move to the patch after.
func TestChannelVersionOrder(t *testing.T) {
	order := []string{"0.1.20", "0.1.21-dev.5", "0.1.21-dev.17", "0.1.21-rc.1", "0.1.21-rc.2", "0.1.21", "0.1.22-dev.18"}
	for i := 1; i < len(order); i++ {
		if !NewerRelease(order[i], order[i-1]) {
			t.Errorf("%s is not newer than %s", order[i], order[i-1])
		}
	}
}
