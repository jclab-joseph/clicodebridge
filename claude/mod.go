package claude

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	bridge "github.com/jclab-joseph/clicodebridge"
	"github.com/jclab-joseph/clicodebridge/internal/core"
)

// The bridge mod (bridgemod/) runs inside the Claude Code process as a
// function-hooks plugin, loaded per session with --plugin-dir. It gives the
// library actuators that need no keystrokes: submitting a prompt
// ($.prompt.submit) and aborting the running turn ($.turn.abort), plus a
// structured "turn aborted" signal the Stop hook does not provide.
//
// The function hooks API is early access. A session that never hears the
// mod's hello (an older CLI, a policy that disables plugins, a protocol
// change) keeps using hooks and the keyboard, and reports that once.

// Only the plugin itself: the engine writes type files and a tsconfig.json
// into the folder when it loads it there, which must not be embedded.
//
//go:embed bridgemod/.claude-plugin/plugin.json bridgemod/hooks/hooks.json bridgemod/hooks/register.ts
var bridgeModFS embed.FS

// modProtocol is the protocol version the embedded mod speaks.
const modProtocol = 1

// Bridge selects whether sessions load the bridge mod.
type Bridge int

const (
	// BridgeAuto loads the mod and falls back to the keyboard without it.
	BridgeAuto Bridge = iota
	// BridgeOff never loads it.
	BridgeOff
)

var (
	modDirOnce sync.Once
	modDir     string
	modDirErr  error
)

// bridgeModDir extracts the embedded mod once per process.
func bridgeModDir() (string, error) {
	modDirOnce.Do(func() {
		dir, err := os.MkdirTemp("", "clicodebridge-mod-*")
		if err != nil {
			modDirErr = err
			return
		}
		modDirErr = fs.WalkDir(bridgeModFS, "bridgemod", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel("bridgemod", path)
			target := filepath.Join(dir, rel)
			if d.IsDir() {
				return os.MkdirAll(target, 0o755)
			}
			b, err := bridgeModFS.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, b, 0o644)
		})
		modDir = dir
	})
	return modDir, modDirErr
}

type modCmd struct {
	ID   string `json:"id"`
	Op   string `json:"op"`
	Text string `json:"text,omitempty"`
}

type modAck struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

type modEvent struct {
	Kind    string `json:"kind"`
	TurnID  string `json:"turnId"`
	Text    string `json:"text"`
	Reason  string `json:"reason"`
	Aborted bool   `json:"aborted"`
}

// modLink is the session's side of the mod protocol.
type modLink struct {
	mu      sync.Mutex
	ready   bool
	version string
	queue   []modCmd
	wake    chan struct{}
	acks    map[string]chan modAck
}

func newModLink() *modLink {
	return &modLink{wake: make(chan struct{}, 1), acks: map[string]chan modAck{}}
}

func (m *modLink) isReady() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ready
}

// reset forgets the connection; the relaunched process says hello again.
func (m *modLink) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ready = false
	m.queue = nil
	for id, ch := range m.acks {
		ch <- modAck{ID: id, Error: "the CLI was relaunched"}
		delete(m.acks, id)
	}
}

var errModUnavailable = errors.New("claude: bridge mod not connected")

// call sends a command and waits for its ack.
func (m *modLink) call(ctx context.Context, op, text string) error {
	m.mu.Lock()
	if !m.ready {
		m.mu.Unlock()
		return errModUnavailable
	}
	c := modCmd{ID: core.NewID(), Op: op, Text: text}
	ch := make(chan modAck, 1)
	m.acks[c.ID] = ch
	m.queue = append(m.queue, c)
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
	defer func() {
		m.mu.Lock()
		delete(m.acks, c.ID)
		m.mu.Unlock()
	}()
	select {
	case a := <-ch:
		if !a.OK {
			return fmt.Errorf("claude: bridge mod %s: %s", op, a.Error)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
		return fmt.Errorf("claude: bridge mod %s: no answer", op)
	}
}

// onModHook handles the mod's calls (hook server events "mod.*").
func (s *session) onModHook(ctx context.Context, event string, body []byte) []byte {
	m := s.mod
	switch event {
	case "mod.hello":
		var h struct {
			Protocol  int    `json:"protocol"`
			Version   string `json:"version"`
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(body, &h)
		if h.Protocol != modProtocol {
			s.Emit(bridge.Event{Type: bridge.EventNotice, Kind: "bridge_mod",
				Text: fmt.Sprintf("bridge mod speaks protocol %d, want %d; using hooks and the keyboard", h.Protocol, modProtocol)})
			return nil
		}
		m.mu.Lock()
		m.ready, m.version = true, h.Version
		m.mu.Unlock()
		s.setCLIVersion(h.Version)
		s.Emit(bridge.Event{Type: bridge.EventNotice, Kind: "bridge_mod", Text: "bridge mod connected (Claude Code " + h.Version + ")"})
	case "mod.poll":
		timer := time.NewTimer(20 * time.Second)
		defer timer.Stop()
		for {
			m.mu.Lock()
			if len(m.queue) > 0 {
				out, _ := json.Marshal(map[string]any{"commands": m.queue})
				m.queue = nil
				m.mu.Unlock()
				return out
			}
			m.mu.Unlock()
			select {
			case <-m.wake:
			case <-timer.C:
				return []byte(`{"commands":[]}`)
			case <-ctx.Done():
				return nil
			case <-s.stop:
				return []byte(`{"commands":[]}`)
			}
		}
	case "mod.ack":
		var a modAck
		if json.Unmarshal(body, &a) == nil {
			m.mu.Lock()
			if ch, ok := m.acks[a.ID]; ok {
				ch <- a
			}
			m.mu.Unlock()
		}
	case "mod.event":
		var e modEvent
		if json.Unmarshal(body, &e) != nil {
			return nil
		}
		switch e.Kind {
		case "turn.start":
			s.startTurn(e.Text)
		case "turn.complete":
			if e.Aborted {
				s.flushTranscript()
				s.endTurn("", true)
			} else {
				s.stopTurn(e.Text)
			}
		}
	}
	return nil
}
