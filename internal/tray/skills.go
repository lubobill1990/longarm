package tray

import (
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/lubobill1990/longarm/internal/agentskill"
)

// The Longarm agent skill (internal/agentskill) in Settings: per coding
// agent whether it is detected and whether the skill is installed, with
// Install / Update / Remove. Nothing is written until the user clicks; the
// page only names an agent from the table, never a path.
//
//	GET  /trayapi/skills                     {"bundled_version", "agents": [Status...]}
//	POST /trayapi/skills/install {"agent": "claude"}   install or update; same answer
//	POST /trayapi/skills/remove  {"agent": "claude"}   remove Longarm's copy; same answer
//
// A refusal (a changed copy, a skill Longarm did not install) is 409 with
// the reason and the files in "error".

// skillEnv is the machine the skill section acts on (tests replace it).
var skillEnv = agentskill.DefaultEnv

var skillMu sync.Mutex // one install / remove at a time

type skillsAnswer struct {
	Bundled string              `json:"bundled_version"`
	Agents  []agentskill.Status `json:"agents"`
	Message string              `json:"message,omitempty"`
}

func (s *Server) skillsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /trayapi/skills", s.skillsState)
	mux.HandleFunc("POST /trayapi/skills/install", func(w http.ResponseWriter, r *http.Request) { s.skillsOp(w, r, true) })
	mux.HandleFunc("POST /trayapi/skills/remove", func(w http.ResponseWriter, r *http.Request) { s.skillsOp(w, r, false) })
}

func skillsList(msg string) (skillsAnswer, error) {
	all, err := skillEnv().StatusAll()
	return skillsAnswer{Bundled: agentskill.BundledVersion(), Agents: all, Message: msg}, err
}

func (s *Server) skillsState(w http.ResponseWriter, _ *http.Request) {
	skillMu.Lock()
	defer skillMu.Unlock()
	a, err := skillsList("")
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, a)
}

func (s *Server) skillsOp(w http.ResponseWriter, r *http.Request, install bool) {
	var body struct {
		Agent string `json:"agent"`
	}
	if !readBody(w, r, &body) {
		return
	}
	if _, ok := agentskill.Lookup(body.Agent); !ok {
		httpError(w, http.StatusBadRequest, "unknown agent")
		return
	}
	skillMu.Lock()
	defer skillMu.Unlock()
	env := skillEnv()
	op, verb := env.Remove, "removed"
	if install {
		op, verb = env.Install, "installed"
	}
	st, err := op(body.Agent)
	var re *agentskill.RefusedError
	switch {
	case errors.As(err, &re):
		httpError(w, http.StatusConflict, re.Error())
		return
	case err != nil:
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	msg := st.Name + ": " + verb
	if install {
		msg += " " + st.Installed
	} else if st.Note != "" {
		msg = st.Name + ": " + st.Note
	}
	s.opts.Log.Info("agent skill", "agent", body.Agent, "action", verb, "dir", st.Dir)
	a, err := skillsList(msg)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, a)
}

// RefreshSkills updates the skill copies Longarm installed and nobody
// changed, after Longarm itself was updated (the tray starts with the new
// version). Release builds only: a local build leaves them alone.
func RefreshSkills(log *slog.Logger, ver string) {
	if ver == "dev" || ver == "" {
		return
	}
	skillMu.Lock()
	defer skillMu.Unlock()
	up, err := skillEnv().Refresh()
	for _, st := range up {
		log.Info("agent skill updated", "agent", st.Agent, "version", st.Installed, "dir", st.Dir)
	}
	if err != nil {
		log.Warn("agent skill update", "err", err)
	}
}
