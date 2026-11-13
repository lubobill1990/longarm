package hub

// Release retention: a hub that CI publishes to gets a full release on
// every merge (dev: ~80 files, over a GB) and, without this, keeps them
// all until its disk fills. After each publish and at start the hub keeps
// the newest [releases] keep_dev / keep_preview / keep_stable versions of
// each channel (default 3, 3, 5; 0 = every version) and deletes the rest:
// the index first (nothing offers the version any more), then the files
// in the data dir and the blob container. Never deleted: the version of
// the current rollout (workers are told to fetch it) and the latest
// stable release (what install scripts and stable machines get).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"longarm/internal/diag"
	"longarm/internal/hub/store"
	"longarm/internal/update"
)

// Default retention per channel.
const (
	defaultKeepDev     = 3
	defaultKeepPreview = 3
	defaultKeepStable  = 5
)

// releaseKeep is how many versions of channel the hub keeps (0 = all).
func (c *ReleaseConfig) releaseKeep(channel string) int {
	pick := func(p *int, def int) int {
		if p == nil {
			return def
		}
		return max(*p, 0)
	}
	switch normChannel(channel) {
	case update.ChannelDev:
		return pick(c.KeepDev, defaultKeepDev)
	case update.ChannelPreview:
		return pick(c.KeepPreview, defaultKeepPreview)
	case update.ChannelStable:
		return pick(c.KeepStable, defaultKeepStable)
	}
	return 0 // a channel this hub doesn't know: keep
}

// releasesToPrune picks the versions to delete from the index rows rels:
// per channel, all but the newest keep(channel), never one in protect.
func releasesToPrune(rels []store.Release, keep func(channel string) int, protect map[string]bool) []string {
	chanOf := map[string]string{}
	for _, r := range rels {
		if _, ok := chanOf[r.Version]; !ok {
			chanOf[r.Version] = normChannel(r.Channel)
		}
	}
	byChan := map[string][]string{}
	for v, ch := range chanOf {
		byChan[ch] = append(byChan[ch], v)
	}
	var out []string
	for ch, vs := range byChan {
		n := keep(ch)
		if n <= 0 || len(vs) <= n {
			continue
		}
		slices.SortFunc(vs, func(a, b string) int { return update.Compare(b, a) }) // newest first
		for _, v := range vs[n:] {
			if !protect[v] {
				out = append(out, v)
			}
		}
	}
	slices.SortFunc(out, update.Compare)
	return out
}

// pruneReleasesOnce runs pruneReleases unless a pass is running already.
func (h *Hub) pruneReleasesOnce(ctx context.Context) {
	if !h.pruneMu.TryLock() {
		return
	}
	defer h.pruneMu.Unlock()
	h.pruneReleases(ctx)
}

// protectedRelease reports whether version must stay: the current
// rollout's (workers are told to fetch it) or the latest stable release.
func (h *Hub) protectedRelease(version string) bool {
	h.mu.Lock()
	r := h.rollout
	h.mu.Unlock()
	if r != nil && r.Version == version {
		return true
	}
	v, err := h.store.Meta("latest_release")
	return err == nil && v == version
}

// pruneReleases deletes the versions past the retention (see the top),
// then the files of versions the index doesn't have (sweepOrphans).
func (h *Hub) pruneReleases(ctx context.Context) {
	rels, err := h.store.Releases()
	if err != nil {
		h.log.Warn("releases: retention: reading the index", "err", err)
		return
	}
	protect := map[string]bool{}
	for _, r := range rels {
		if !protect[r.Version] && h.protectedRelease(r.Version) {
			protect[r.Version] = true
		}
	}
	for _, v := range releasesToPrune(rels, h.cfg.Releases.releaseKeep, protect) {
		var size int64
		n := 0
		for _, r := range rels {
			if r.Version == v {
				size += r.Size
				n++
			}
		}
		deleted, err := h.pruneVersion(v)
		if err != nil {
			h.log.Warn("releases: retention: deleting a version", "version", v, "err", err)
		}
		if deleted {
			h.log.Info("releases: deleted an old version (retention)", "version", v, "files", n, "bytes", size)
		}
	}
	if len(rels) > 0 { // an empty index: GitHub releases mode, whose cache is in the data dir
		h.sweepOrphans(ctx)
	}
}

// pruneVersion forgets version under its lock - unless a rollout of it
// began meanwhile (pushReleaseAs holds the same lock) - and deletes its
// files; false: it stays.
func (h *Hub) pruneVersion(version string) (bool, error) {
	if !versionRE.MatchString(version) {
		return false, errors.New("invalid version")
	}
	defer h.rel.lockVersion(version)()
	if h.protectedRelease(version) {
		return false, nil
	}
	if err := h.store.DeleteReleaseVersion(version); err != nil {
		return false, err
	}
	// the files: what fails here stays an orphan, which the sweep at the
	// end of this pass and of the next ones retries
	return true, h.rel.deleteFiles(context.Background(), version, nil)
}

// sweepOrphans deletes release files - in the data dir and the blob
// container - of versions the index doesn't list: left by a deletion
// that failed half way, or a cache download that ended after it. Each
// under the version's lock, checked against the index again there (a
// publish stores its files before it indexes them, under that lock).
func (h *Hub) sweepOrphans(ctx context.Context) {
	cands := map[string]bool{}
	if ents, err := os.ReadDir(h.rel.dir); err == nil {
		for _, e := range ents {
			if e.IsDir() && versionRE.MatchString(e.Name()) {
				cands[e.Name()] = true
			}
		}
	}
	var blobs map[string][]string // version -> blob names
	if h.rel.blob != nil {
		if _, ok := h.rel.blob.(diag.Deleter); ok {
			list, err := h.rel.blob.List(ctx, "")
			if err != nil {
				h.log.Warn("releases: retention: listing the blob container", "err", err)
			}
			blobs = map[string][]string{}
			for _, b := range list {
				v, _, ok := strings.Cut(b.Name, "/")
				if ok && versionRE.MatchString(v) {
					blobs[v] = append(blobs[v], b.Name)
					cands[v] = true
				}
			}
		}
	}
	if len(cands) == 0 {
		return
	}
	indexed := func() map[string]bool {
		rels, err := h.store.Releases()
		if err != nil {
			return nil
		}
		m := map[string]bool{}
		for _, r := range rels {
			m[r.Version] = true
		}
		return m
	}
	if idx := indexed(); idx == nil {
		return
	} else {
		for v := range idx {
			delete(cands, v)
		}
	}
	for v := range cands {
		func() {
			defer h.rel.lockVersion(v)()
			idx := indexed()
			if idx == nil || idx[v] {
				return // published meanwhile (or the index can't be read)
			}
			if err := h.rel.deleteFiles(ctx, v, blobs[v]); err != nil {
				h.log.Warn("releases: retention: deleting the files of an unindexed version", "version", v, "err", err)
				return
			}
			h.log.Info("releases: deleted the files of an unindexed version (retention)", "version", v)
		}()
	}
}

// deleteFiles deletes <data_dir>/releases/<version> and, with blob_url,
// the version's blobs (names: as listed; nil = list them). The caller holds
// the version's lock and has removed it from the index.
func (s *releaseStore) deleteFiles(ctx context.Context, version string, names []string) error {
	if !versionRE.MatchString(version) {
		return errors.New("invalid version")
	}
	var errs []error
	if err := os.RemoveAll(filepath.Join(s.dir, version)); err != nil {
		errs = append(errs, err)
	}
	if s.blob != nil {
		d, ok := s.blob.(diag.Deleter)
		if !ok {
			return errors.Join(append(errs, errors.New("the blob container can't delete; its blobs stay"))...)
		}
		if names == nil {
			list, err := s.blob.List(ctx, version+"/")
			if err != nil {
				return errors.Join(append(errs, err)...)
			}
			for _, b := range list {
				names = append(names, b.Name)
			}
		}
		for _, name := range names {
			if err := d.Delete(ctx, name); err != nil {
				errs = append(errs, err)
				continue
			}
			s.mu.Lock()
			delete(s.inBlob, name)
			s.mu.Unlock()
		}
	}
	return errors.Join(errs...)
}
