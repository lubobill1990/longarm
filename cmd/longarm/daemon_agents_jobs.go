package main

// Agents in Longarm jobs (longarm run -- copilot -p / claude -p) and the
// private Copilot homes of jobs and sessions, for the Sessions window's list
// (docs/agent-sessions-ui.md 3.1).
//
// A job, session or exec given Copilot credentials gets a private
// COPILOT_HOME, <data dir>/agent-home/<kind>-<id>-<12 hex> (kind j: a job,
// s: a Longarm session; internal/userspace NewAgentHome): its
// conversations are under session-state there, not in ~/.copilot. A job
// with --repo runs in <data dir>/worktrees/<job id>: Claude Code keeps its
// transcript in ~/.claude/projects with that directory.

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/agentlog"
	"github.com/lubobill1990/longarm/internal/jobs"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// whereJob: the agent runs (or ran) in a Longarm job: read-only, no terminal.
const whereJob = "job"

// agentHome is a private COPILOT_HOME on a worker.
type agentHome struct {
	path   string
	kind   string    // "j" (a job) or "s" (a Longarm session)
	id     string    // the job's id or the session's name
	newest time.Time // its newest conversation's events.jsonl
}

var agentHomeRE = regexp.MustCompile(`^([js])-(.+)-[0-9a-f]{12}$`)

// agentHomes lists the private Copilot homes under dataDir with one
// listing, which it returns too (by its directory: hoEndpoint.listed).
func agentHomes(ctx context.Context, ep *hoEndpoint, dataDir string) ([]*agentHome, map[string][]hoFile, error) {
	root := ep.join(dataDir, jobs.AgentHomeDir)
	ents, err := ep.list(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	byDir := map[string]*agentHome{}
	for _, f := range ents {
		parts := strings.Split(f.rel, "/")
		m := agentHomeRE.FindStringSubmatch(parts[0])
		if m == nil {
			continue
		}
		h := byDir[parts[0]]
		if h == nil {
			h = &agentHome{path: ep.join(root, parts[0]), kind: m[1], id: m[2]}
			byDir[parts[0]] = h
		}
		if len(parts) == 4 && parts[1] == "session-state" && parts[3] == "events.jsonl" && f.mtime.After(h.newest) {
			h.newest = f.mtime
		}
	}
	out := make([]*agentHome, 0, len(byDir))
	for _, h := range byDir {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, map[string][]hoFile{root: ents}, nil
}

// homeTranscripts lists the conversations in the private Copilot homes
// under dataDir the list shows: those of running jobs and sessions (the
// first page) and the others modified in win; owners says whose they are.
// running: by "j-<job id>" / "s-<session name>".
func (d *daemon) homeTranscripts(ctx context.Context, ep *hoEndpoint, dataDir string, running map[string]bool, win agentWindow,
	sawOlder func(time.Time)) ([]*agentSession, map[*agentSession]agentOwner) {
	homes, listed, err := agentHomes(ctx, ep, dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "longarm daemon: agent homes on %s: %s\n", ep.name, oneLine(err))
		return nil, nil
	}
	lep := *ep // the homes' conversations from the one listing
	lep.listed = listed
	var out []*agentSession
	owners := map[*agentSession]agentOwner{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, h := range homes {
		live := running[h.kind+"-"+h.id]
		// a running one: on the first page only; the others: when in win
		if relevant := (live && !win.page()) || (!live && win.has(h.newest)); !relevant {
			if !live && !h.newest.IsZero() {
				sawOlder(h.newest)
			}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			keep := func(_ string, mod time.Time) bool {
				if live {
					return true
				}
				sawOlder(mod)
				return win.has(mod)
			}
			list, err := copilotAdapter{}.list(ctx, &lep, h.path, "", false, keep)
			if err != nil {
				fmt.Fprintf(os.Stderr, "longarm daemon: copilot sessions in %s on %s: %s\n", h.path, ep.name, oneLine(err))
			}
			o := agentOwner{job: h.id}
			if h.kind == "s" {
				o = agentOwner{session: h.id}
			}
			mu.Lock()
			defer mu.Unlock()
			for _, t := range list {
				out = append(out, t)
				owners[t] = o
			}
		}()
	}
	wg.Wait()
	return out, owners
}

// agentOwner is the job or Longarm session a transcript belongs to.
type agentOwner struct{ job, session string }

// jobOfDir is the job whose worktree dir is in (<dataDir>/worktrees/<id>/...), else "".
func jobOfDir(ep *hoEndpoint, dataDir, dir string) string {
	if dataDir == "" || dir == "" {
		return ""
	}
	root := ep.join(dataDir, "worktrees")
	if !ep.within(dir, root) || ep.samePath(dir, root) {
		return ""
	}
	rest := ep.native(dir)[len(strings.TrimRight(ep.native(root), ep.sep()))+1:]
	id, _, _ := strings.Cut(rest, ep.sep())
	if !jobIDRE.MatchString(id) {
		return ""
	}
	return id
}

// steerableItems are the running steerable jobs that have no item yet
// (no transcript found: e.g. still at a start-up screen) - their terminal
// is worth showing.
func steerableItems(worker string, items []*agentItem, byID map[string]*pb.Job) []*agentItem {
	have := map[string]bool{}
	for _, it := range items {
		if it.Job != "" {
			have[it.Job] = true
		}
	}
	var out []*agentItem
	for id, j := range byID {
		a := j.Spec.GetSteer().GetAgent()
		if a == "" || have[id] || terminalState(j.State) {
			continue
		}
		it := &agentItem{Key: worker + "/job/" + id, Worker: worker, Agent: a, Title: cmp.Or(j.Spec.GetName(), argvTitle(j.Spec.GetArgv())),
			ProjectDir: j.GetWorkdir(), LastActivity: j.GetStartedAt().AsTime(), live: true}
		if st := j.GetSteer().GetUpdatedAt(); st != nil {
			it.LastActivity = st.AsTime()
		}
		jobItem(it, id, j)
		it.State = agentlog.StateWorking
		if it.Steer == jobs.SteerNeedsYou {
			it.State = agentlog.StateWaiting
		}
		out = append(out, it)
	}
	return out
}

// jobIDRE: a job id (a ULID).
var jobIDRE = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// jobItem labels a transcript item as the job's: live while it runs.
func jobItem(it *agentItem, id string, job *pb.Job) {
	it.Where, it.Job, it.PID, it.live = whereJob, id, 0, false
	if job != nil {
		it.JobName = job.Spec.GetName()
		it.live = !terminalState(job.State)
		if it.live && job.Spec.GetSteer() != nil {
			it.Steer = cmp.Or(job.GetSteer().GetState(), jobs.SteerStarting)
		}
	}
	if !it.live {
		it.State = agentlog.StateEnded
	} else {
		it.State = "" // inferred from the transcript
	}
}

// agentLiveTTL: how long the live agent processes of a worker are reused.
// Finding them takes a process listing on the worker (an Exec: seconds on
// a busy one), so the list and the conversation use the last answer and
// refresh it in the background.
const agentLiveTTL = 20 * time.Second

// agentLiveWait: how long a first answer is waited for.
const agentLiveWait = 3 * time.Second

// agentLive is the live agent processes of the worker (liveAgents of every
// agent, by "<agent>/<id>"), at most agentLiveTTL old - or, until the
// first listing is done, what agentLiveWait gives (nil: none yet).
func (d *daemon) agentLive(ctx context.Context, ep *hoEndpoint, h *agentHost) map[string]int {
	agentHosts.Lock()
	live, done := h.live, h.liveDone
	if done == nil && time.Since(h.liveAt) >= agentLiveTTL {
		done = make(chan struct{})
		h.liveDone = done
		go func() {
			defer close(done)
			c, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			out := map[string]int{}
			for _, ad := range allAdapters {
				maps.Copy(out, liveAgents(c, ep, ad.name(), h.cfg[ad.name()]))
			}
			agentHosts.Lock()
			h.live, h.liveAt, h.liveDone = out, time.Now(), nil
			agentHosts.Unlock()
		}()
	}
	agentHosts.Unlock()
	if live == nil && done != nil {
		select {
		case <-done:
		case <-time.After(agentLiveWait):
		case <-ctx.Done():
		}
		agentHosts.Lock()
		live = h.live
		agentHosts.Unlock()
	}
	return live
}

// jobRunning reports whether the transcript at p is in the private Copilot
// home of a job that is running.
func (d *daemon) jobRunning(ctx context.Context, ep *hoEndpoint, h *agentHost, p string) bool {
	if h.dataDir == "" {
		return false
	}
	sub, ok := ep.subRel(ep.join(h.dataDir, jobs.AgentHomeDir), p)
	if !ok {
		return false
	}
	dir, _, _ := strings.Cut(sub, "/")
	m := agentHomeRE.FindStringSubmatch(dir)
	if m == nil || m[1] != "j" {
		return false
	}
	j, err := ep.conn.Jobs().Get(ctx, &pb.GetJobRequest{Id: m[2]})
	return err == nil && !terminalState(j.State)
}
