package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A scheduled errand is an errand the owner asked to be dispatched at a specific
// future time. It is NOT an errand yet: it carries the same fields `errand start`
// takes, plus when to fire. When its time arrives the scheduler turns it into a
// real errand via the ordinary startErrand path (so a scheduled errand behaves
// exactly like one started by hand at that moment — approval draft by default,
// or straight into the conversation when AutoStart). The target is resolved at
// fire time, not now, so a `#index` target tracks the latest triage batch and a
// contact that isn't reachable yet can still be lined up.

// ScheduledErrand is the durable record of a future errand dispatch.
type ScheduledErrand struct {
	ID    string    `json:"id"`
	RunAt time.Time `json:"run_at"`

	// The same inputs startErrand takes, kept verbatim and resolved at fire time.
	Target          string `json:"target"`
	Brief           string `json:"brief"`
	DeliverToTarget bool   `json:"deliver_to_target"`
	AutoStart       bool   `json:"auto_start"`

	CreatedAt time.Time `json:"created_at"`
}

const scheduledDirName = "scheduled-errands"

// scheduledDir returns (creating if needed) the directory holding per-schedule JSON.
func scheduledDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, scheduledDirName)
	if err := os.MkdirAll(p, 0o700); err != nil {
		return "", err
	}
	return p, nil
}

// newScheduledID returns a short, sortable id, prefixed so it's easy to tell a
// scheduled errand apart from a live one in listings.
func newScheduledID() string {
	now := time.Now()
	return fmt.Sprintf("sch-%s-%04d", now.Format("20060102-150405"), now.Nanosecond()/1e5)
}

// SaveScheduled writes one scheduled errand to scheduled-errands/<id>.json (0600).
func SaveScheduled(s *ScheduledErrand) error {
	dir, err := scheduledDir()
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, s.ID+".json"), s)
}

// DeleteScheduled removes a scheduled errand's file (missing is not an error).
func DeleteScheduled(id string) error {
	dir, err := scheduledDir()
	if err != nil {
		return err
	}
	err = os.Remove(filepath.Join(dir, id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// LoadScheduled reads every scheduled errand from disk, soonest-first. A missing
// dir is not an error (returns an empty slice).
func LoadScheduled() ([]*ScheduledErrand, error) {
	dir, err := scheduledDir()
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
	var out []*ScheduledErrand
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			continue
		}
		var s ScheduledErrand
		if err := json.Unmarshal(data, &s); err != nil {
			continue
		}
		out = append(out, &s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunAt.Before(out[j].RunAt) })
	return out, nil
}

// --- parsing ----------------------------------------------------------------

// parseErrandSchedule parses the part after "schedule":
//
//	[deliver] [go] <target> [at|in|on] <when> | <brief>
//
// The target is always a single token (@user, wa:JID, a numeric id, or #index),
// so everything between it and the "|" is the time. The optional at/in/on word
// is sugar and is stripped. Flags must come before the target.
func parseErrandSchedule(s string, now time.Time) (ErrandSpec, time.Time, error) {
	usage := "usage: errand schedule [deliver] [go] <@user|wa:JID|#index> at <time> | <brief>"
	i := strings.Index(s, "|")
	if i < 0 {
		return ErrandSpec{}, time.Time{}, errors.New(usage)
	}
	spec := ErrandSpec{Brief: strings.TrimSpace(s[i+1:])}
	toks := strings.Fields(strings.TrimSpace(s[:i]))

	// Leading flags, then the target token, then the time.
	n := 0
	for n < len(toks) {
		switch strings.ToLower(toks[n]) {
		case "deliver":
			spec.DeliverToTarget = true
		case "go", "auto":
			spec.AutoStart = true
		default:
			goto target
		}
		n++
	}
target:
	if n >= len(toks) {
		return spec, time.Time{}, fmt.Errorf("no target contact given — %s", usage)
	}
	spec.Target = toks[n]
	n++
	// A bare time keyword in the target slot means the contact was left out.
	switch strings.ToLower(spec.Target) {
	case "at", "in", "on":
		return spec, time.Time{}, fmt.Errorf("no target contact given — %s", usage)
	}

	whenToks := toks[n:]
	if len(whenToks) > 0 {
		switch strings.ToLower(whenToks[0]) {
		case "at", "in", "on":
			whenToks = whenToks[1:]
		}
	}
	whenStr := strings.TrimSpace(strings.Join(whenToks, " "))
	if whenStr == "" {
		return spec, time.Time{}, fmt.Errorf("no time given — %s", usage)
	}
	runAt, err := parseWhen(whenStr, now)
	if err != nil {
		return spec, time.Time{}, err
	}
	return spec, runAt, nil
}

// parseWhen turns a human time expression into an absolute instant, relative to
// now. It accepts a relative duration (+30m, 90m, 1h30m), a wall-clock time
// today/tomorrow (15:30), or a full local timestamp (2026-06-18T15:30,
// 2026-06-18 15:30, optionally with seconds). Absolute times are read in the
// machine's local zone; an absolute time already in the past is rejected.
func parseWhen(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("no time given")
	}
	if strings.EqualFold(s, "now") {
		return now, nil
	}

	// Relative duration: "+30m" or a bare "30m" / "1h30m".
	dur := strings.TrimPrefix(s, "+")
	if d, err := time.ParseDuration(dur); err == nil {
		if d < 0 {
			return time.Time{}, fmt.Errorf("a scheduled time can't be in the past (%q)", s)
		}
		return now.Add(d), nil
	}

	loc := now.Location()

	// Full local timestamps.
	for _, layout := range []string{
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
	} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			if t.Before(now) {
				return time.Time{}, fmt.Errorf("%s is in the past", t.Format("Mon 02 Jan 15:04"))
			}
			return t, nil
		}
	}

	// Wall-clock time only: today if still ahead, else tomorrow.
	for _, layout := range []string{"15:04", "15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			res := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc)
			if !res.After(now) {
				res = res.AddDate(0, 0, 1)
			}
			return res, nil
		}
	}

	return time.Time{}, fmt.Errorf("couldn't understand the time %q — try +30m, 15:30, or 2026-06-18T15:30", s)
}

// humanDuration renders a duration as a compact "1d 2h 3m" (minute resolution),
// for "fires in …" lines. Sub-minute rounds to "<1m".
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	d = d.Round(time.Minute)
	mins := int(d / time.Minute)
	days, mins := mins/(24*60), mins%(24*60)
	hours, mins := mins/60, mins%60
	var parts []string
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if mins > 0 {
		parts = append(parts, fmt.Sprintf("%dm", mins))
	}
	return strings.Join(parts, " ")
}

// --- lifecycle ---------------------------------------------------------------

// schedulerTick is how often the scheduler checks for due errands. The fire time
// is therefore accurate to within one tick, which is plenty for human-scheduled
// tasks and keeps the design restart-proof (a past-due time fires on next tick).
const schedulerTick = 15 * time.Second

// runScheduler dispatches scheduled errands whose time has come. It also catches
// up anything that fell due while the component was down (their RunAt is already
// in the past, so they fire on the first tick).
func (d *comp) runScheduler() {
	for {
		d.fireDueScheduled(time.Now())
		time.Sleep(schedulerTick)
	}
}

// fireDueScheduled claims every due schedule and dispatches each outside the lock.
func (d *comp) fireDueScheduled(now time.Time) {
	for _, s := range d.claimDueScheduled(now) {
		d.fireScheduled(s)
	}
}

// claimDueScheduled removes and returns every schedule whose time has come, under
// the lock — so a slow dispatch can never let the same one fire twice.
func (d *comp) claimDueScheduled(now time.Time) []*ScheduledErrand {
	d.mu.Lock()
	defer d.mu.Unlock()
	var due []*ScheduledErrand
	for id, s := range d.scheduled {
		if !s.RunAt.After(now) {
			due = append(due, s)
			delete(d.scheduled, id)
		}
	}
	return due
}

// fireScheduled turns one due schedule into a real errand and reports it.
func (d *comp) fireScheduled(s *ScheduledErrand) {
	_ = DeleteScheduled(s.ID)

	late := ""
	if behind := time.Since(s.RunAt); behind > 90*time.Second {
		late = fmt.Sprintf(" (was due %s ago)", humanDuration(behind))
	}

	reply, err := d.startErrand(ErrandSpec{
		Target:          s.Target,
		Brief:           s.Brief,
		DeliverToTarget: s.DeliverToTarget,
		AutoStart:       s.AutoStart,
	})
	if err != nil {
		d.send(d.ownerChat, fmt.Sprintf("⚠️ Scheduled errand %s → %s didn't start%s: %v",
			s.ID, s.Target, late, err))
		return
	}
	d.send(d.ownerChat, fmt.Sprintf("⏰ Scheduled errand %s firing now%s.\n%s", s.ID, late, reply))
}

// scheduleErrand records a future errand and returns a confirmation line.
func (d *comp) scheduleErrand(spec ErrandSpec, runAt time.Time) (string, error) {
	if d.ownerChat == 0 {
		return "", fmt.Errorf("no main user resolved — set main_user in agent-settings.json so I know where to report back")
	}
	if strings.TrimSpace(spec.Target) == "" {
		return "", fmt.Errorf("a scheduled errand needs a target contact")
	}
	if strings.TrimSpace(spec.Brief) == "" {
		return "", fmt.Errorf("a scheduled errand needs a brief (what should I ask / produce?)")
	}

	s := &ScheduledErrand{
		ID:              newScheduledID(),
		RunAt:           runAt,
		Target:          strings.TrimSpace(spec.Target),
		Brief:           strings.TrimSpace(spec.Brief),
		DeliverToTarget: spec.DeliverToTarget,
		AutoStart:       spec.AutoStart,
		CreatedAt:       time.Now(),
	}
	d.mu.Lock()
	d.scheduled[s.ID] = s
	d.mu.Unlock()
	if err := SaveScheduled(s); err != nil {
		return "", fmt.Errorf("couldn't save the schedule: %w", err)
	}

	verb := "draft a plan for your approval"
	if s.AutoStart {
		verb = "start messaging immediately"
	}
	return fmt.Sprintf("⏰ Scheduled errand %s → %s for %s (in %s). It will %s when it fires; `errand unschedule %s` to cancel.",
		s.ID, s.Target, runAt.Format("Mon 02 Jan 15:04"), humanDuration(time.Until(runAt)), verb, s.ID), nil
}

// unschedule drops a pending scheduled errand before it fires.
func (d *comp) unschedule(id string) bool {
	d.mu.Lock()
	_, ok := d.scheduled[id]
	if ok {
		delete(d.scheduled, id)
	}
	d.mu.Unlock()
	if ok {
		_ = DeleteScheduled(id)
	}
	return ok
}

// scheduledListText renders the pending scheduled errands, soonest first.
func (d *comp) scheduledListText() string {
	d.mu.Lock()
	list := make([]*ScheduledErrand, 0, len(d.scheduled))
	for _, s := range d.scheduled {
		list = append(list, s)
	}
	d.mu.Unlock()
	if len(list) == 0 {
		return "No scheduled errands."
	}
	sort.Slice(list, func(i, j int) bool { return list[i].RunAt.Before(list[j].RunAt) })

	var b strings.Builder
	b.WriteString("⏰ Scheduled errands:\n")
	for _, s := range list {
		fmt.Fprintf(&b, "  %s → %s at %s (in %s) — %s\n",
			s.ID, s.Target, s.RunAt.Format("Mon 02 Jan 15:04"), humanDuration(time.Until(s.RunAt)), snippet(s.Brief, 70))
	}
	return b.String()
}
