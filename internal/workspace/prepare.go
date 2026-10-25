package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/lubobill1990/longarm/internal/jobs"
)

// PrepareJob is the supervisor's Prepare hook: it gets the job's commit into
// the mirror (fetching spec.Workspace.URL, or using what Workspace.Sync
// staged), checks it out as <data>/worktrees/<job-id> and applies staged
// uncommitted changes. Progress goes to log (the job's out.log).
func (s *Store) PrepareJob(ctx context.Context, d jobs.Dir, spec jobs.Spec, secrets map[string]string, log io.Writer) (string, error) {
	w := spec.Workspace
	if w == nil {
		return "", errors.New("no workspace")
	}
	commit := w.Commit
	if w.URL != "" {
		fmt.Fprintf(log, "longarmd: fetching %s %s into mirror %s\n", w.URL, w.Ref, w.Repo)
		c, err := s.FetchURL(ctx, w.Repo, w.URL, w.Ref, secrets["GH_TOKEN"], log)
		if err != nil {
			fmt.Fprintf(log, "longarmd: %v\n", err)
			return "", err
		}
		if commit != "" && commit != c {
			if err := s.ImportBundle(ctx, w.Repo, "", "", commit); err != nil {
				return "", err
			}
		} else {
			commit = c
		}
	}
	if commit == "" {
		return "", errors.New("workspace needs a url or a commit")
	}
	if err := s.ImportBundle(ctx, w.Repo, "", "", commit); err != nil { // verify + pin
		fmt.Fprintf(log, "longarmd: %v\n", err)
		return "", err
	}
	var patch, untracked string
	if w.StagingID != "" {
		if !jobs.ValidID(w.StagingID) {
			return "", fmt.Errorf("bad staging id %q", w.StagingID)
		}
		patch, untracked = s.StagedFiles(w.StagingID)
	}
	dst := s.JobWorktree(spec.ID)
	fmt.Fprintf(log, "longarmd: worktree %s at %s\n", dst, commit)
	if err := s.Checkout(ctx, w.Repo, commit, dst, patch, untracked, log); err != nil {
		fmt.Fprintf(log, "longarmd: %v\n", err)
		return "", err
	}
	if w.StagingID != "" {
		s.RemoveStaging(w.StagingID)
	}
	return dst, nil
}
