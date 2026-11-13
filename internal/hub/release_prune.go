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

// pruneReleases deletes the versions past the retention (see the top).
func (h *Hub) pruneReleases(ctx context.Context) {
	rels, err := h.store.Releases()
	if err != nil {
		h.log.Warn("releases: retention: reading the index", "err", err)
		return
	}
	protect := map[string]bool{}
	h.mu.Lock()
	if h.rollout != nil && h.rollout.Version != "" {
		protect[h.rollout.Version] = true
	}
	h.mu.Unlock()
	if v, err := h.store.Meta("latest_release"); err == nil && v != "" {
		protect[v] = true
	}
	for _, v := range releasesToPrune(rels, h.cfg.Releases.releaseKeep, protect) {
		var files []string
		var size int64
		for _, r := range rels {
			if r.Version == v {
				files = append(files, r.File)
				size += r.Size
			}
		}
		if err := h.rel.removeVersion(ctx, h.store, v, files); err != nil {
			h.log.Warn("releases: retention: deleting a version", "version", v, "err", err)
			continue
		}
		h.log.Info("releases: deleted an old version (retention)", "version", v, "files", len(files), "bytes", size)
	}
}

// removeVersion deletes version: its index rows, then its files in the
// data dir and (with blob_url) the container. Holds the version's lock,
// so a publish of the same version can't interleave.
func (s *releaseStore) removeVersion(ctx context.Context, st *store.Store, version string, files []string) error {
	if !versionRE.MatchString(version) {
		return errors.New("invalid version")
	}
	defer s.lockVersion(version)()
	if err := st.DeleteReleaseVersion(version); err != nil {
		return err
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
		for _, f := range files {
			for _, name := range []string{blobName(version, f), blobName(version, f+update.SigExt)} {
				if err := d.Delete(ctx, name); err != nil {
					errs = append(errs, err)
					continue
				}
				s.mu.Lock()
				delete(s.inBlob, name)
				s.mu.Unlock()
			}
		}
	}
	return errors.Join(errs...)
}
