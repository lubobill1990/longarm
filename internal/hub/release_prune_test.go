package hub

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"longarm/internal/diag"
	"longarm/internal/hub/store"
	pb "longarm/proto/longarm/v1"
)

// Delete makes the test container a diag.Deleter.
func (m *memBlob) Delete(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.blobs, name)
	return nil
}

var _ diag.Deleter = (*memBlob)(nil)

func TestReleasesToPrune(t *testing.T) {
	var rels []store.Release
	add := func(v, ch string) {
		rels = append(rels, store.Release{Version: v, Channel: ch, File: "a"}, store.Release{Version: v, Channel: ch, File: "b"})
	}
	for _, v := range []string{"0.1.41-dev.118", "0.1.41-dev.120", "0.1.41-dev.9", "0.1.41-dev.119", "0.1.40-dev.116"} {
		add(v, "dev")
	}
	add("0.1.40-rc.1", "beta") // the old name of preview
	add("0.1.40-rc.2", "preview")
	add("0.1.37", "")
	add("0.1.38", "stable")
	keep := func(ch string) int { return map[string]int{"dev": 2, "preview": 1, "stable": 0}[ch] }

	got := releasesToPrune(rels, keep, nil)
	want := []string{"0.1.40-dev.116", "0.1.40-rc.1", "0.1.41-dev.9", "0.1.41-dev.118"}
	if !slices.Equal(got, want) {
		t.Errorf("prune %v, want %v (by version, not by name: dev.9 < dev.118)", got, want)
	}
	if got := releasesToPrune(rels, keep, map[string]bool{"0.1.41-dev.118": true}); slices.Contains(got, "0.1.41-dev.118") {
		t.Errorf("a protected version is pruned: %v", got)
	}
	none := func(string) int { return 0 }
	if got := releasesToPrune(rels, none, nil); len(got) != 0 {
		t.Errorf("keep 0 (all) prunes %v", got)
	}
}

func TestReleaseKeepConfig(t *testing.T) {
	zero, one := 0, 1
	c := &ReleaseConfig{KeepDev: &one, KeepStable: &zero}
	for ch, want := range map[string]int{"dev": 1, "preview": defaultKeepPreview, "beta": defaultKeepPreview, "stable": 0, "": 0, "nightly": 0} {
		if got := c.releaseKeep(ch); got != want {
			t.Errorf("keep %q = %d, want %d", ch, got, want)
		}
	}
}

// pruneReleases deletes old versions from the index, the data dir and the
// blob container; the rollout's version and the latest stable stay.
func TestPruneReleases(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	blob := newMemBlob()
	data := t.TempDir()
	rs, _ := testReleaseStore(t, data, blob)
	publish := func(v, ch string) {
		files := map[string]string{"longarmd-linux-amd64": "longarmd " + v, "longarmd-linux-amd64.minisig": "sig " + v}
		if _, err := addRelease(ctx, rs, st, releaseDir(t, files), v, ch, "", false); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []string{"0.2.0-dev.1", "0.2.0-dev.2", "0.2.0-dev.3", "0.2.0-dev.4", "0.2.0-dev.5"} {
		publish(v, "dev")
	}
	publish("0.1.0", "stable")
	publish("0.1.1", "stable") // latest_release

	two, one := 2, 1
	h := &Hub{cfg: &Config{DataDir: data, Releases: ReleaseConfig{KeepDev: &two, KeepStable: &one}},
		store: st, rel: rs, log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		rollout: &pb.Rollout{Version: "0.2.0-dev.1", Channel: "dev", State: rolloutDone}}
	h.pruneReleasesOnce(ctx)

	rels, _ := st.Releases()
	left := map[string]bool{}
	for _, r := range rels {
		left[r.Version] = true
	}
	for v, want := range map[string]bool{
		"0.2.0-dev.1": true, // the rollout's
		"0.2.0-dev.2": false, "0.2.0-dev.3": false,
		"0.2.0-dev.4": true, "0.2.0-dev.5": true,
		"0.1.0": false, "0.1.1": true,
	} {
		if left[v] != want {
			t.Errorf("%s in the index: %v, want %v", v, left[v], want)
		}
		_, err := os.Stat(filepath.Join(data, ReleasesDir, v))
		if (err == nil) != want {
			t.Errorf("%s in the data dir: %v, want %v", v, err == nil, want)
		}
		inBlob := slices.ContainsFunc(blob.names(), func(n string) bool { return strings.HasPrefix(n, v+"/") })
		if inBlob != want {
			t.Errorf("%s in the container: %v, want %v", v, inBlob, want)
		}
	}
	// a deleted version can be published again
	publish("0.2.0-dev.2", "dev")
	if rels, _ := st.Releases(); !slices.ContainsFunc(rels, func(r store.Release) bool { return r.Version == "0.2.0-dev.2" }) {
		t.Error("re-publish after retention")
	}
}

func TestReleaseKeepConfigLoad(t *testing.T) {
	t.Setenv(EnvReleasesKeepDev, "7")
	cfg := &Config{DataDir: t.TempDir()}
	if err := cfg.applyDefaults(); err != nil || cfg.Releases.releaseKeep("dev") != 7 || cfg.Releases.releaseKeep("stable") != defaultKeepStable {
		t.Fatalf("env: %v %v", cfg.Releases.KeepDev, err)
	}
	t.Setenv(EnvReleasesKeepDev, "")
	neg := -1
	cfg = &Config{DataDir: t.TempDir(), Releases: ReleaseConfig{KeepStable: &neg}}
	if err := cfg.applyDefaults(); err == nil || !strings.Contains(err.Error(), "keep_stable") {
		t.Fatalf("negative: %v", err)
	}
	t.Setenv(EnvReleasesKeepPreview, "many")
	if err := (&Config{DataDir: t.TempDir()}).applyDefaults(); err == nil || !strings.Contains(err.Error(), EnvReleasesKeepPreview) {
		t.Fatalf("not a number: %v", err)
	}
}

// The files of versions the index doesn't list (a deletion that failed
// half way, a cache download that ended after it) go in the next pass;
// indexed versions' files stay. A version a rollout took meanwhile isn't
// deleted.
func TestPruneSweepAndRolloutRecheck(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	blob := newMemBlob()
	data := t.TempDir()
	rs, _ := testReleaseStore(t, data, blob)
	files := map[string]string{"longarmd-linux-amd64": "x", "longarmd-linux-amd64.minisig": "sig"}
	if _, err := addRelease(ctx, rs, st, releaseDir(t, files), "1.0.0-dev.1", "dev", "", false); err != nil {
		t.Fatal(err)
	}
	// orphans: a data-dir copy and a blob of 0.9.0, nothing indexed
	if err := os.MkdirAll(filepath.Join(data, ReleasesDir, "0.9.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(data, ReleasesDir, "0.9.0", "longarmd-linux-amd64"), []byte("old"), 0o644)
	blob.Put(ctx, "0.9.0/longarmd-linux-amd64", strings.NewReader("old"), 3, nil)
	os.MkdirAll(filepath.Join(data, ReleasesDir, ".upload-123"), 0o755) // a publish staging its upload

	h := &Hub{cfg: &Config{DataDir: data}, store: st, rel: rs, log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	h.pruneReleasesOnce(ctx)
	if _, err := os.Stat(filepath.Join(data, ReleasesDir, "0.9.0")); err == nil {
		t.Error("orphan dir stays")
	}
	if slices.ContainsFunc(blob.names(), func(n string) bool { return strings.HasPrefix(n, "0.9.0/") }) {
		t.Error("orphan blob stays")
	}
	if _, err := os.Stat(filepath.Join(data, ReleasesDir, "1.0.0-dev.1", "longarmd-linux-amd64")); err != nil {
		t.Error("an indexed version's file went")
	}
	if _, err := os.Stat(filepath.Join(data, ReleasesDir, ".upload-123")); err != nil {
		t.Error("a publish's staging dir went")
	}

	// the rollout check under the version's lock
	h.rollout = &pb.Rollout{Version: "1.0.0-dev.1"}
	if deleted, err := h.pruneVersion("1.0.0-dev.1"); deleted || err != nil {
		t.Fatalf("deleted the rollout's version: %v %v", deleted, err)
	}
	if rels, _ := st.Releases(); len(rels) == 0 {
		t.Fatal("index emptied")
	}
}
