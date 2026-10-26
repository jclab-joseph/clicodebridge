//go:build e2e

// Real end-to-end test against an installed, logged-in codex CLI:
//
//	CLICODEBRIDGE_E2E_DIR=<scratch dir> CODEX_BIN=<path to codex> go test -tags e2e ./codex -run E2E -v
package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bridge "github.com/jclab-joseph/clicodebridge"
)

func TestE2ECodex(t *testing.T) {
	base := os.Getenv("CLICODEBRIDGE_E2E_DIR")
	if base == "" {
		t.Skip("set CLICODEBRIDGE_E2E_DIR to run real e2e tests")
	}
	dir := filepath.Join(base, "codex")
	os.MkdirAll(dir, 0o755)
	os.Remove(filepath.Join(dir, "out.txt"))

	p := New(Config{Executable: os.Getenv("CODEX_BIN")})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	s, err := p.Start(ctx, bridge.SessionOptions{Cwd: dir, Mode: ModeReadOnly, Effort: "low",
		Provider: &Options{TrustWorkspace: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events := s.Events()
	wait := func(timeout time.Duration, what string, pred func(bridge.Event) bool) bridge.Event {
		t.Helper()
		deadline := time.After(timeout)
		for {
			select {
			case ev := <-events:
				t.Logf("event %s %s %.100s", ev.Type, ev.State, ev.Text)
				if pred(ev) {
					return ev
				}
			case <-deadline:
				t.Fatalf("timed out waiting for %s\nscreen:\n%s", what, strings.Join(s.Terminal().Lines(), "\n"))
			}
		}
	}
	is := func(typ bridge.EventType) func(bridge.Event) bool {
		return func(ev bridge.Event) bool { return ev.Type == typ }
	}

	if err := s.Send(ctx, "Run this exact shell command to create a file: echo hello > out.txt"); err != nil {
		t.Fatal(err)
	}
	req := wait(2*time.Minute, "approval", is(bridge.EventRequest)).Request
	if req.Kind != bridge.RequestPermission || !strings.Contains(req.Permission.Detail, "echo hello") {
		t.Fatalf("request = %+v", req)
	}
	if err := s.Respond(ctx, req.ID, bridge.Allow()); err != nil {
		t.Fatal(err)
	}
	wait(2*time.Minute, "turn end", is(bridge.EventTurnCompleted))
	if b, _ := os.ReadFile(filepath.Join(dir, "out.txt")); strings.TrimSpace(string(b)) != "hello" {
		t.Fatalf("out.txt = %q", b)
	}
	if s.ID() == "" {
		t.Fatal("session id not discovered from rollout")
	}

	if err := s.SetMode(ctx, ModePlan); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		prompt string
		answer bridge.Answer
		want   string
	}{
		{"Use request_user_input to ask me one question: which language, options Go or Rust. Then reply with only my answer and stop.",
			bridge.Answer{Selected: []string{"Rust"}}, "Rust"},
		{"Use request_user_input to ask me one question: which editor, options Vim or Emacs. Then reply with only my answer and stop.",
			bridge.Answer{Text: "Helix please"}, "Helix"},
	} {
		if err := s.Send(ctx, tc.prompt); err != nil {
			t.Fatal(err)
		}
		req = wait(2*time.Minute, "question", is(bridge.EventRequest)).Request
		if req.Kind != bridge.RequestQuestion || len(req.Questions) != 1 {
			t.Fatalf("request = %+v", req)
		}
		if err := s.Respond(ctx, req.ID, bridge.Response{Answers: []bridge.Answer{tc.answer}}); err != nil {
			t.Fatal(err)
		}
		done := wait(2*time.Minute, "turn end", is(bridge.EventTurnCompleted))
		if !strings.Contains(done.Text, tc.want) {
			t.Fatalf("final text %q lacks %q", done.Text, tc.want)
		}
	}
	if err := s.SetMode(ctx, ModeReadOnly); err != nil {
		t.Fatal(err)
	}

	id := s.ID()
	if err := s.SetEffort(ctx, "medium"); err != nil {
		t.Fatal(err)
	}
	if s.ID() != id {
		t.Fatalf("resume changed id %s -> %s", id, s.ID())
	}
	if err := s.Send(ctx, "Reply with exactly the word AGAIN."); err != nil {
		t.Fatal(err)
	}
	done := wait(2*time.Minute, "turn end after restart", is(bridge.EventTurnCompleted))
	if !strings.Contains(done.Text, "AGAIN") {
		t.Fatalf("final text = %q", done.Text)
	}
}
