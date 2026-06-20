// Command zcoms-errands is the zcoms errands component: a standalone, pure-Go
// process that dispatches and drives autonomous interviewer→producer agents at a
// contact. It reaches Telegram through the core daemon's IPC socket (subscribe
// for the contact's replies, send/sendfile to message them) and WhatsApp through
// the Baileys sidecar. Errand commands arrive on its own socket (errands.sock),
// used by `zc errand …` and the bridge's `errand …` command.
package main

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Zouriel/zcoms-sdk/agent"
	"github.com/Zouriel/zcoms-sdk/ipc"
	"github.com/Zouriel/zcoms-sdk/whatsapp"
)

const waErrandPoll = 25 * time.Second

func main() {
	log.SetFlags(log.LstdFlags)
	log.Println("[errands] component starting")

	client, err := ipc.NewDefault()
	if err != nil {
		log.Fatalf("[errands] cannot resolve daemon socket: %v", err)
	}
	settings, _, err := agent.LoadOrSeedSettings()
	if err != nil {
		log.Fatalf("[errands] settings: %v", err)
	}
	agents, _, err := agent.LoadOrSeedAgents()
	if err != nil {
		log.Fatalf("[errands] agents: %v", err)
	}

	d := &comp{
		client:     client,
		waSocket:   settings.WhatsApp.Socket,
		waEnabled:  settings.WhatsApp.Enabled,
		agents:     agents,
		errands:    map[string]*Errand{},
		interviews: map[int64]*interview{},
		scheduled:  map[string]*ScheduledErrand{},
	}

	// Resolve the owner chat (where errands report back). Retry briefly in case
	// the daemon is still coming up alongside us.
	for i := 0; i < 30; i++ {
		if id, err := client.Resolve(settings.MainUser); err == nil {
			d.ownerChat = id
			break
		}
		time.Sleep(2 * time.Second)
	}
	if d.ownerChat == 0 {
		log.Println("[errands] warning: main_user not resolved yet; will resolve per-errand")
	}

	// Resume errands persisted before a restart, and re-publish their claims.
	if list, err := LoadErrands(); err == nil {
		for _, e := range list {
			d.errands[e.ID] = e
		}
	}
	d.syncClaims()

	// Resume scheduled errands; any that fell due while we were down fire on the
	// scheduler's first tick.
	if list, err := LoadScheduled(); err == nil {
		for _, s := range list {
			d.scheduled[s.ID] = s
		}
		if len(list) > 0 {
			log.Printf("[errands] resumed %d scheduled errand(s)", len(list))
		}
	}

	go d.runTGSubscribe()
	go d.runWAPoll()
	go d.runScheduler()
	serveCommands(d) // blocks
}

// runTGSubscribe streams the contact replies the daemon routes to us (claimed
// chats) and feeds them into the matching errand.
func (d *comp) runTGSubscribe() {
	for {
		err := d.client.Subscribe("errands", func(ev ipc.Event) {
			// A standup interview owns the chat first, if one is active.
			if d.feedInterview(ev.ChatID, ev.Text) {
				return
			}
			e := d.activeErrandForTG(ev.ChatID)
			if e == nil {
				return
			}
			d.mu.Lock()
			fresh := e.markSeen(strconv.FormatInt(ev.MessageID, 10))
			d.mu.Unlock()
			if !fresh {
				return
			}
			_ = SaveErrand(e)
			if !e.interviewing() {
				return
			}
			log.Printf("[errand %s] %s replied (Telegram)", e.ID, e.TargetName)
			d.feedErrand(e, ev.Text, ev.File)
		})
		log.Printf("[errands] TG subscription ended (%v); reconnecting…", err)
		time.Sleep(5 * time.Second)
	}
}

// runWAPoll polls the WhatsApp sidecar for replies to active WhatsApp errands.
func (d *comp) runWAPoll() {
	for {
		time.Sleep(waErrandPoll)
		if !d.waEnabled || !d.hasActiveWAErrand() {
			continue
		}
		unread, err := whatsapp.FetchUnread(d.waSocket)
		if err != nil {
			continue
		}
		handled := map[string][]string{}
		for _, u := range unread {
			e := d.activeErrandForWA(u.ChatID)
			if e == nil {
				continue
			}
			d.mu.Lock()
			fresh := e.markSeen(u.MsgID)
			d.mu.Unlock()
			handled[u.ChatID] = append(handled[u.ChatID], u.MsgID)
			if !fresh || !e.interviewing() {
				continue
			}
			_ = SaveErrand(e)
			log.Printf("[errand %s] %s replied (WhatsApp)", e.ID, e.TargetName)
			d.feedErrand(e, u.Text, u.File)
		}
		for jid, ids := range handled {
			_ = whatsapp.Dismiss(d.waSocket, jid, ids)
		}
	}
}

func (d *comp) activeErrandForTG(chatID int64) *Errand {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range d.errands {
		if e.Source != "wa" && e.TGChat == chatID && e.active() {
			return e
		}
	}
	return nil
}

func (d *comp) activeErrandForWA(jid string) *Errand {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range d.errands {
		if e.Source == "wa" && e.WAChat == jid && e.active() {
			return e
		}
	}
	return nil
}

func (d *comp) hasActiveWAErrand() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range d.errands {
		if e.Source == "wa" && e.active() {
			return true
		}
	}
	return false
}

// --- command socket (errands.sock) ------------------------------------------

type cmdRequest struct {
	Text      string         `json:"text"`                // a full "errand …" command line
	Interview *InterviewSpec `json:"interview,omitempty"` // a standup interview to conduct (from zc-team)
}

type cmdResponse struct {
	OK    bool   `json:"ok"`
	Reply string `json:"reply,omitempty"`
	Error string `json:"error,omitempty"`
}

func errandsSocketPath() string {
	dir, err := agent.DefaultAppDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "errands.sock")
	}
	return filepath.Join(dir, "errands.sock")
}

func serveCommands(d *comp) {
	path := errandsSocketPath()
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		log.Fatalf("[errands] cannot listen on %s: %v", path, err)
	}
	_ = os.Chmod(path, 0o600)
	log.Println("[errands] listening on", path)
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			defer func() {
				if r := recover(); r != nil {
					writeCmd(c, cmdResponse{Error: "internal error"})
				}
			}()
			line, err := bufio.NewReader(c).ReadBytes('\n')
			if err != nil && len(line) == 0 {
				return
			}
			var req cmdRequest
			if json.Unmarshal(line, &req) != nil {
				writeCmd(c, cmdResponse{Error: "bad request"})
				return
			}
			if req.Interview != nil {
				go d.runInterview(*req.Interview) // conducts + posts result to team.sock
				writeCmd(c, cmdResponse{OK: true})
				return
			}
			writeCmd(c, cmdResponse{OK: true, Reply: d.handleErrandCommand(req.Text)})
		}(conn)
	}
}

func writeCmd(c net.Conn, resp cmdResponse) {
	b, _ := json.Marshal(resp)
	_, _ = c.Write(append(b, '\n'))
}
