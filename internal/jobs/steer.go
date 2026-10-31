package jobs

import (
	"encoding/json"
	"os"
	"time"
)

// Files of a steerable job (Spec.Steer) in its directory.
const (
	// SteerFile is the shim's view of the agent (SteerState).
	SteerFile = "steer.json"
	// TermLog is the raw terminal of a steerable job (redacted, capped
	// like out.log); out.log gets the agent's answers.
	TermLog = "term.log"
	// SteerDir holds the hook configuration and events.jsonl, which the
	// agent's hooks append to.
	SteerDir = "steer"
)

// Steer states (SteerState.State).
const (
	SteerStarting  = "starting"  // until the first prompt is submitted
	SteerWorking   = "working"   // a turn is running
	SteerNeedsYou  = "needs_you" // a permission prompt (or a start-up screen) waits for a human
	SteerIdle      = "idle"      // the turn finished; not done yet (section 4 of the spec)
	SteerFinishing = "finishing" // /exit was typed
	SteerDone      = "done"      // the agent exited
)

// SteerState is what steer.json holds.
type SteerState struct {
	State      string    `json:"state"`
	Turns      int       `json:"turns,omitempty"`
	Message    string    `json:"message,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	Transcript string    `json:"transcript,omitempty"`
	UpdatedAt  time.Time `json:"updated_at,omitzero"`
}

// WriteSteer writes steer.json.
func (d Dir) WriteSteer(st SteerState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return d.WriteAtomic(SteerFile, b)
}

// ReadSteer reads steer.json (nil when there is none).
func (d Dir) ReadSteer() *SteerState {
	b, err := os.ReadFile(d.Path(SteerFile))
	if err != nil {
		return nil
	}
	var st SteerState
	if json.Unmarshal(b, &st) != nil {
		return nil
	}
	return &st
}
