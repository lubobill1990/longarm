package main

// The agent sessions of the tray's Sessions window (docs/agent-sessions-ui.md
// section 3): the Claude Code / Copilot CLI sessions of the user's workers,
// matched to the Longarm sessions they run in, their conversations
// (internal/agentlog), and the Longarm session controls. Transcripts are
// found by the handoff agent adapters (handoff_agents.go) and read through
// Files, worker-direct; the terminal (WebSocket) is daemon_agents_ws.go.
//
//	GET  /v1/agent-sessions?worker=&agent=&all=1
//	GET  /v1/agent-sessions/{worker}/{agent}/{id}/messages?offset=&tail_bytes=&limit_bytes=
//	GET  /v1/agent-sessions/{worker}/{agent}/{id}/messages/watch?offset=   (SSE: "messages", "state")
//	POST /v1/workers/{worker}/sessions                {"name","agent","cwd","resume","fork","argv","cols","rows"}
//	GET  /v1/workers/{worker}/sessions/{name}
//	POST /v1/workers/{worker}/sessions/{name}/send    {"text","keys","if_idle_ms","raw"}
//	POST /v1/workers/{worker}/sessions/{name}/stop    {"grace_ms"}
//	GET  /v1/workers/{worker}/sessions/{name}/attach?cols=&rows=   (WebSocket)

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/agentlog"
	"github.com/lubobill1990/longarm/internal/attach"
	"github.com/lubobill1990/longarm/internal/client"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

const (
	agentListTimeout = 15 * time.Second // per worker
	agentTailDefault = 2 << 20          // messages: the last 2 MiB
	agentReadMax     = 16 << 20         // messages: at most this much per request
	agentStateTail   = 64 << 10         // what the list reads of a live transcript for its state
	agentStateEvery  = 15 * time.Second // watch: liveness and idle re-checked
	agentHostTTL     = 10 * time.Minute
)

// Where an agent session runs.
const (
	whereLongarm = "longarm" // in a Longarm session
	whereDesktop = "desktop" // a live process outside Longarm
	whereNone    = "none"    // no live process: the transcript only
)

// stateRunning: a Longarm session whose program is no known agent.
const stateRunning = "running"

var agentMO = protojson.MarshalOptions{UseProtoNames: true}

// agentItem is one entry of the list.
type agentItem struct {
	Key        string    `json:"key"`
	Worker     string    `json:"worker"`
	Agent      string    `json:"agent"`
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	ProjectDir string    `json:"project_dir,omitempty"`
	Modified   time.Time `json:"modified,omitzero"`
	Size       int64     `json:"size,omitempty"`
	Where      string    `json:"where"`
	Session    string    `json:"session,omitempty"`
	Job        string    `json:"job,omitempty"`      // where job: the job's id
	JobName    string    `json:"job_name,omitempty"` // and its name
	// Steer: a steerable job's state while it runs (starting, working,
	// needs_you, idle, finishing; docs/steerable-runs.md): its terminal
	// can be attached to and typed into (/v1/workers/{w}/jobs/{id}/...).
	Steer        string    `json:"steer,omitempty"`
	State        string    `json:"state"`
	LastActivity time.Time `json:"last_activity,omitzero"`
	PID          int       `json:"pid,omitempty"`

	transcript string // path on the worker ("" = none yet)
	live       bool
	lastOutput time.Time // the Longarm session's terminal output (zero: unknown)
}

// agentBusyOutput: an agent whose terminal printed this recently is
// working (its TUI animates while it works and is quiet while it waits).
const agentBusyOutput = 3 * time.Second

type agentListJSON struct {
	Items       []*agentItem `json:"items"`
	Unreachable []string     `json:"unreachable"`
	// Older is the modification time of the newest transcript older than
	// the window (unset: none): the next page is until=<older>.
	Older time.Time `json:"older,omitzero"`
}

// agentJobsMax: the newest jobs the list asks a worker for (their names
// and states for the agents in them).
const agentJobsMax = 500

// agentRecent is how far back the first page of the list goes.
const agentRecent = 7 * 24 * time.Hour

// agentWindow is the part of the transcripts a list shows: modified in
// [since, until]; until zero: the first page (no upper bound; the running
// sessions too, whenever their transcripts were written), else an older
// page (transcripts only).
type agentWindow struct{ since, until time.Time }

func (w agentWindow) page() bool { return !w.until.IsZero() }

func (w agentWindow) has(t time.Time) bool {
	return !t.Before(w.since) && (w.until.IsZero() || !t.After(w.until))
}

// queryWindow reads since and until (RFC 3339); since defaults to
// agentRecent before until (or now).
func queryWindow(q url.Values, now time.Time) (agentWindow, error) {
	var w agentWindow
	for _, f := range []struct {
		k string
		t *time.Time
	}{{"since", &w.since}, {"until", &w.until}} {
		if v := q.Get(f.k); v != "" {
			t, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				return w, usagef("%s %q: want an RFC 3339 time", f.k, v)
			}
			*f.t = t
		}
	}
	if w.since.IsZero() {
		w.since = cmp.Or(w.until, now).Add(-agentRecent)
	}
	return w, nil
}

type messagesJSON struct {
	Messages []agentlog.Message `json:"messages"`
	// Start is where the first message's line begins: the next "load
	// earlier" asks before=<start>.
	Start      int64  `json:"start"`
	NextOffset int64  `json:"next_offset"`
	Size       int64  `json:"size"`
	Truncated  bool   `json:"truncated"`
	State      string `json:"state"`
}

// agentsGateway adds the routes of the Sessions window (see the top of this file).
func (d *daemon) agentsGateway(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/agent-sessions", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		agent := q.Get("agent")
		if agent != "" && agentOf([]string{agent}) == "" {
			agentError(w, usagef("agent %q: want claude or copilot", agent))
			return
		}
		win, err := queryWindow(q, time.Now())
		if err != nil {
			agentError(w, err)
			return
		}
		workers := []string{q.Get("worker")}
		if workers[0] == "" {
			workers = d.agentWorkers()
		}
		agentJSON(w, http.StatusOK, d.listAgentSessions(r.Context(), workers, agent, queryFlag(q, "all"), win))
	})
	mux.HandleFunc("GET /v1/agent-sessions/{worker}/{agent}/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		m, err := d.agentMessages(r.Context(), r.PathValue("worker"), r.PathValue("agent"), r.PathValue("id"), r.URL.Query())
		if err != nil {
			agentError(w, err)
			return
		}
		agentJSON(w, http.StatusOK, m)
	})
	mux.HandleFunc("GET /v1/agent-sessions/{worker}/{agent}/{id}/messages/watch", func(w http.ResponseWriter, r *http.Request) {
		off, given, err := queryInt(r.URL.Query(), "offset")
		if err != nil {
			agentError(w, err)
			return
		}
		sseJSON(w, r, func(ctx context.Context, send func(string, any) error) error {
			return d.watchMessages(ctx, r.PathValue("worker"), r.PathValue("agent"), r.PathValue("id"), off, given, send)
		})
	})
	mux.HandleFunc("POST /v1/workers/{worker}/sessions", func(w http.ResponseWriter, r *http.Request) {
		var req startAgentReq
		if err := readJSON(r, &req); err != nil {
			agentError(w, err)
			return
		}
		res, err := d.startAgentSession(r.Context(), r.PathValue("worker"), req)
		if err != nil {
			agentError(w, err)
			return
		}
		code := http.StatusOK
		if res.Created {
			code = http.StatusCreated
		}
		sessionJSON(w, code, res.Session)
	})
	mux.HandleFunc("GET /v1/workers/{worker}/sessions/{name}", func(w http.ResponseWriter, r *http.Request) {
		ss, err := d.sessionCall(r, "session get", func(ctx context.Context, sc pb.SessionsClient, name string) (*pb.Session, error) {
			return sc.Get(ctx, &pb.GetSessionRequest{Name: name})
		})
		if err != nil {
			agentError(w, err)
			return
		}
		sessionJSON(w, http.StatusOK, ss)
	})
	mux.HandleFunc("POST /v1/workers/{worker}/sessions/{name}/send", func(w http.ResponseWriter, r *http.Request) {
		var req sendReq
		if err := readJSON(r, &req); err != nil {
			agentError(w, err)
			return
		}
		seq, err := d.sendSession(r.Context(), r.PathValue("worker"), r.PathValue("name"), false, req)
		if err != nil {
			agentError(w, err)
			return
		}
		agentJSON(w, http.StatusOK, map[string]uint64{"input_seq": seq})
	})
	// a steerable run (longarm run --steerable): type into it, watch it
	mux.HandleFunc("POST /v1/workers/{worker}/jobs/{id}/send", func(w http.ResponseWriter, r *http.Request) {
		var req sendReq
		if err := readJSON(r, &req); err != nil {
			agentError(w, err)
			return
		}
		seq, err := d.sendSession(r.Context(), r.PathValue("worker"), r.PathValue("id"), true, req)
		if err != nil {
			agentError(w, err)
			return
		}
		agentJSON(w, http.StatusOK, map[string]uint64{"input_seq": seq})
	})
	mux.HandleFunc("GET /v1/workers/{worker}/jobs/{id}/attach", d.attachWS)
	mux.HandleFunc("POST /v1/workers/{worker}/sessions/{name}/stop", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			GraceMs *int64 `json:"grace_ms"`
		}
		if err := readJSON(r, &req); err != nil {
			agentError(w, err)
			return
		}
		grace := 10 * time.Second
		if req.GraceMs != nil {
			grace = time.Duration(max(*req.GraceMs, 0)) * time.Millisecond
		}
		ss, err := d.sessionCall(r, "session stop", func(ctx context.Context, sc pb.SessionsClient, name string) (*pb.Session, error) {
			ctx, cancel := context.WithTimeout(ctx, grace+time.Minute)
			defer cancel()
			return sc.Stop(ctx, &pb.StopSessionRequest{Name: name, Grace: durationpb.New(grace)})
		})
		if err != nil {
			agentError(w, err)
			return
		}
		sessionJSON(w, http.StatusOK, ss)
	})
	mux.HandleFunc("GET /v1/workers/{worker}/sessions/{name}/attach", d.attachWS)
}

// ---------------------------------------------------------------- list

// agentWorkers are the workers the list asks by default: those online in
// the pool view that are this user's or nobody's (fleet workers).
func (d *daemon) agentWorkers() []string {
	me := d.s.dialer.Identity().Owner()
	var out []string
	for _, p := range d.mon.View(false).Workers {
		if o := p.GetNode().GetOwner(); p.State == "online" && (o == "" || o == me) {
			out = append(out, p.Name)
		}
	}
	return out
}

// listAgentSessions asks the workers in parallel (agentListTimeout each).
func (d *daemon) listAgentSessions(ctx context.Context, workers []string, agent string, all bool, win agentWindow) agentListJSON {
	out := agentListJSON{Items: []*agentItem{}, Unreachable: []string{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, agentListTimeout)
			defer cancel()
			items, older, err := d.workerAgentSessions(c, w, agent, all, win)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				fmt.Fprintf(os.Stderr, "longarm daemon: agent sessions of %s: %s\n", w, oneLine(err))
				out.Unreachable = append(out.Unreachable, w)
				return
			}
			out.Items = append(out.Items, items...)
			if older.After(out.Older) {
				out.Older = older
			}
		}()
	}
	wg.Wait()
	sort.Strings(out.Unreachable)
	sortAgentItems(out.Items)
	return out
}

// workerAgentSessions lists one worker's sessions in win: its Longarm
// sessions, the agents' transcripts and live processes, matched (an older
// page: the transcripts only). older: the newest transcript before win.
func (d *daemon) workerAgentSessions(ctx context.Context, worker, agent string, all bool, win agentWindow) (items []*agentItem, older time.Time, err error) {
	ep, h, err := d.agentEndpoint(ctx, worker)
	if err != nil {
		return nil, older, err
	}
	adapters := slices.DeleteFunc(slices.Clone(allAdapters), func(ad agentAdapter) bool { return agent != "" && ad.name() != agent })
	var sessions []*pb.Session
	var serr error
	live := map[string]int{}
	jobsByID := map[string]*pb.Job{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(2) // an older page too: what the running sessions resume stays on the first
	go func() {
		defer wg.Done()
		resp, err := ep.conn.Sessions().List(ctx, &pb.ListSessionsRequest{All: all})
		sessions, serr = resp.GetSessions(), err
	}()
	go func() { // the agents in jobs: their names, whether they run
		defer wg.Done()
		resp, err := ep.conn.Jobs().List(ctx, &pb.ListJobsRequest{Limit: agentJobsMax})
		if err != nil {
			fmt.Fprintf(os.Stderr, "longarm daemon: jobs on %s: %s\n", worker, oneLine(err))
		}
		for _, j := range resp.GetJobs() {
			jobsByID[j.Spec.GetId()] = j
		}
	}()
	maps.Copy(live, d.agentLive(ctx, ep, h))
	wg.Wait()
	if serr != nil {
		return nil, older, serr
	}
	// the running sessions' transcripts are on the first page, however old
	pinned := map[string]bool{}
	for k := range live {
		pinned[k] = true
	}
	for _, ss := range sessions {
		if a, id := agentOf(ss.Argv), resumedID(ss.Argv); a != "" && id != "" && !terminalState(ss.State) {
			pinned[a+"/"+id] = true
		}
	}
	sawOlder := func(mod time.Time) {
		if mod.Before(win.since) {
			mu.Lock()
			if mod.After(older) {
				older = mod
			}
			mu.Unlock()
		}
	}
	var transcripts []*agentSession
	owners := map[*agentSession]agentOwner{}
	if h.dataDir != "" && slices.ContainsFunc(adapters, func(ad agentAdapter) bool { return ad.name() == agentlog.Copilot }) {
		running := map[string]bool{} // "j-<id>" / "s-<name>" of what runs
		for id, j := range jobsByID {
			running["j-"+id] = !terminalState(j.State)
		}
		for _, ss := range sessions {
			running["s-"+ss.Name] = !terminalState(ss.State)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			list, owned := d.homeTranscripts(ctx, ep, h.dataDir, running, win, sawOlder)
			mu.Lock()
			defer mu.Unlock()
			transcripts = append(transcripts, list...)
			maps.Copy(owners, owned)
		}()
	}
	for _, ad := range adapters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keep := func(id string, mod time.Time) bool {
				if pinned[ad.name()+"/"+id] {
					return !win.page()
				}
				sawOlder(mod)
				return win.has(mod)
			}
			list, err := ad.list(ctx, ep, h.cfg[ad.name()], "", false, keep)
			if err != nil { // the Longarm sessions are still worth listing
				fmt.Fprintf(os.Stderr, "longarm daemon: %s sessions on %s: %s\n", ad.name(), worker, oneLine(err))
			}
			mu.Lock()
			defer mu.Unlock()
			transcripts = append(transcripts, list...)
		}()
	}
	wg.Wait()
	h.remember(transcripts)
	steered := map[string]string{} // "<agent>/<session id>" of a steerable job: the job
	for id, j := range jobsByID {
		if a, sid := j.Spec.GetSteer().GetAgent(), j.GetSteer().GetSessionId(); a != "" && sid != "" {
			steered[a+"/"+sid] = id
		}
	}
	for _, t := range transcripts { // an agent in a job's worktree, a steerable job's: the job's
		if _, ok := owners[t]; !ok {
			if id := jobOfDir(ep, h.dataDir, t.ProjectDir); id != "" {
				owners[t] = agentOwner{job: id}
			} else if id := steered[t.Agent+"/"+t.ID]; id != "" {
				owners[t] = agentOwner{job: id}
			}
		}
	}
	if win.page() {
		sessions = nil // listed on the first page
	}
	items = matchAgentSessions(ep, sessions, transcripts, live, all, owners, jobsByID)
	if !win.page() {
		items = append(items, steerableItems(ep.name, items, jobsByID)...)
	}
	if agent != "" {
		items = slices.DeleteFunc(items, func(it *agentItem) bool { return it.Agent != agent })
	}
	inferStates(ctx, ep, items)
	return items, older, nil
}

// agentOf is the agent a command line runs: claude or copilot by the
// program's base name (.exe / .cmd stripped), else "".
func agentOf(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	b := strings.ToLower(argv[0][strings.LastIndexAny(argv[0], `/\`)+1:])
	b = strings.TrimSuffix(strings.TrimSuffix(b, ".exe"), ".cmd")
	if b == agentlog.Claude || b == agentlog.Copilot {
		return b
	}
	return ""
}

// resumedID is the conversation a claude / copilot command line resumes
// in place ("" for a new or a forked one).
func resumedID(argv []string) string {
	if slices.Contains(argv, "--fork-session") {
		return ""
	}
	for i := 1; i < len(argv); i++ {
		switch a := argv[i]; {
		case (a == "--resume" || a == "-r") && i+1 < len(argv):
			return argv[i+1]
		case strings.HasPrefix(a, "--resume="):
			return strings.TrimPrefix(a, "--resume=")
		}
	}
	return ""
}

// matchAgentSessions pairs a worker's Longarm sessions with the agent
// transcripts found there and lists the other transcripts: live elsewhere
// (desktop) or the transcript only (none; handed-off ones only with all).
// live: the running agent processes, pid by "<agent>/<id>" (0: unknown).
// A session is paired with the transcript its process has open, the one
// it resumes, else the newest one of its agent in its directory written
// since it started. owners: transcripts known to be a job's or a session's
// (their private Copilot homes, job worktrees); a session's own comes
// first, a job's are listed as the job's. States other than ended and
// running are inferStates'.
func matchAgentSessions(ep *hoEndpoint, sessions []*pb.Session, transcripts []*agentSession, live map[string]int, all bool,
	owners map[*agentSession]agentOwner, jobs map[string]*pb.Job) []*agentItem {
	byKey := map[string]*agentSession{}
	bySession := map[string]*agentSession{}
	for _, t := range transcripts {
		byKey[t.Agent+"/"+t.ID] = t
		if o := owners[t]; o.session != "" && (bySession[o.session] == nil || t.Modified.After(bySession[o.session].Modified)) {
			bySession[o.session] = t
		}
	}
	byPID := map[int]string{}
	for k, pid := range live {
		if pid > 0 {
			byPID[pid] = k
		}
	}
	claimed := map[*agentSession]bool{}
	found := make([]*agentSession, len(sessions))
	for i, ss := range sessions {
		a := agentOf(ss.Argv)
		if a == "" {
			continue
		}
		t := byKey[a+"/"+resumedID(ss.Argv)]
		if k, ok := byPID[int(ss.Pid)]; ok && strings.HasPrefix(k, a+"/") && byKey[k] != nil {
			t = byKey[k]
		}
		if own := bySession[ss.Name]; own != nil && own.Agent == a { // its private Copilot home
			t = own
		}
		if t != nil && !claimed[t] {
			found[i], claimed[t] = t, true
		}
	}
	order := make([]int, len(sessions)) // the newest session first: it gets the newest transcript
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		return sessionStart(sessions[order[i]]).After(sessionStart(sessions[order[j]]))
	})
	for _, i := range order {
		ss := sessions[i]
		a := agentOf(ss.Argv)
		if a == "" || found[i] != nil {
			continue
		}
		dir := cmp.Or(ss.Dir, ep.home)
		var best *agentSession
		for _, t := range transcripts {
			if t.Agent == a && !claimed[t] && ep.samePath(t.ProjectDir, dir) && t.Modified.After(sessionStart(ss)) &&
				(best == nil || t.Modified.After(best.Modified)) {
				best = t
			}
		}
		if best != nil {
			found[i], claimed[best] = best, true
		}
	}
	var items []*agentItem
	for i, ss := range sessions {
		items = append(items, sessionItem(ep.name, ss, found[i]))
	}
	for _, t := range transcripts {
		if claimed[t] || (t.HandedOff != nil && !all) {
			continue
		}
		it := transcriptItem(ep.name, t)
		if o := owners[t]; o.job != "" {
			jobItem(it, o.job, jobs[o.job])
			items = append(items, it)
			continue
		}
		if pid, ok := live[t.Agent+"/"+t.ID]; ok {
			it.Where, it.PID, it.live = whereDesktop, pid, true
		} else {
			it.Where, it.State = whereNone, agentlog.StateEnded
		}
		items = append(items, it)
	}
	return items
}

func transcriptItem(worker string, t *agentSession) *agentItem {
	return &agentItem{Key: worker + "/" + t.Agent + "/" + t.ID, Worker: worker, Agent: t.Agent, ID: t.ID, Title: t.Title,
		ProjectDir: t.ProjectDir, Modified: t.Modified, Size: t.Size, LastActivity: t.Modified, transcript: t.transcript}
}

// sessionItem is a Longarm session and the transcript paired with it (t nil: none).
func sessionItem(worker string, ss *pb.Session, t *agentSession) *agentItem {
	it := &agentItem{Key: worker + "/session/" + ss.Name, Worker: worker, Agent: agentOf(ss.Argv), Title: argvTitle(ss.Argv),
		ProjectDir: ss.Dir, LastActivity: pbTime(ss.LastActivity)}
	if t != nil {
		title := cmp.Or(t.Title, it.Title)
		it = transcriptItem(worker, t)
		it.Title = title
		if la := pbTime(ss.LastActivity); la.After(it.LastActivity) {
			it.LastActivity = la
		}
	}
	if it.LastActivity.IsZero() {
		it.LastActivity = pbTime(ss.CreatedAt)
	}
	it.Where, it.Session, it.PID, it.live = whereLongarm, ss.Name, int(ss.Pid), !terminalState(ss.State)
	it.lastOutput = pbTime(ss.LastOutput)
	switch {
	case !it.live:
		it.State = agentlog.StateEnded
	case it.Agent == "":
		it.State = stateRunning
	}
	return it
}

// argvTitle names a session by its program ("shell": the worker user's).
func argvTitle(argv []string) string {
	if len(argv) == 0 {
		return "shell"
	}
	return argv[0][strings.LastIndexAny(argv[0], `/\`)+1:]
}

func sessionStart(ss *pb.Session) time.Time {
	if t := pbTime(ss.StartedAt); !t.IsZero() {
		return t
	}
	return pbTime(ss.CreatedAt)
}

// pbTime is ts, or the zero time when unset.
func pbTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

// sortAgentItems: live ones first, then the most recently active.
func sortAgentItems(items []*agentItem) {
	sort.SliceStable(items, func(i, j int) bool {
		li, lj := items[i].State != agentlog.StateEnded, items[j].State != agentlog.StateEnded
		if li != lj {
			return li
		}
		return items[i].LastActivity.After(items[j].LastActivity)
	})
}

// inferStates fills in the state of the live agent sessions from the end
// of their transcripts (agentlog.InferState).
func inferStates(ctx context.Context, ep *hoEndpoint, items []*agentItem) {
	now := time.Now()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, it := range items {
		if it.State != "" {
			continue
		}
		if !it.lastOutput.IsZero() && now.Sub(it.lastOutput) < agentBusyOutput {
			it.State = agentlog.StateWorking // the terminal says so, whatever the transcript's end
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			var msgs []agentlog.Message
			if it.transcript != "" {
				if lines, _, _, err := tailLines(ctx, ep, it.transcript, agentStateTail); err == nil {
					msgs = agentlog.Parse(it.Agent, lines, 0)
				}
			}
			it.State = agentlog.InferState(it.Agent, msgs, it.live, it.LastActivity, now)
		}()
	}
	wg.Wait()
}

// liveAgents finds the agent's processes running on ep: Claude Code's
// sessions/<pid>.json of live processes, Copilot CLI's recently refreshed
// open sessions (pid unknown: 0). Keys: "<agent>/<id>".
func liveAgents(ctx context.Context, ep *hoEndpoint, agent, cfg string) map[string]int {
	out := map[string]int{}
	switch agent {
	case agentlog.Claude:
		dir := ep.join(cfg, "sessions")
		ents, err := ep.list(ctx, dir)
		if err != nil {
			return out
		}
		var mu sync.Mutex
		var wg sync.WaitGroup
		// the processes, listed once (a check per file took a tasklist each:
		// workers with many ended claude -p jobs ran out of time)
		names := sync.OnceValue(func() map[int]string { return processNames(ctx, ep) })
		sem := make(chan struct{}, 8)
		for _, f := range ents {
			if f.isDir || strings.Contains(f.rel, "/") || !strings.HasSuffix(f.rel, ".json") {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				b, err := ep.readSmall(ctx, ep.join(dir, f.rel), 256<<10)
				var ps struct {
					PID       int    `json:"pid"`
					SessionID string `json:"sessionId"`
				}
				if err != nil || json.Unmarshal(b, &ps) != nil || ps.PID <= 0 || ps.SessionID == "" {
					return
				}
				name, alive := "", false
				if all := names(); all != nil {
					name, alive = all[ps.PID]
				} else {
					name, alive = processAlive(ctx, ep, ps.PID)
				}
				if alive && (name == "" || agentProcName(name)) {
					mu.Lock()
					out[agent+"/"+ps.SessionID] = ps.PID
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
	case agentlog.Copilot:
		b, err := ep.readSmall(ctx, ep.join(cfg, "open-sessions-state.json"), 4<<20)
		var open map[string]struct {
			RefreshedAt time.Time `json:"refreshedAt"`
		}
		if err != nil || json.Unmarshal(b, &open) != nil {
			return out
		}
		for id, e := range open {
			if time.Since(e.RefreshedAt) < copilotOpenWindow {
				out[agent+"/"+id] = 0
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- workers

// agentHost is what the agent views keep of a worker: its OS and home,
// the agents' config directories (learning Claude's takes an Exec) and
// where the transcripts listed last are.
type agentHost struct {
	goos, home string
	dataDir    string            // longarmd's ("" when the worker doesn't say: per-user isolation)
	cfg        map[string]string // by agent
	paths      map[string]string // transcript by "<agent>/<id>"
	// the live agent processes (agentLive): the last answer, when, and the
	// refresh under way (nil: none)
	live     map[string]int
	liveAt   time.Time
	liveDone chan struct{}
	at       time.Time
}

var agentHosts = struct {
	sync.Mutex
	m map[string]*agentHost
}{m: map[string]*agentHost{}}

func (h *agentHost) remember(ts []*agentSession) {
	agentHosts.Lock()
	defer agentHosts.Unlock()
	for _, t := range ts {
		h.paths[t.Agent+"/"+t.ID] = t.transcript
	}
}

func (h *agentHost) path(key string) string {
	agentHosts.Lock()
	defer agentHosts.Unlock()
	return h.paths[key]
}

// agentEndpoint is the worker as a handoff endpoint over the daemon's
// connection to it (not to be closed: d.conn's).
func (d *daemon) agentEndpoint(ctx context.Context, worker string) (*hoEndpoint, *agentHost, error) {
	conn, err := d.conn(worker)
	if err != nil {
		return nil, nil, err
	}
	agentHosts.Lock()
	h := agentHosts.m[worker]
	agentHosts.Unlock()
	if h != nil && time.Since(h.at) < agentHostTTL {
		return &hoEndpoint{name: worker, conn: conn, goos: h.goos, home: h.home}, h, nil
	}
	info, err := conn.Node().Info(ctx, &pb.InfoRequest{})
	if err != nil {
		return nil, nil, err
	}
	home, err := conn.Files().Stat(ctx, &pb.FileStatRequest{Path: "~"})
	if err != nil {
		return nil, nil, err
	}
	ep := &hoEndpoint{name: worker, conn: conn, goos: info.Os, home: home.AbsPath}
	h = &agentHost{goos: ep.goos, home: ep.home, dataDir: info.DataDir, cfg: map[string]string{}, paths: map[string]string{}, at: time.Now()}
	for _, ad := range allAdapters {
		h.cfg[ad.name()] = ad.configDir(ctx, ep)
	}
	agentHosts.Lock()
	agentHosts.m[worker] = h
	agentHosts.Unlock()
	return ep, h, nil
}

// agentTranscript finds the transcript of agent's conversation id on worker.
func (d *daemon) agentTranscript(ctx context.Context, worker, agent, id string) (*hoEndpoint, *agentHost, string, error) {
	ad, err := adapterFor(agent)
	if err != nil {
		return nil, nil, "", err
	}
	if !sessionIDRE.MatchString(id) {
		return nil, nil, "", usagef("session id %q: not an agent session id", id)
	}
	ep, h, err := d.agentEndpoint(ctx, worker)
	if err != nil {
		return nil, nil, "", err
	}
	if p := h.path(agent + "/" + id); p != "" {
		return ep, h, p, nil
	}
	list, err := ad.list(ctx, ep, h.cfg[agent], id, false, nil)
	if err != nil {
		return nil, nil, "", err
	}
	if len(list) == 0 && agent == agentlog.Copilot && h.dataDir != "" {
		// a job's or session's private Copilot home
		homes, listed, _ := agentHomes(ctx, ep, h.dataDir)
		lep := *ep
		lep.listed = listed
		for _, home := range homes {
			if list, _ = ad.list(ctx, &lep, home.path, id, false, nil); len(list) > 0 {
				break
			}
		}
	}
	if len(list) == 0 {
		return nil, nil, "", notFoundf("no %s session %s on %s", agent, id, worker)
	}
	h.remember(list[:1])
	return ep, h, list[0].transcript, nil
}

// ---------------------------------------------------------------- messages

// transcriptLines reads the complete lines of a transcript from off (at
// most limit bytes). align: off may fall inside a line, which is skipped.
// next is the offset after the last complete line.
func transcriptLines(ctx context.Context, ep *hoEndpoint, p string, off, limit int64, align bool) ([]agentlog.Line, int64, error) {
	skip := align && off > 0
	if skip { // the byte before off tells whether a line starts at off
		off, limit = off-1, limit+1
	}
	b, err := ep.readRange(ctx, p, off, limit)
	if err != nil {
		return nil, off, err
	}
	lines, next := splitLines(b, off, skip)
	return lines, next, nil
}

// tailLines reads the complete lines of the last n bytes of a transcript;
// start is where they begin (> 0: older content exists), size the file's.
func tailLines(ctx context.Context, ep *hoEndpoint, p string, n int64) (lines []agentlog.Line, start, size int64, err error) {
	fi, ok, err := ep.stat(ctx, p)
	if err != nil {
		return nil, 0, 0, err
	}
	if !ok {
		return nil, 0, 0, notFoundf("%s: no such file", p)
	}
	start = max(fi.size-n, 0)
	lines, _, err = transcriptLines(ctx, ep, p, start, fi.size-start, true)
	if len(lines) > 0 {
		start = lines[0].Offset
	}
	return lines, start, fi.size, err
}

// splitLines cuts b, read from offset base, into its complete lines (a
// trailing CR dropped, blank ones skipped); skipFirst drops what comes
// before the first newline. next is the offset after the last newline.
func splitLines(b []byte, base int64, skipFirst bool) (lines []agentlog.Line, next int64) {
	pos := 0
	if skipFirst {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			return nil, base
		}
		pos = i + 1
	}
	for {
		i := bytes.IndexByte(b[pos:], '\n')
		if i < 0 {
			return lines, base + int64(pos)
		}
		if l := bytes.TrimSuffix(b[pos:pos+i], []byte("\r")); len(bytes.TrimSpace(l)) > 0 {
			lines = append(lines, agentlog.Line{Offset: base + int64(pos), Data: l})
		}
		pos += i + 1
	}
}

// agentMessages is GET .../messages: from offset (a line start), else the
// last tail_bytes before before (a line start; default: the end) aligned to
// the next line start.
func (d *daemon) agentMessages(ctx context.Context, worker, agent, id string, q url.Values) (*messagesJSON, error) {
	off, given, err := queryInt(q, "offset")
	if err != nil {
		return nil, err
	}
	before, beforeGiven, err := queryInt(q, "before")
	if err != nil {
		return nil, err
	}
	tail, _, err := queryInt(q, "tail_bytes")
	if err != nil {
		return nil, err
	}
	limit, _, err := queryInt(q, "limit_bytes")
	if err != nil {
		return nil, err
	}
	ep, h, p, err := d.agentTranscript(ctx, worker, agent, id)
	if err != nil {
		return nil, err
	}
	fi, ok, err := ep.stat(ctx, p)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, notFoundf("no %s session %s on %s", agent, id, worker)
	}
	if limit <= 0 {
		limit = agentTailDefault
	}
	limit = min(limit, agentReadMax)
	align := !given || off < 0 || off > fi.size // a stale offset (the file shrank): the tail
	end := fi.size
	if align {
		if tail <= 0 {
			tail = agentTailDefault
		}
		if beforeGiven && before >= 0 && before < end {
			end = before // load earlier: the part before the first message shown
		}
		off = max(end-min(tail, agentReadMax), 0)
		limit = end - off
	}
	var lines []agentlog.Line
	var next int64
	for {
		if lines, next, err = transcriptLines(ctx, ep, p, off, limit, align); err != nil {
			return nil, err
		}
		// a window inside one long line (a big tool result) holds no
		// complete line: look further back
		if len(lines) > 0 || !align || off == 0 || limit >= agentReadMax {
			break
		}
		off = max(end-min(limit*4, agentReadMax), 0)
		limit = end - off
	}
	first := next
	if len(lines) > 0 {
		first = lines[0].Offset
	} else if align {
		first, next = end, end
	}
	msgs := agentlog.Parse(agent, lines, 0)
	_, isLive := d.agentLive(ctx, ep, h)[agent+"/"+id]
	if !isLive {
		isLive = d.jobRunning(ctx, ep, h, p)
	}
	m := &messagesJSON{Messages: msgs, Start: first, NextOffset: next, Size: fi.size, Truncated: first > 0,
		State: agentlog.InferState(agent, msgs, isLive, fi.mtime, time.Now())}
	if m.Messages == nil {
		m.Messages = []agentlog.Message{}
	}
	return m, nil
}

// watchMessages follows a transcript from off (default: its end) and
// sends "messages" whenever complete lines arrive and "state" when the
// inferred state changes, until ctx ends.
func (d *daemon) watchMessages(ctx context.Context, worker, agent, id string, off int64, given bool, send func(string, any) error) error {
	ep, h, p, err := d.agentTranscript(ctx, worker, agent, id)
	if err != nil {
		return err
	}
	// what came before off: the state as the client has it
	recent, _, size, err := tailLines(ctx, ep, p, agentStateTail)
	if err != nil {
		return err
	}
	if !given || off < 0 || off > size {
		off = size
	}
	w := &transcriptWatch{agent: agent, base: off, recent: agentlog.Parse(agent, recent, 0), lastActivity: time.Now()}
	isLive := func() bool {
		_, ok := d.agentLive(ctx, ep, h)[agent+"/"+id]
		return ok || d.jobRunning(ctx, ep, h, p)
	}
	w.live = isLive()
	w.state = w.inferState()
	st, err := ep.conn.Files().Tail(ctx, &pb.FileTailRequest{Path: p, Offset: off, Follow: true})
	if err != nil {
		return err
	}
	chunks := make(chan *pb.FileChunk, 64)
	errc := make(chan error, 1)
	go func() {
		for {
			ch, err := st.Recv()
			if err != nil {
				errc <- err
				return
			}
			select {
			case chunks <- ch:
			case <-ctx.Done():
				return
			}
		}
	}()
	tick := time.NewTicker(agentStateEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case <-tick.C:
			w.live = isLive()
		case ch := <-chunks:
			w.add(ch)
			for drained := false; !drained; { // what else arrived: one event
				select {
				case ch := <-chunks:
					w.add(ch)
				default:
					drained = true
				}
			}
			if m := w.flush(); m != nil {
				if err := send("messages", m); err != nil {
					return err
				}
			}
		}
		if s := w.inferState(); s != w.state {
			w.state = s
			if err := send("state", map[string]string{"state": s}); err != nil {
				return err
			}
		}
	}
}

// transcriptWatch is the state of one watch: bytes received after the
// last complete line, the recent messages (for the state), the seq.
type transcriptWatch struct {
	agent        string
	base         int64 // offset of buf
	buf          []byte
	seq          int
	recent       []agentlog.Message
	live         bool
	lastActivity time.Time
	state        string
}

// add appends a chunk; one starting before the end means the file shrank
// (replaced): start over from there.
func (w *transcriptWatch) add(ch *pb.FileChunk) {
	if ch.Offset < w.base+int64(len(w.buf)) {
		w.base, w.buf, w.recent = ch.Offset, nil, nil
	}
	w.buf = append(w.buf, ch.Data...)
	w.lastActivity = time.Now()
}

// flush parses the complete lines received (nil: none yet).
func (w *transcriptWatch) flush() *messagesJSON {
	lines, next := splitLines(w.buf, w.base, false)
	if next == w.base {
		return nil
	}
	msgs := agentlog.Parse(w.agent, lines, w.seq)
	w.seq += len(msgs)
	w.recent = append(w.recent, msgs...)
	if n := len(w.recent); n > 200 {
		w.recent = slices.Clone(w.recent[n-200:])
	}
	w.buf = slices.Clone(w.buf[next-w.base:])
	w.base = next
	if msgs == nil {
		msgs = []agentlog.Message{}
	}
	return &messagesJSON{Messages: msgs, NextOffset: next, Size: next + int64(len(w.buf)), State: w.inferState()}
}

func (w *transcriptWatch) inferState() string {
	return agentlog.InferState(w.agent, w.recent, w.live, w.lastActivity, time.Now())
}

// ---------------------------------------------------------------- control

// startAgentReq is the body of POST /v1/workers/{worker}/sessions.
type startAgentReq struct {
	Name   string   `json:"name"`
	Agent  string   `json:"agent"`
	Cwd    string   `json:"cwd"`
	Resume string   `json:"resume"`
	Fork   bool     `json:"fork"`
	Argv   []string `json:"argv"`
	Cols   uint32   `json:"cols"`
	Rows   uint32   `json:"rows"`
}

// command is what the session runs: the agent (resuming / forking a
// conversation), argv, or nil (the worker user's shell).
func (r startAgentReq) command() ([]string, error) {
	switch {
	case r.Agent != "" && len(r.Argv) > 0:
		return nil, usagef("give agent or argv, not both")
	case r.Agent == "":
		if r.Resume != "" || r.Fork {
			return nil, usagef("resume and fork go with agent")
		}
		return r.Argv, nil
	case r.Resume != "" && !sessionIDRE.MatchString(r.Resume):
		return nil, usagef("resume %q: not an agent session id", r.Resume)
	case r.Fork && r.Resume == "":
		return nil, usagef("fork goes with resume")
	}
	switch r.Agent {
	case agentlog.Claude:
		if r.Resume == "" {
			return []string{"claude"}, nil
		}
		argv := claudeAdapter{}.resumeArgv(r.Resume)
		if r.Fork {
			argv = append(argv, "--fork-session")
		}
		return argv, nil
	case agentlog.Copilot:
		if r.Fork {
			return nil, usagef("copilot cannot fork a conversation: end the other copilot first, then resume")
		}
		if r.Resume == "" {
			return []string{"copilot"}, nil
		}
		return copilotAdapter{}.resumeArgv(r.Resume), nil
	}
	return nil, usagef("agent %q: want claude or copilot", r.Agent)
}

var sessionNameUnsafe = regexp.MustCompile(`[^0-9A-Za-z_-]+`)

// sessionName is the name given, else "<agent or program>-<4 hex>".
func (r startAgentReq) sessionName(argv []string) string {
	if r.Name != "" {
		return r.Name
	}
	p := r.Agent
	if p == "" {
		p = strings.TrimSuffix(strings.ToLower(argvTitle(argv)), ".exe")
	}
	p = sessionNameUnsafe.ReplaceAllString(p, "")
	if p == "" || len(p) > 32 {
		p = "session"
	}
	b := make([]byte, 2)
	rand.Read(b)
	return p + "-" + hex.EncodeToString(b)
}

// startAgentSession starts a Longarm session; agent credentials go along
// as with session start --agent-auth auto.
func (d *daemon) startAgentSession(ctx context.Context, worker string, r startAgentReq) (*pb.AttachStarted, error) {
	argv, err := r.command()
	if err != nil {
		return nil, err
	}
	req := &pb.StartSessionRequest{Name: r.sessionName(argv), Argv: argv, Cwd: r.Cwd}
	if r.Cols > 0 && r.Rows > 0 {
		req.Size = &pb.TermSize{Cols: r.Cols, Rows: r.Rows}
	}
	var conn *client.Conn
	if agentOf(argv) != "" {
		// a connection of its own: forwarding credentials pins it to the node checked
		if conn, err = d.s.conn(worker); err != nil {
			return nil, err
		}
		defer conn.Close()
		if req.AgentAuth, req.Env, req.SecretEnv, err = forwardAgentAuth(ctx, d.s, conn, worker, argv,
			agentAuthOpts{modes: []string{"auto"}}, nil, nil); err != nil {
			return nil, err
		}
		defer clear(req.SecretEnv)
	} else if conn, err = d.conn(worker); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, err := conn.Sessions().Start(ctx, req)
	if err != nil {
		return nil, tooOld(err, conn, "session start")
	}
	return res, nil
}

// sendReq is the body of .../send.
type sendReq struct {
	Text     string   `json:"text"`
	Keys     []string `json:"keys"`
	IfIdleMs uint32   `json:"if_idle_ms"`
	Raw      bool     `json:"raw"`
}

// writes are the terminal writes of a send: the text (a newline is Enter
// unless raw), then each key on its own - a TUI takes an Enter that comes
// with the text as part of a paste.
func (r sendReq) writes() ([][]byte, error) {
	var out [][]byte
	if r.Text != "" {
		b := []byte(r.Text)
		if !r.Raw {
			b = typed(b)
		}
		out = append(out, b)
	}
	for _, k := range r.Keys {
		b, err := keyBytes(k)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return nil, usagef("nothing to send: give text or keys")
	}
	if len(out[0]) > attach.MaxSend {
		return nil, usagef("at most %d bytes per send", attach.MaxSend)
	}
	return out, nil
}

// sendSession types into a session (job: a steerable job of id name);
// if_idle_ms guards the first write (the others follow it at once).
// Returns the input_seq after the last.
func (d *daemon) sendSession(ctx context.Context, worker, name string, job bool, r sendReq) (uint64, error) {
	writes, err := r.writes()
	if err != nil {
		return 0, err
	}
	conn, err := d.conn(worker)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var seq uint64
	for i, b := range writes {
		req := &pb.SendSessionRequest{Name: name, Job: job, Data: b}
		if i == 0 {
			req.IfIdleMs = r.IfIdleMs
		}
		resp, err := conn.Sessions().Send(ctx, req)
		clear(b)
		if status.Code(err) == codes.ResourceExhausted {
			return 0, errNotIdle
		}
		if err != nil {
			return 0, tooOld(err, conn, "session send")
		}
		seq = resp.InputSeq
	}
	return seq, nil
}

// errNotIdle: if_idle_ms refused the send (input arrived within the window).
var errNotIdle = &codedError{code: exitTempFail, err: errors.New("not idle")}

// sessionCall runs a call on the session {name} of {worker}.
func (d *daemon) sessionCall(r *http.Request, what string, call func(context.Context, pb.SessionsClient, string) (*pb.Session, error)) (*pb.Session, error) {
	conn, err := d.conn(r.PathValue("worker"))
	if err != nil {
		return nil, err
	}
	ss, err := call(r.Context(), conn.Sessions(), r.PathValue("name"))
	return ss, tooOld(err, conn, what)
}

// ---------------------------------------------------------------- HTTP

// agentStatus maps an error to the HTTP status of the agent routes: 400
// bad input, 404 no such worker / session, 409 not idle (or ended,
// exists), 403 not allowed, 502 worker unreachable, 500 the rest.
func agentStatus(err error) int {
	var ce *codedError
	if s, ok := status.FromError(err); ok && !errors.As(err, &ce) {
		switch s.Code() {
		case codes.InvalidArgument, codes.OutOfRange, codes.Unimplemented:
			return http.StatusBadRequest
		case codes.NotFound:
			return http.StatusNotFound
		case codes.AlreadyExists, codes.FailedPrecondition, codes.ResourceExhausted:
			return http.StatusConflict
		case codes.PermissionDenied, codes.Unauthenticated:
			return http.StatusForbidden
		case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
			return http.StatusBadGateway
		}
		return http.StatusInternalServerError
	}
	switch exitCodeFor(err, true) {
	case exitUsage:
		return http.StatusBadRequest
	case exitNotFound:
		return http.StatusNotFound
	case exitTempFail:
		return http.StatusConflict
	case exitNoPerm:
		return http.StatusForbidden
	}
	return http.StatusBadGateway // dialing the worker
}

func agentError(w http.ResponseWriter, err error) {
	agentJSON(w, agentStatus(err), map[string]string{"error": status.Convert(err).Message()})
}

func agentJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// sessionJSON writes a session as protojson with the proto field names.
func sessionJSON(w http.ResponseWriter, code int, ss *pb.Session) {
	b, err := agentMO.Marshal(ss)
	if err != nil {
		agentError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(b)
}

// readJSON decodes a request body (empty: v unchanged).
func readJSON(r *http.Request, v any) error {
	err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
	if err != nil && !errors.Is(err, io.EOF) {
		return usagef("request body: %v", err)
	}
	return nil
}

func queryFlag(q url.Values, k string) bool { v := q.Get(k); return v == "1" || v == "true" }

// queryInt is an integer parameter (given false: absent).
func queryInt(q url.Values, k string) (n int64, given bool, err error) {
	v := q.Get(k)
	if v == "" {
		return 0, false, nil
	}
	n, err = strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false, usagef("%s=%q: want an integer", k, v)
	}
	return n, true, nil
}

// sseJSON streams JSON events with the framing of localapi.SSEStream: a
// comment first, then "event: <kind>" / "data: <json>"; an error from run
// ends the stream with an "error" event, else "end".
func sseJSON(w http.ResponseWriter, r *http.Request, run func(ctx context.Context, send func(string, any) error) error) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(w, ": ok\n\n")
	fl.Flush()
	event := func(kind string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b); err != nil {
			return err
		}
		fl.Flush()
		return nil
	}
	if err := run(r.Context(), event); err != nil && r.Context().Err() == nil {
		event("error", map[string]string{"error": status.Convert(err).Message()})
		return
	}
	event("end", struct{}{})
}
