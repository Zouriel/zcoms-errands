package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	errandLogInterviewer = "interviewer"
	errandLogProducer    = "producer"
	errandLogLifecycle   = "lifecycle"
)

func (d *comp) logErrand(e *Errand, role, event, format string, args ...any) {
	if e == nil {
		return
	}
	path, err := d.errandLogPath(e, role)
	if err != nil {
		log.Printf("[errand %s] couldn't open %s log: %v", e.ID, role, err)
		return
	}
	msg := strings.TrimSpace(fmt.Sprintf(format, args...))
	if msg == "" {
		msg = "(empty)"
	}
	line := fmt.Sprintf("[%s] %s: %s\n", time.Now().Format(time.RFC3339), event, msg)
	if err := appendFile(path, line); err != nil {
		log.Printf("[errand %s] couldn't write %s log: %v", e.ID, role, err)
	}
}

func (d *comp) errandLogPath(e *Errand, role string) (string, error) {
	role = strings.TrimSpace(strings.ToLower(role))
	if role == "" {
		role = errandLogLifecycle
	}
	if e.AgentLogs != nil {
		if path := e.AgentLogs[role]; path != "" {
			return path, nil
		}
	}
	staging, err := errandStagingDir(e.ID)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(staging, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, role+".log")
	d.mu.Lock()
	if e.AgentLogs == nil {
		e.AgentLogs = map[string]string{}
	}
	e.AgentLogs[role] = path
	d.mu.Unlock()
	return path, nil
}

func appendFile(path, text string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(text)
	return err
}
