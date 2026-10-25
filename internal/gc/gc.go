// Package gc removes old data from longarmd's data dir (P0b ops): terminal jobs and
// sessions past their retention (with their worktrees), more of them -
// oldest first - while the disk is low, stale staging uploads, orphaned
// worktrees, and mirrors unused for a long time when space is still short.
// Running jobs and sessions are never touched.
package gc

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/supervisor"
	"github.com/lubobill1990/longarm/internal/workspace"
)

// StagingMaxAge is how long an uploaded-but-unused staging dir is kept.
const StagingMaxAge = 24 * time.Hour

// Options for one collection.
type Options struct {
	Keep           time.Duration // retention of terminal jobs/sessions (0 = none kept, < 0 = no age pass)
	MinFreePercent float64       // <= 0: no disk-pressure pass
	DryRun         bool
}

// Collector knows what to collect.
type Collector struct {
	DataDir  string
	Jobs     *supervisor.Manager
	Sessions *supervisor.Manager
	WS       *workspace.Store
	Log      *slog.Logger
	// DiskUsage defaults to platform.DiskUsage (tests override it).
	DiskUsage func(path string) (free, total uint64, err error)
}

// Result of a collection.
type Result struct {
	Removed []string
	Freed   int64
}

type candidate struct {
	kind  string // "jobs" | "sessions"
	mgr   *supervisor.Manager
	job   supervisor.Job
	ended time.Time
}

// Run performs one collection.
func (c *Collector) Run(ctx context.Context, o Options) Result {
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	disk := c.DiskUsage
	if disk == nil {
		disk = platform.DiskUsage
	}
	var res Result
	var cands []candidate
	for _, src := range []struct {
		kind string
		mgr  *supervisor.Manager
	}{{"jobs", c.Jobs}, {"sessions", c.Sessions}} {
		if src.mgr == nil {
			continue
		}
		for _, j := range src.mgr.List(nil, 0) {
			if !j.Status.State.Terminal() {
				continue
			}
			ended := j.Status.EndedAt
			if ended.IsZero() {
				ended = j.Spec.CreatedAt
			}
			cands = append(cands, candidate{src.kind, src.mgr, j, ended})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].ended.Before(cands[j].ended) })

	remove := func(cd candidate) {
		dir := filepath.Join(c.DataDir, cd.kind, cd.job.Spec.ID)
		size := dirSize(dir)
		wt := cd.job.Status.Workdir
		if wt != "" {
			size += dirSize(wt)
		}
		name := cd.kind + "/" + cd.job.Spec.ID
		if !o.DryRun {
			if err := cd.mgr.Remove(cd.job.Spec.ID); err != nil {
				log.Warn("gc: remove", "what", name, "err", err)
				return
			}
			if wt != "" && c.WS != nil && isUnder(wt, filepath.Join(c.DataDir, workspace.WorktreesDir)) {
				if err := c.WS.RemoveWorktree(ctx, wt); err != nil {
					log.Warn("gc: remove worktree", "dir", wt, "err", err)
				}
			}
		}
		res.Removed = append(res.Removed, name)
		res.Freed += size
	}

	cutoff := time.Now().Add(-o.Keep)
	rest := cands[:0:0]
	for _, cd := range cands {
		if o.Keep >= 0 && !cd.ended.After(cutoff) {
			remove(cd)
		} else {
			rest = append(rest, cd)
		}
	}

	low := func() bool {
		if o.MinFreePercent <= 0 {
			return false
		}
		free, total, err := disk(c.DataDir)
		if err != nil || total == 0 {
			return false
		}
		if o.DryRun {
			free += uint64(res.Freed)
		}
		return float64(free)*100/float64(total) < o.MinFreePercent
	}
	for len(rest) > 0 && low() {
		remove(rest[0])
		rest = rest[1:]
	}

	c.collectStaging(o, &res)
	c.collectOrphanWorktrees(ctx, o, &res)
	if low() {
		c.collectMirrors(o, &res, low)
	}
	if !o.DryRun && c.WS != nil {
		c.WS.PruneAll(ctx)
	}
	if len(res.Removed) > 0 {
		log.Info("gc", "removed", len(res.Removed), "freed_bytes", res.Freed, "dry_run", o.DryRun)
	}
	return res
}

func (c *Collector) collectStaging(o Options, res *Result) {
	root := filepath.Join(c.DataDir, workspace.StagingDir)
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil || time.Since(fi.ModTime()) < StagingMaxAge {
			continue
		}
		p := filepath.Join(root, e.Name())
		size := dirSize(p)
		if !o.DryRun {
			if err := os.RemoveAll(p); err != nil {
				continue
			}
		}
		res.Removed = append(res.Removed, "staging/"+e.Name())
		res.Freed += size
	}
}

// collectOrphanWorktrees removes worktrees/<id> whose job no longer exists
// (e.g. preparation failed and the job dir was collected, or longarmd died).
func (c *Collector) collectOrphanWorktrees(ctx context.Context, o Options, res *Result) {
	if c.Jobs == nil || c.WS == nil {
		return
	}
	root := filepath.Join(c.DataDir, workspace.WorktreesDir)
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if !e.IsDir() || !jobs.ValidID(e.Name()) {
			continue
		}
		if _, err := c.Jobs.Get(e.Name()); err == nil {
			continue
		}
		fi, err := e.Info()
		if err != nil || time.Since(fi.ModTime()) < time.Hour { // may be preparing right now
			continue
		}
		p := filepath.Join(root, e.Name())
		size := dirSize(p)
		if !o.DryRun {
			if err := c.WS.RemoveWorktree(ctx, p); err != nil {
				continue
			}
		}
		res.Removed = append(res.Removed, "worktrees/"+e.Name())
		res.Freed += size
	}
}

// collectMirrors removes mirrors unused for longer than 4x the retention,
// least recently used first, while the disk is still low.
func (c *Collector) collectMirrors(o Options, res *Result, low func() bool) {
	if c.WS == nil {
		return
	}
	root := filepath.Join(c.DataDir, workspace.MirrorsDir)
	ents, _ := os.ReadDir(root)
	type mirror struct {
		repo string
		used time.Time
	}
	var ms []mirror
	for _, e := range ents {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".git") {
			repo := strings.TrimSuffix(e.Name(), ".git")
			ms = append(ms, mirror{repo, c.WS.MirrorLastUsed(repo)})
		}
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].used.Before(ms[j].used) })
	for _, m := range ms {
		if !low() || time.Since(m.used) < 4*o.Keep {
			return
		}
		if c.WS.MirrorInUse(m.repo) {
			continue // a live worktree (longarm sync target) depends on it
		}
		size := dirSize(c.WS.Mirror(m.repo))
		if !o.DryRun {
			if err := c.WS.RemoveMirror(m.repo); err != nil {
				continue
			}
		}
		res.Removed = append(res.Removed, "mirrors/"+m.repo+".git")
		res.Freed += size
	}
}

func dirSize(root string) int64 {
	var n int64
	filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func isUnder(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}
