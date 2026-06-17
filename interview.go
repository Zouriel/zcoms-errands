package main

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Zouriel/zcoms-sdk/agent"
)

// InterviewSpec / InterviewResult mirror the team component's standup interview
// protocol (JSON-compatible). team dispatches a spec; errands conducts the
// scripted conversation and posts the result back to the callback socket.
type InterviewSpec struct {
	RunID     string              `json:"run_id"`
	StaffID   string              `json:"staff_id"`
	Target    string              `json:"target"`
	Greeting  string              `json:"greeting"`
	Closing   string              `json:"closing"`
	Callback  string              `json:"callback"`
	Questions []InterviewQuestion `json:"questions"`
}

type InterviewQuestion struct {
	TaskID       string `json:"task_id"`
	GithubItemID string `json:"github_item_id"`
	Title        string `json:"title"`
	Prompt       string `json:"prompt"`
}

type InterviewResult struct {
	RunID   string            `json:"run_id"`
	StaffID string            `json:"staff_id"`
	Answers []InterviewAnswer `json:"answers"`
}

type InterviewAnswer struct {
	TaskID       string `json:"task_id"`
	GithubItemID string `json:"github_item_id"`
	Title        string `json:"title"`
	Response     string `json:"response"`
}

// interview is one in-flight scripted conversation, keyed by the contact's chat.
type interview struct {
	spec    InterviewSpec
	chatID  int64
	replies chan string
}

const perQuestionWait = 30 * time.Minute

// runInterview conducts a scripted standup interview over Telegram (the daemon
// routes the claimed chat's replies to us) and posts the structured result back
// to the team component. Telegram targets only (standup staff use @usernames).
func (d *comp) runInterview(spec InterviewSpec) {
	chatID, err := d.client.Resolve(spec.Target)
	if err != nil {
		log.Printf("[interview] couldn't resolve %s: %v", spec.Target, err)
		d.postResult(spec, nil) // empty result so the run can finalize
		return
	}
	iv := &interview{spec: spec, chatID: chatID, replies: make(chan string, 4)}
	d.mu.Lock()
	d.interviews[chatID] = iv
	d.mu.Unlock()
	d.syncClaims() // claim the chat so the daemon routes replies here
	defer func() {
		d.mu.Lock()
		delete(d.interviews, chatID)
		d.mu.Unlock()
		d.syncClaims()
	}()

	to := strconv.FormatInt(chatID, 10)
	if spec.Greeting != "" {
		_, _ = d.client.Send(to, spec.Greeting)
	}
	var answers []InterviewAnswer
	for _, q := range spec.Questions {
		_, _ = d.client.Send(to, q.Prompt)
		resp := "(no response)"
		select {
		case r := <-iv.replies:
			resp = r
		case <-time.After(perQuestionWait):
		}
		answers = append(answers, InterviewAnswer{
			TaskID: q.TaskID, GithubItemID: q.GithubItemID, Title: q.Title, Response: resp,
		})
	}
	if spec.Closing != "" {
		_, _ = d.client.Send(to, spec.Closing)
	}
	d.postResult(spec, answers)
}

// feedInterview delivers a contact reply to an active interview (non-blocking).
func (d *comp) feedInterview(chatID int64, text string) bool {
	d.mu.Lock()
	iv := d.interviews[chatID]
	d.mu.Unlock()
	if iv == nil {
		return false
	}
	select {
	case iv.replies <- text:
	default:
	}
	return true
}

// postResult sends the collected answers back to the team component's socket.
func (d *comp) postResult(spec InterviewSpec, answers []InterviewAnswer) {
	dir, err := agent.DefaultAppDir()
	if err != nil {
		return
	}
	sock := spec.Callback
	if sock == "" {
		sock = "team.sock"
	}
	conn, err := net.DialTimeout("unix", filepath.Join(dir, sock), 3*time.Second)
	if err != nil {
		log.Printf("[interview] callback %s unreachable: %v", sock, err)
		return
	}
	defer conn.Close()
	payload, _ := json.Marshal(struct {
		Result InterviewResult `json:"result"`
	}{InterviewResult{RunID: spec.RunID, StaffID: spec.StaffID, Answers: answers}})
	_, _ = conn.Write(append(payload, '\n'))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _ = bufio.NewReader(conn).ReadBytes('\n')
}
