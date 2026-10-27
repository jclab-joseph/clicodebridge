//go:build e2e

// Real end-to-end tests against the installed claude CLI. They spend tokens
// and leave transcripts under ~/.claude/projects, so they only run with
//
//	CLICODEBRIDGE_E2E_DIR=<scratch dir> go test -tags e2e ./claude -run E2E -v
//
// The scratch directories are trusted (via the trust dialog) by the tests.
package claude

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bridge "github.com/jclab-joseph/clicodebridge"
)

type eventLog struct {
	t      *testing.T
	ch     <-chan bridge.Event
	bridge string // last bridge_mod notice not yet consumed by waitBridge
}

func (l *eventLog) wait(timeout time.Duration, what string, pred func(bridge.Event) bool) bridge.Event {
	l.t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-l.ch:
			if !ok {
				l.t.Fatalf("event stream closed while waiting for %s", what)
			}
			l.t.Logf("event %s %s %s %s", ev.Type, ev.State, ev.Kind, trimForLog(ev.Text))
			if ev.Type == bridge.EventNotice && ev.Kind == "bridge_mod" {
				l.bridge = ev.Text
			}
			if ev.Type == bridge.EventDiagnostic {
				l.t.Errorf("drift diagnostic: %s\n%s", ev.Text, ev.Screen)
			}
			if pred(ev) {
				return ev
			}
		case <-deadline:
			l.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func trimForLog(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

func is(typ bridge.EventType) func(bridge.Event) bool {
	return func(ev bridge.Event) bool { return ev.Type == typ }
}

func e2eDir(t *testing.T, name string) string {
	base := os.Getenv("CLICODEBRIDGE_E2E_DIR")
	if base == "" {
		t.Skip("set CLICODEBRIDGE_E2E_DIR to run real e2e tests")
	}
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func startE2E(t *testing.T, dir string, co *Options) (bridge.Session, *eventLog) {
	t.Helper()
	p := New(Config{})
	t.Cleanup(func() { p.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := p.Start(ctx, bridge.SessionOptions{Cwd: dir, Model: "haiku", Mode: ModeManual, Provider: co})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, &eventLog{t: t, ch: s.Events()}
}

// waitIdle answers a trust request if one shows up, then waits for idle.
func waitIdle(t *testing.T, s bridge.Session, log *eventLog) {
	t.Helper()
	log.wait(90*time.Second, "idle", func(ev bridge.Event) bool {
		if ev.Type == bridge.EventRequest && ev.Request.Title == "Trust this folder?" {
			if err := s.Respond(context.Background(), ev.Request.ID, bridge.Allow()); err != nil {
				t.Fatal(err)
			}
		}
		return ev.Type == bridge.EventState && ev.State == bridge.StateIdle
	})
}

// waitBridge waits for the bridge mod's handshake notice.
func waitBridge(t *testing.T, log *eventLog) {
	t.Helper()
	if log.bridge == "" {
		log.wait(40*time.Second, "bridge mod", func(ev bridge.Event) bool {
			return ev.Type == bridge.EventNotice && ev.Kind == "bridge_mod"
		})
	}
	if !strings.Contains(log.bridge, "connected") || strings.Contains(log.bridge, "did not") {
		t.Fatalf("bridge mod: %s", log.bridge)
	}
	log.bridge = ""
}

func send(t *testing.T, s bridge.Session, prompt string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Send(ctx, prompt); err != nil {
		t.Fatalf("send: %v\nscreen:\n%s", err, s.Terminal().Screen().Text())
	}
}

func respond(t *testing.T, s bridge.Session, id string, r bridge.Response) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := s.Respond(ctx, id, r); err != nil {
		t.Fatalf("respond: %v\nscreen:\n%s", err, s.Terminal().Screen().Text())
	}
}

// interrupt starts a long turn and stops it.
func interrupt(t *testing.T, s bridge.Session, log *eventLog) {
	t.Helper()
	send(t, s, "Write a 3000-word essay about the history of terminals.")
	log.wait(30*time.Second, "turn start", is(bridge.EventTurnStarted))
	time.Sleep(3 * time.Second)
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := log.wait(30*time.Second, "interrupted turn", is(bridge.EventTurnCompleted))
	if !done.Interrupted {
		t.Fatalf("turn not reported interrupted: %+v", done)
	}
}

func TestE2EHookInteraction(t *testing.T) {
	dir := e2eDir(t, "hook")
	home, _ := os.UserHomeDir()
	settingsBefore, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))

	s, log := startE2E(t, dir, &Options{})
	waitIdle(t, s, log)
	waitBridge(t, log)

	send(t, s, "Reply with exactly the word PONG and nothing else.")
	done := log.wait(60*time.Second, "turn end", is(bridge.EventTurnCompleted))
	if !strings.Contains(done.Text, "PONG") {
		t.Fatalf("final text = %q", done.Text)
	}

	os.Remove(filepath.Join(dir, "f.txt"))
	send(t, s, "Run this exact shell command with the Bash tool: echo e2e > f.txt")
	req := log.wait(60*time.Second, "permission request", is(bridge.EventRequest)).Request
	if req.Kind != bridge.RequestPermission || req.Permission.ToolName != "Bash" || !strings.Contains(req.Permission.Detail, "echo e2e") {
		t.Fatalf("request = %+v", req)
	}
	respond(t, s, req.ID, bridge.Allow())
	log.wait(60*time.Second, "turn end", is(bridge.EventTurnCompleted))
	if b, err := os.ReadFile(filepath.Join(dir, "f.txt")); err != nil || strings.TrimSpace(string(b)) != "e2e" {
		t.Fatalf("f.txt = %q %v", b, err)
	}

	send(t, s, `Use the AskUserQuestion tool once with one question: header "Size", question "Which size?", options Small and Large, single select. Then reply with only my answer.`)
	req = log.wait(60*time.Second, "question", is(bridge.EventRequest)).Request
	if req.Kind != bridge.RequestQuestion || len(req.Questions) != 1 {
		t.Fatalf("request = %+v", req)
	}
	respond(t, s, req.ID, bridge.Choose("Large"))
	done = log.wait(60*time.Second, "turn end", is(bridge.EventTurnCompleted))
	if !strings.Contains(done.Text, "Large") {
		t.Fatalf("final text = %q", done.Text)
	}

	interrupt(t, s, log)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := s.SetMode(ctx, ModeAcceptEdits); err != nil {
		t.Fatal(err)
	}
	if m := s.Settings().Mode; m != ModeAcceptEdits {
		t.Fatalf("mode = %q", m)
	}
	if err := s.SetMode(ctx, ModeManual); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEffort(ctx, "low"); err != nil {
		t.Fatal(err)
	}
	log.bridge = ""
	log.wait(10*time.Second, "restart", is(bridge.EventRestarted))
	waitBridge(t, log) // the relaunched process says hello again
	if e := s.Settings().Effort; e != "low" {
		t.Fatalf("effort = %q", e)
	}
	send(t, s, "Reply with exactly the word AGAIN.")
	done = log.wait(60*time.Second, "turn end after restart", is(bridge.EventTurnCompleted))
	if !strings.Contains(done.Text, "AGAIN") {
		t.Fatalf("final text = %q", done.Text)
	}
	settingsAfter, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if !bytes.Equal(settingsBefore, settingsAfter) {
		t.Fatal("user settings.json was modified")
	}
}

// terminalScenario answers a two-question dialog (single select, then
// multi-select with free text) and a permission prompt by keyboard.
func terminalScenario(t *testing.T, dir string, co *Options) {
	s, log := startE2E(t, dir, co)
	waitIdle(t, s, log)
	if co.Bridge != BridgeOff {
		waitBridge(t, log)
	}

	send(t, s, `Use the AskUserQuestion tool once with two questions: (1) header "Color", question "Which color?", single select, options Red, Green, Blue. (2) header "Pets", question "Which pets?", multiSelect true, options Cat, Dog, Fish. Then reply with only my answers.`)
	req := log.wait(60*time.Second, "question", is(bridge.EventRequest)).Request
	if req.Kind != bridge.RequestQuestion || len(req.Questions) != 2 || !req.Questions[1].MultiSelect {
		t.Fatalf("request = %+v", req)
	}
	respond(t, s, req.ID, bridge.Response{Answers: []bridge.Answer{
		{Selected: []string{"Green"}},
		{Selected: []string{"Cat", "Fish"}, Text: "Hamster"},
	}})
	done := log.wait(60*time.Second, "turn end", is(bridge.EventTurnCompleted))
	for _, want := range []string{"Green", "Cat", "Fish", "Hamster"} {
		if !strings.Contains(done.Text, want) {
			t.Fatalf("final text %q lacks %q", done.Text, want)
		}
	}

	send(t, s, `Use the AskUserQuestion tool once with one question: header "Drink", question "Which drink?", single select, options Tea, Coffee. Then reply with only my answer.`)
	req = log.wait(60*time.Second, "question", is(bridge.EventRequest)).Request
	respond(t, s, req.ID, bridge.Response{Answers: []bridge.Answer{{Text: "Mate"}}})
	done = log.wait(60*time.Second, "turn end", is(bridge.EventTurnCompleted))
	if !strings.Contains(done.Text, "Mate") {
		t.Fatalf("final text %q", done.Text)
	}

	os.Remove(filepath.Join(dir, "g.txt"))
	send(t, s, "Run this exact shell command with the Bash tool: echo term > g.txt")
	req = log.wait(60*time.Second, "permission", is(bridge.EventRequest)).Request
	respond(t, s, req.ID, bridge.Allow())
	log.wait(60*time.Second, "turn end", is(bridge.EventTurnCompleted))
	if b, err := os.ReadFile(filepath.Join(dir, "g.txt")); err != nil || strings.TrimSpace(string(b)) != "term" {
		t.Fatalf("g.txt = %q %v", b, err)
	}

	interrupt(t, s, log)
}

func TestE2ETerminalInteraction(t *testing.T) {
	terminalScenario(t, e2eDir(t, "terminal"), &Options{Interaction: InteractionTerminal, TrustWorkspace: true})
}

func TestE2EKeyboardOnly(t *testing.T) {
	terminalScenario(t, e2eDir(t, "keyboard"), &Options{Interaction: InteractionTerminal, TrustWorkspace: true, Bridge: BridgeOff})
}

func TestE2EScreenReader(t *testing.T) {
	// A fresh directory each run, so the trust dialog is exercised too.
	dir := e2eDir(t, fmt.Sprintf("ax-%d", time.Now().Unix()))
	terminalScenario(t, dir, &Options{Interaction: InteractionTerminal, TrustWorkspace: true, ScreenReader: true, Bridge: BridgeOff})
}

func TestE2EScreenReaderBridge(t *testing.T) {
	terminalScenario(t, e2eDir(t, "ax-bridge"), &Options{Interaction: InteractionTerminal, TrustWorkspace: true, ScreenReader: true})
}
