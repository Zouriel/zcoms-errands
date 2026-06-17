package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Zouriel/zcoms-sdk/agent"
	"github.com/Zouriel/zcoms-sdk/ipc"
)

// comp is the errands component's runtime: it owns the errand state and reaches
// Telegram through the core daemon over IPC and WhatsApp through the sidecar.
// It replaces the in-daemon receiver the ported errand logic was written against.
type comp struct {
	client    *ipc.Client
	waSocket  string
	waEnabled bool
	ownerChat int64
	agents    agent.AgentConfig

	mu         sync.Mutex
	errands    map[string]*Errand
	interviews map[int64]*interview // standup interviews in flight, keyed by chat
}

// --- IO the ported errand logic calls (TG via IPC, WA via the sidecar) -------

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

func (d *comp) send(chatID int64, text string) { _ = d.sendErr(chatID, text) }

func (d *comp) sendErr(chatID int64, text string) error {
	_, err := d.client.Send(itoa(chatID), text)
	return err
}

func (d *comp) sendFile(chatID int64, path, caption string) error {
	_, err := d.client.SendFile(itoa(chatID), path, caption)
	return err
}

func (d *comp) resolveChat(target string) (int64, int64, error) {
	id, err := d.client.Resolve(target)
	return id, id, err
}

// --- claims: tell the daemon/triage which chats this component owns ----------

// syncClaims writes claims.json from the currently-active errands so the daemon
// routes those chats here and triage skips them.
func (d *comp) syncClaims() {
	d.mu.Lock()
	var c agent.Claims
	for _, e := range d.errands {
		if !e.active() {
			continue
		}
		if e.Source == "wa" {
			c.WA = append(c.WA, e.WAChat)
		} else {
			c.TG = append(c.TG, e.TGChat)
		}
	}
	for cid := range d.interviews {
		c.TG = append(c.TG, cid) // standup interviews claim the staff member's chat
	}
	d.mu.Unlock()
	_ = agent.SaveClaims(c)
}

// --- lifecycle (ported from errand_daemon.go, returning reply strings) -------

// ErrandSpec is a request to start an errand before target resolution.
type ErrandSpec struct {
	Target          string
	Brief           string
	DeliverToTarget bool
	AutoStart       bool
}

func (d *comp) startErrand(spec ErrandSpec) (string, error) {
	if d.ownerChat == 0 {
		return "", fmt.Errorf("no main user resolved — set main_user in agent-settings.json so I know where to report back")
	}
	if strings.TrimSpace(spec.Brief) == "" {
		return "", fmt.Errorf("an errand needs a brief (what should I ask / produce?)")
	}
	e := &Errand{
		ID:              newErrandID(),
		Status:          ErrandPendingApproval,
		Brief:           strings.TrimSpace(spec.Brief),
		OwnerChat:       d.ownerChat,
		DeliverToTarget: spec.DeliverToTarget,
		AutoStart:       spec.AutoStart,
		CreatedAt:       time.Now(),
	}
	if spec.AutoStart {
		e.Status = ErrandActive
	}
	if err := d.resolveErrandTarget(e, spec.Target); err != nil {
		return "", err
	}
	d.mu.Lock()
	d.errands[e.ID] = e
	d.mu.Unlock()
	if err := SaveErrand(e); err != nil {
		return "", fmt.Errorf("couldn't save errand: %w", err)
	}
	d.syncClaims()
	d.kickErrand(e)
	verb := "Drafting a plan for your approval"
	if e.AutoStart {
		verb = "Starting now"
	}
	return fmt.Sprintf("🗂 Errand %s → %s (%s). %s.", e.ID, e.TargetName, e.platform(), verb), nil
}

func (d *comp) resolveErrandTarget(e *Errand, target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return fmt.Errorf("an errand needs a target contact")
	}
	if strings.HasPrefix(target, "#") {
		idx, err := strconv.Atoi(strings.TrimPrefix(target, "#"))
		if err != nil {
			return fmt.Errorf("bad batch index %q", target)
		}
		batch, err := loadTriageBatch()
		if err != nil {
			return fmt.Errorf("couldn't load the last triage batch: %w", err)
		}
		for _, r := range batch.Recipients {
			if r.Index == idx {
				e.Source, e.TargetName, e.TGChat, e.WAChat = r.Source, r.Name, r.TGChat, r.WAChat
				return nil
			}
		}
		return fmt.Errorf("no recipient #%d in the last triage batch", idx)
	}
	if strings.HasPrefix(target, "wa:") || strings.Contains(target, "@s.whatsapp.net") || strings.Contains(target, "@lid") {
		jid := strings.TrimPrefix(target, "wa:")
		e.Source, e.WAChat, e.TargetName = "wa", jid, jid
		if batch, err := loadTriageBatch(); err == nil {
			for _, r := range batch.Recipients {
				if r.Source == "wa" && r.WAChat == jid {
					e.TargetName = r.Name
					break
				}
			}
		}
		return nil
	}
	chatID, _, err := d.resolveChat(target)
	if err != nil {
		return fmt.Errorf("couldn't resolve %q: %w", target, err)
	}
	e.Source, e.TGChat, e.TargetName = "tg", chatID, target
	return nil
}

func (d *comp) pendingErrand(id string) *Errand {
	d.mu.Lock()
	defer d.mu.Unlock()
	var match *Errand
	count := 0
	for _, e := range d.errands {
		if e.Status != ErrandPendingApproval {
			continue
		}
		if id != "" && e.ID == id {
			return e
		}
		match = e
		count++
	}
	if id == "" && count == 1 {
		return match
	}
	return nil
}

func (d *comp) cancelErrand(id string) (*Errand, bool) {
	d.mu.Lock()
	e, ok := d.errands[id]
	if ok {
		e.Status = ErrandCancelled
	}
	d.mu.Unlock()
	if ok {
		_ = SaveErrand(e)
		d.syncClaims()
	}
	return e, ok
}

func (d *comp) errandExists(id string) bool {
	d.mu.Lock()
	_, ok := d.errands[id]
	d.mu.Unlock()
	return ok
}

func (d *comp) reviseErrandPlan(e *Errand, changes string) {
	prompt := fmt.Sprintf("The owner wants these changes before you start: %s\n\n%s",
		strings.TrimSpace(changes), errandApprovalPrompt(e))
	d.driveErrandAsync(e, prompt)
}

func (d *comp) errandListText() string {
	d.mu.Lock()
	var active []*Errand
	for _, e := range d.errands {
		if e.active() {
			active = append(active, e)
		}
	}
	d.mu.Unlock()
	if len(active) == 0 {
		return "No active errands."
	}
	var b strings.Builder
	b.WriteString("🗂 Active errands:\n")
	for _, e := range active {
		fmt.Fprintf(&b, "  %s → %s (%s) [%s] — %s\n", e.ID, e.TargetName, e.platform(), e.Status, snippet(e.Brief, 80))
	}
	return b.String()
}

func parseErrandStart(s string) (ErrandSpec, error) {
	i := strings.Index(s, "|")
	if i < 0 {
		return ErrandSpec{}, fmt.Errorf("usage: errand start [deliver] [go] <@user|wa:JID|#index> | <brief>")
	}
	left := strings.Fields(strings.TrimSpace(s[:i]))
	spec := ErrandSpec{Brief: strings.TrimSpace(s[i+1:])}
	var target string
	for _, tok := range left {
		switch strings.ToLower(tok) {
		case "deliver":
			spec.DeliverToTarget = true
		case "go", "now", "auto":
			spec.AutoStart = true
		default:
			if target == "" {
				target = tok
			} else {
				target += " " + tok
			}
		}
	}
	spec.Target = target
	if target == "" {
		return spec, fmt.Errorf("no target contact given")
	}
	return spec, nil
}

// handleErrandCommand parses an "errand …" command and performs it, returning
// the immediate reply for whoever issued it (the CLI or the bridge owner).
// Long-running progress is sent to the owner via d.send.
func (d *comp) handleErrandCommand(text string) string {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return d.errandListText() + "\n\nCommands: errand list · errand start [deliver] [go] <@user|wa:JID|#index> | <brief> · errand yes [id] · errand no [id] · errand edit [id] <changes> · errand cancel <id>"
	}
	switch strings.ToLower(fields[1]) {
	case "list":
		return d.errandListText()
	case "start":
		_, after, _ := strings.Cut(text, "start")
		spec, err := parseErrandStart(after)
		if err != nil {
			return "⚠️ " + err.Error()
		}
		msg, err := d.startErrand(spec)
		if err != nil {
			return "⚠️ " + err.Error()
		}
		return msg
	case "yes", "go", "approve":
		id := ""
		if len(fields) >= 3 {
			id = fields[2]
		}
		e := d.pendingErrand(id)
		if e == nil {
			return "No errand is waiting for approval (give an id if there are several): errand yes <id>"
		}
		d.approveErrand(e, "")
		return "👍 Starting errand " + e.ID + "…"
	case "no", "reject":
		id := ""
		if len(fields) >= 3 {
			id = fields[2]
		}
		e := d.pendingErrand(id)
		if e == nil {
			return "No errand is waiting for approval."
		}
		d.setErrandStatus(e, ErrandCancelled)
		return "🛑 Dropped errand " + e.ID + "."
	case "edit":
		rest := fields[2:]
		id := ""
		if len(rest) > 0 && d.errandExists(rest[0]) {
			id, rest = rest[0], rest[1:]
		}
		e := d.pendingErrand(id)
		if e == nil {
			return "No errand is waiting for approval to edit."
		}
		changes := strings.TrimSpace(strings.Join(rest, " "))
		if changes == "" {
			return "Usage: errand edit [id] <what to change>"
		}
		d.reviseErrandPlan(e, changes)
		return "✏️ Revising the plan for errand " + e.ID + "…"
	case "cancel":
		if len(fields) < 3 {
			return "Usage: errand cancel <id>"
		}
		if e, ok := d.cancelErrand(fields[2]); ok {
			return "🛑 Cancelled errand " + e.ID + ". " + e.TargetName + " won't be messaged further."
		}
		return "No errand with id " + fields[2] + "."
	default:
		return "Unknown errand command. Try: errand list · errand start … · errand yes · errand cancel <id>"
	}
}

// --- small helpers ported from core -----------------------------------------

func platformLabel(source string) string {
	if source == "wa" {
		return "WhatsApp"
	}
	return "Telegram"
}

func snippet(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func splitFileArg(rest string) (path, caption string) {
	if i := strings.Index(rest, "|"); i >= 0 {
		return strings.TrimSpace(rest[:i]), strings.TrimSpace(rest[i+1:])
	}
	return strings.TrimSpace(rest), ""
}

func configDir() (string, error) { return agent.DefaultAppDir() }

func ensureStagingDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "agent-staging")
	if err := os.MkdirAll(p, 0o700); err != nil {
		return "", err
	}
	return p, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// Recipient/TriageBatch mirror the triage component's last-triage.json (so
// `#index` errand targets resolve).
type Recipient struct {
	Index    int      `json:"index"`
	Source   string   `json:"source"`
	Name     string   `json:"name"`
	TGChat   int64    `json:"tg_chat,omitempty"`
	WAChat   string   `json:"wa_chat,omitempty"`
	Messages []string `json:"messages"`
	Files    []string `json:"files,omitempty"`
}

type TriageBatch struct {
	At         time.Time   `json:"at"`
	Recipients []Recipient `json:"recipients"`
}

func loadTriageBatch() (TriageBatch, error) {
	dir, err := configDir()
	if err != nil {
		return TriageBatch{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "last-triage.json"))
	if os.IsNotExist(err) {
		return TriageBatch{}, nil
	}
	if err != nil {
		return TriageBatch{}, err
	}
	var b TriageBatch
	if err := json.Unmarshal(data, &b); err != nil {
		return TriageBatch{}, err
	}
	return b, nil
}
