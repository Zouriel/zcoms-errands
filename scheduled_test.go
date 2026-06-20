package main

import (
	"testing"
	"time"
)

// fixed reference instant for deterministic time parsing: Thu 2026-06-18 10:00 local.
func refNow() time.Time {
	return time.Date(2026, 6, 18, 10, 0, 0, 0, time.Local)
}

func TestParseWhen(t *testing.T) {
	now := refNow()
	tests := []struct {
		name string
		in   string
		want time.Time
		err  bool
	}{
		{"plus duration", "+30m", now.Add(30 * time.Minute), false},
		{"bare duration", "90m", now.Add(90 * time.Minute), false},
		{"compound duration", "1h30m", now.Add(90 * time.Minute), false},
		{"hours", "+2h", now.Add(2 * time.Hour), false},
		{"now keyword", "now", now, false},
		{"clock later today", "15:30", time.Date(2026, 6, 18, 15, 30, 0, 0, time.Local), false},
		{"clock already passed rolls to tomorrow", "09:00", time.Date(2026, 6, 19, 9, 0, 0, 0, time.Local), false},
		{"full timestamp T", "2026-06-18T15:30", time.Date(2026, 6, 18, 15, 30, 0, 0, time.Local), false},
		{"full timestamp space", "2026-06-18 15:30", time.Date(2026, 6, 18, 15, 30, 0, 0, time.Local), false},
		{"full timestamp with seconds", "2026-06-18T15:30:45", time.Date(2026, 6, 18, 15, 30, 45, 0, time.Local), false},
		{"future date", "2026-06-20 08:00", time.Date(2026, 6, 20, 8, 0, 0, 0, time.Local), false},
		{"negative duration rejected", "+-5m", time.Time{}, true},
		{"absolute past rejected", "2026-06-18T09:00", time.Time{}, true},
		{"empty", "", time.Time{}, true},
		{"garbage", "sometime soon", time.Time{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseWhen(tt.in, now)
			if tt.err {
				if err == nil {
					t.Fatalf("parseWhen(%q) = %v, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseWhen(%q) unexpected error: %v", tt.in, err)
			}
			if !got.Equal(tt.want) {
				t.Fatalf("parseWhen(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseErrandSchedule(t *testing.T) {
	now := refNow()

	t.Run("basic", func(t *testing.T) {
		spec, runAt, err := parseErrandSchedule(" @bob at 15:30 | make a CV", now)
		if err != nil {
			t.Fatal(err)
		}
		if spec.Target != "@bob" {
			t.Errorf("target = %q, want @bob", spec.Target)
		}
		if spec.Brief != "make a CV" {
			t.Errorf("brief = %q, want 'make a CV'", spec.Brief)
		}
		if spec.AutoStart || spec.DeliverToTarget {
			t.Errorf("flags should default off, got autostart=%v deliver=%v", spec.AutoStart, spec.DeliverToTarget)
		}
		want := time.Date(2026, 6, 18, 15, 30, 0, 0, time.Local)
		if !runAt.Equal(want) {
			t.Errorf("runAt = %v, want %v", runAt, want)
		}
	})

	t.Run("flags before target", func(t *testing.T) {
		spec, _, err := parseErrandSchedule(" deliver go @bob in +2h | ship it", now)
		if err != nil {
			t.Fatal(err)
		}
		if !spec.DeliverToTarget || !spec.AutoStart {
			t.Errorf("expected both flags set, got deliver=%v autostart=%v", spec.DeliverToTarget, spec.AutoStart)
		}
		if spec.Target != "@bob" {
			t.Errorf("target = %q, want @bob", spec.Target)
		}
	})

	t.Run("relative without keyword", func(t *testing.T) {
		spec, runAt, err := parseErrandSchedule(" wa:1555 +45m | poll them", now)
		if err != nil {
			t.Fatal(err)
		}
		if spec.Target != "wa:1555" {
			t.Errorf("target = %q", spec.Target)
		}
		if !runAt.Equal(now.Add(45 * time.Minute)) {
			t.Errorf("runAt = %v, want %v", runAt, now.Add(45*time.Minute))
		}
	})

	t.Run("full timestamp with space survives target split", func(t *testing.T) {
		spec, runAt, err := parseErrandSchedule(" #2 at 2026-06-19 09:15 | follow up", now)
		if err != nil {
			t.Fatal(err)
		}
		if spec.Target != "#2" {
			t.Errorf("target = %q, want #2", spec.Target)
		}
		want := time.Date(2026, 6, 19, 9, 15, 0, 0, time.Local)
		if !runAt.Equal(want) {
			t.Errorf("runAt = %v, want %v", runAt, want)
		}
	})

	t.Run("missing pipe", func(t *testing.T) {
		if _, _, err := parseErrandSchedule("@bob at 15:30 make a CV", now); err == nil {
			t.Error("expected error when '|' is missing")
		}
	})

	t.Run("missing time", func(t *testing.T) {
		if _, _, err := parseErrandSchedule("@bob | make a CV", now); err == nil {
			t.Error("expected error when time is missing")
		}
	})

	t.Run("missing target", func(t *testing.T) {
		if _, _, err := parseErrandSchedule("at 15:30 | make a CV", now); err == nil {
			t.Error("expected error when target is missing")
		}
	})
}

func TestHumanDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "<1m"},
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1h 30m"},
		{2 * time.Hour, "2h"},
		{25 * time.Hour, "1d 1h"},
		{-time.Minute, "<1m"},
	}
	for _, tt := range tests {
		if got := humanDuration(tt.d); got != tt.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func newTestComp(t *testing.T) *comp {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // isolate ~/.config/zcoms persistence
	return &comp{
		ownerChat: 12345,
		errands:   map[string]*Errand{},
		scheduled: map[string]*ScheduledErrand{},
	}
}

func TestScheduleErrandPersistsAndReloads(t *testing.T) {
	d := newTestComp(t)
	runAt := refNow().Add(time.Hour)

	reply, err := d.scheduleErrand(ErrandSpec{Target: "@bob", Brief: "make a CV", AutoStart: true}, runAt)
	if err != nil {
		t.Fatal(err)
	}
	if reply == "" {
		t.Fatal("expected a confirmation reply")
	}
	if len(d.scheduled) != 1 {
		t.Fatalf("expected 1 scheduled in memory, got %d", len(d.scheduled))
	}

	// It must survive a restart: a fresh load reads the same record back.
	loaded, err := LoadScheduled()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 scheduled on disk, got %d", len(loaded))
	}
	got := loaded[0]
	if got.Target != "@bob" || got.Brief != "make a CV" || !got.AutoStart {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if !got.RunAt.Equal(runAt) {
		t.Errorf("RunAt = %v, want %v", got.RunAt, runAt)
	}
}

func TestScheduleRequiresOwnerAndBrief(t *testing.T) {
	d := newTestComp(t)
	d.ownerChat = 0
	if _, err := d.scheduleErrand(ErrandSpec{Target: "@bob", Brief: "x"}, refNow().Add(time.Hour)); err == nil {
		t.Error("expected error when no owner is resolved")
	}
	d.ownerChat = 1
	if _, err := d.scheduleErrand(ErrandSpec{Target: "@bob", Brief: "  "}, refNow().Add(time.Hour)); err == nil {
		t.Error("expected error when brief is blank")
	}
}

func TestClaimDueScheduledFiresOnlyPastDue(t *testing.T) {
	d := newTestComp(t)
	now := refNow()
	past := &ScheduledErrand{ID: "sch-past", RunAt: now.Add(-time.Minute), Target: "@a", Brief: "b"}
	exact := &ScheduledErrand{ID: "sch-now", RunAt: now, Target: "@a", Brief: "b"}
	future := &ScheduledErrand{ID: "sch-future", RunAt: now.Add(time.Hour), Target: "@a", Brief: "b"}
	d.scheduled[past.ID] = past
	d.scheduled[exact.ID] = exact
	d.scheduled[future.ID] = future

	due := d.claimDueScheduled(now)
	if len(due) != 2 {
		t.Fatalf("expected 2 due (past + exact), got %d", len(due))
	}
	// The claimed ones are removed; only the future one remains.
	if len(d.scheduled) != 1 {
		t.Fatalf("expected 1 left, got %d", len(d.scheduled))
	}
	if _, ok := d.scheduled[future.ID]; !ok {
		t.Error("future schedule should remain queued")
	}
}

func TestUnschedule(t *testing.T) {
	d := newTestComp(t)
	reply, err := d.scheduleErrand(ErrandSpec{Target: "@bob", Brief: "x"}, refNow().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_ = reply
	var id string
	for k := range d.scheduled {
		id = k
	}
	if !d.unschedule(id) {
		t.Fatal("unschedule returned false for an existing id")
	}
	if len(d.scheduled) != 0 {
		t.Errorf("expected map empty after unschedule, got %d", len(d.scheduled))
	}
	if loaded, _ := LoadScheduled(); len(loaded) != 0 {
		t.Errorf("expected file removed, got %d on disk", len(loaded))
	}
	if d.unschedule("sch-nope") {
		t.Error("unschedule should return false for an unknown id")
	}
}
