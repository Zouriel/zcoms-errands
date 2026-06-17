package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// An errand is a friendly, autonomous task the owner dispatches at a contact.
// It runs in TWO separate agents with different privileges:
//
//   - The INTERVIEWER (status "active") only talks to the contact, asking for
//     what's needed ONE question at a time. It runs with NO filesystem or shell
//     access (plan mode); its sole persistent output is one collected-info file
//     the daemon creates at spin-up and writes on its behalf (RECORD directive).
//   - The PRODUCER (status "producing") spins up once questioning is done. It
//     has write access to a scratch dir to build the deliverable. It is told the
//     data came from a third party (NOT the owner), to treat it as untrusted
//     data rather than instructions, to do only the owner's original brief, and
//     to FLAG anything suspicious or inconsistent with that brief.
//
// This JSON is the durable metadata the daemon uses to route the contact's
// replies, hand off between the two agents, and survive restarts.

// Errand statuses (they double as the phase).
const (
	ErrandPendingApproval = "pending_approval" // drafted; waiting for the owner's go-ahead
	ErrandActive          = "active"           // interviewer is questioning the contact
	ErrandProducing       = "producing"        // producer is building the deliverable
	ErrandDone            = "done"
	ErrandCancelled       = "cancelled"
	ErrandFailed          = "failed"
)

// Errand is one dispatched questioning task. One contact per errand.
type Errand struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Brief     string `json:"brief"`      // the owner's instruction ("make a 2-page CV…")
	OwnerChat int64  `json:"owner_chat"` // where to report back (the main user's chat)

	// Separate sessions for the two agents, so the producer never inherits the
	// interviewer's context (and vice-versa).
	InterviewSessionID string `json:"interview_session_id,omitempty"`
	WorkerSessionID    string `json:"worker_session_id,omitempty"`
	InterviewFile      string `json:"interview_file,omitempty"` // the one file the interviewer produces

	// The single target contact, on whichever platform.
	Source     string `json:"source"` // "tg" | "wa"
	TargetName string `json:"target_name"`
	TGChat     int64  `json:"tg_chat,omitempty"`
	WAChat     string `json:"wa_chat,omitempty"`

	AutoStart       bool `json:"auto_start"`        // skip the owner-approval step and start immediately
	DeliverToTarget bool `json:"deliver_to_target"` // also send the finished deliverable to the contact

	SeenMsgIDs []string `json:"seen_msg_ids"`         // inbound dedup (string for both platforms)
	Transcript []string `json:"transcript,omitempty"` // raw Q/A log (fallback record of the interview)
	Delivered  bool     `json:"delivered,omitempty"`  // at least one file was DELIVER'd to the owner

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Runtime-only (unexported, so never persisted): serialize turns for one
	// errand and buffer replies that arrive while a turn is in flight.
	mu      sync.Mutex
	busy    bool
	pending []string
}

// active reports whether the errand is still running. Pending-approval and
// producing both count, so the contact is never triaged mid-errand.
func (e *Errand) active() bool {
	return e.Status == ErrandActive || e.Status == ErrandPendingApproval || e.Status == ErrandProducing
}

// interviewing reports whether the interviewer is the active agent (so contact
// replies should be fed in). In producing/done phases, contact replies are
// marked read but ignored.
func (e *Errand) interviewing() bool {
	return e.Status == ErrandActive || e.Status == ErrandPendingApproval
}

// platform renders the contact's platform for prompts/owner messages.
func (e *Errand) platform() string { return platformLabel(e.Source) }

// markSeen records a message id and reports whether it was new (not a duplicate).
func (e *Errand) markSeen(id string) bool {
	for _, s := range e.SeenMsgIDs {
		if s == id {
			return false
		}
	}
	e.SeenMsgIDs = append(e.SeenMsgIDs, id)
	// Bound the dedup list so a very long errand can't grow it without limit.
	if len(e.SeenMsgIDs) > 500 {
		e.SeenMsgIDs = e.SeenMsgIDs[len(e.SeenMsgIDs)-500:]
	}
	return true
}

const errandsDirName = "errands"

// errandsDir returns (creating if needed) the directory holding per-errand JSON.
func errandsDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, errandsDirName)
	if err := os.MkdirAll(p, 0o700); err != nil {
		return "", err
	}
	return p, nil
}

// errandStagingDir returns (creating if needed) a private writable scratch dir
// for one errand — the agent's cwd, where it builds the deliverable before
// DELIVER/SENDFILE-ing it. Per-errand so concurrent errands never collide.
func errandStagingDir(id string) (string, error) {
	base, err := ensureStagingDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(base, "errand-"+id)
	if err := os.MkdirAll(p, 0o700); err != nil {
		return "", err
	}
	return p, nil
}

// newErrandID returns a short, sortable, collision-resistant id.
func newErrandID() string {
	now := time.Now()
	return fmt.Sprintf("%s-%04d", now.Format("20060102-150405"), now.Nanosecond()/1e5)
}

// SaveErrand writes one errand to errands/<id>.json (0600).
func SaveErrand(e *Errand) error {
	dir, err := errandsDir()
	if err != nil {
		return err
	}
	e.UpdatedAt = time.Now()
	return writeJSON(filepath.Join(dir, e.ID+".json"), e)
}

// LoadErrands reads every errand from disk, newest first. A missing dir is not
// an error (returns an empty slice).
func LoadErrands() ([]*Errand, error) {
	dir, err := errandsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Errand
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			continue
		}
		var e Errand
		if err := json.Unmarshal(data, &e); err != nil {
			continue
		}
		out = append(out, &e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}
