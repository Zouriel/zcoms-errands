package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestErrandAgentLogPersistsPathAndEntry(t *testing.T) {
	d := newTestComp(t)
	e := &Errand{
		ID:         "errand-log-test",
		Status:     ErrandActive,
		Brief:      "collect details",
		OwnerChat:  d.ownerChat,
		TargetName: "@contact",
		Source:     "tg",
		CreatedAt:  time.Now(),
	}

	d.logErrand(e, errandLogInterviewer, "agent_start", "prompt=%q", "hello")

	path := e.AgentLogs[errandLogInterviewer]
	if path == "" {
		t.Fatal("expected interviewer log path to be recorded")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "agent_start") || !strings.Contains(text, "prompt=\"hello\"") {
		t.Fatalf("unexpected log contents: %s", text)
	}
}
