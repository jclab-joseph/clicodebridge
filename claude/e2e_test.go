//go:build e2e

// Real end-to-end tests against the installed claude CLI. They spend tokens
// and leave transcripts under ~/.claude/projects, so they only run with
//
//	CLICODEBRIDGE_E2E_DIR=<scratch dir> go test -tags e2e ./claude -run E2E -v
//
// The scratch directory is trusted (via the trust dialog) by the test.
package claude

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bridge "github.com/jclab-joseph/clicodebridge"
)

type eventLog struct {
	t  *testing.T
	ch <-chan bridge.Event
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
			l.t.Logf("event %s %s %s", ev.Type, ev.State, trimForLog(ev.Text))
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

func send(t *testing.T, s bridge.Session, prompt string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Send(ctx, prompt); err != nil {
		t.Fatalf("send: %v", err)
	}
}

func TestE2EHookInteraction(t *testing.T) {
	dir := e2eDir(t, "hook")
	home, _ := os.UserHomeDir()
	settingsBefore, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))

	s, log := startE2E(t, dir, &Options{})
	waitIdle(t, s, log)

	send(t, s, "Reply with exactly the word PONG and nothing else.")
	log.wait(30*time.Second, "assistant text", is(bridge.EventAssistantText))
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
	if err := s.Respond(context.Background(), req.ID, bridge.Allow()); err != nil {
		t.Fatal(err)
	}
	log.wait(10*time.Second, "resolved", is(bridge.EventRequestResolved))
	log.wait(60*time.Second, "turn end", is(bridge.EventTurnCompleted))
	if b, err := os.ReadFile(filepath.Join(dir, "f.txt")); err != nil || strings.TrimSpace(string(b)) != "e2e" {
		t.Fatalf("f.txt = %q %v", b, err)
	}

	send(t, s, `Use the AskUserQuestion tool once with one question: header "Size", question "Which size?", options Small and Large, single select. Then reply with only my answer.`)
	req = log.wait(60*time.Second, "question", is(bridge.EventRequest)).Request
	if req.Kind != bridge.RequestQuestion || len(req.Questions) != 1 {
		t.Fatalf("request = %+v", req)
	}
	if err := s.Respond(context.Background(), req.ID, bridge.Choose("Large")); err != nil {
		t.Fatal(err)
	}
	done = log.wait(60*time.Second, "turn end", is(bridge.EventTurnCompleted))
	if !strings.Contains(done.Text, "Large") {
		t.Fatalf("final text = %q", done.Text)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	log.wait(10*time.Second, "restart", is(bridge.EventRestarted))
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

func TestE2ETerminalInteraction(t *testing.T) {
	dir := e2eDir(t, "terminal")
	s, log := startE2E(t, dir, &Options{Interaction: InteractionTerminal, TrustWorkspace: true})
	waitIdle(t, s, log)

	send(t, s, `Use the AskUserQuestion tool once with two questions: (1) header "Color", question "Which color?", single select, options Red, Green, Blue. (2) header "Pets", question "Which pets?", multiSelect true, options Cat, Dog, Fish. Then reply with only my answers.`)
	req := log.wait(60*time.Second, "question", is(bridge.EventRequest)).Request
	if req.Kind != bridge.RequestQuestion || len(req.Questions) != 2 || !req.Questions[1].MultiSelect {
		t.Fatalf("request = %+v", req)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := s.Respond(ctx, req.ID, bridge.Response{Answers: []bridge.Answer{
		{Selected: []string{"Green"}},
		{Selected: []string{"Cat", "Fish"}, Text: "Hamster"},
	}})
	if err != nil {
		t.Fatalf("respond: %v\nscreen:\n%s", err, strings.Join(s.Terminal().Lines(), "\n"))
	}
	done := log.wait(60*time.Second, "turn end", is(bridge.EventTurnCompleted))
	for _, want := range []string{"Green", "Cat", "Fish", "Hamster"} {
		if !strings.Contains(done.Text, want) {
			t.Fatalf("final text %q lacks %q", done.Text, want)
		}
	}

	os.Remove(filepath.Join(dir, "g.txt"))
	send(t, s, "Run this exact shell command with the Bash tool: echo term > g.txt")
	req = log.wait(60*time.Second, "permission", is(bridge.EventRequest)).Request
	if err := s.Respond(ctx, req.ID, bridge.Allow()); err != nil {
		t.Fatalf("respond: %v\nscreen:\n%s", err, strings.Join(s.Terminal().Lines(), "\n"))
	}
	log.wait(60*time.Second, "turn end", is(bridge.EventTurnCompleted))
	if b, err := os.ReadFile(filepath.Join(dir, "g.txt")); err != nil || strings.TrimSpace(string(b)) != "term" {
		t.Fatalf("g.txt = %q %v", b, err)
	}
}
