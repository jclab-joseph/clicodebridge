package codex

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	bridge "github.com/jclab-joseph/clicodebridge"
)

// Lines shaped like codex-cli 0.164.0 rollouts (content shortened).
const (
	lineMeta      = `{"timestamp":"2026-10-25T13:44:46Z","type":"session_meta","payload":{"id":"01a1734d-e93f-79b3-b865-7fe3e94f36b9","cwd":"/work","originator":"codex-tui","source":"cli"}}`
	lineStarted   = `{"type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`
	lineCtx       = `{"type":"turn_context","payload":{"turn_id":"t1","model":"gpt-6-sol","effort":"low","approval_policy":"on-request","sandbox_policy":{"type":"read-only"},"collaboration_mode":{"mode":"default"}}}`
	lineEnv       = `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/work</cwd>\n</environment_context>"}]}}`
	lineDev       = `{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"You are Codex"}]}}`
	lineUser      = `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Run echo hello"}]}}`
	lineAssistant = `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}],"phase":"final_answer"}}`
	lineCall      = `{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"c1","name":"exec","input":"const r = 1"}}`
	lineCallOut   = `{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"c1","output":[{"type":"input_text","text":"Script completed"}]}}`
	lineAsk       = `{"type":"response_item","payload":{"type":"function_call","name":"request_user_input","arguments":"{\"questions\":[{\"header\":\"Language\",\"id\":\"language\",\"question\":\"Which language?\",\"options\":[{\"label\":\"Go (Recommended)\",\"description\":\"Use Go.\"},{\"label\":\"Rust\",\"description\":\"Use Rust.\"}]}]}","call_id":"c2"}}`
	lineDone      = `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"t1","last_agent_message":"Done."}}`
)

func TestParseRollout(t *testing.T) {
	if p := parseLine([]byte(lineStarted)); !p.turnStarted {
		t.Error("task_started")
	}
	if p := parseLine([]byte(lineCtx)); p.settings == nil || p.settings.Effort != "low" || p.settings.SandboxPolicy.Type != "read-only" {
		t.Errorf("turn_context: %+v", p.settings)
	}
	for _, l := range []string{lineEnv, lineDev} {
		if p := parseLine([]byte(l)); len(p.events) != 0 {
			t.Errorf("injected message emitted: %s", l)
		}
	}
	check := func(l string, typ bridge.EventType) bridge.Event {
		t.Helper()
		p := parseLine([]byte(l))
		if len(p.events) != 1 || p.events[0].Type != typ {
			t.Fatalf("%s: %+v", l, p.events)
		}
		return p.events[0]
	}
	if ev := check(lineUser, bridge.EventUserMessage); ev.Text != "Run echo hello" {
		t.Error(ev.Text)
	}
	check(lineAssistant, bridge.EventAssistantText)
	if ev := check(lineCall, bridge.EventToolCall); ev.Tool.Name != "exec" || string(ev.Tool.Input) != `"const r = 1"` {
		t.Errorf("call: %+v", ev.Tool)
	}
	if ev := check(lineCallOut, bridge.EventToolResult); ev.Tool.Output != "Script completed" {
		t.Errorf("output: %+v", ev.Tool)
	}
	p := parseLine([]byte(lineAsk))
	if p.ask == nil || len(p.ask.Questions) != 1 || p.ask.Questions[0].Options[0].Label != "Go (Recommended)" {
		t.Errorf("ask: %+v", p.ask)
	}
	if p := parseLine([]byte(lineDone)); !p.turnEnded || p.finalText != "Done." {
		t.Errorf("task_complete: %+v", p)
	}
}

func TestFindRollouts(t *testing.T) {
	home := t.TempDir()
	start := time.Now()
	dir := filepath.Join(home, "sessions", start.Format("2006"), start.Format("01"), start.Format("02"))
	os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "rollout-2026-10-25T22-44-08-01a1734d-e93f-79b3-b865-7fe3e94f36b9.jsonl")
	os.WriteFile(path, []byte(lineMeta+"\n"), 0o644)

	got, meta, ok := findNewRollout(home, "/work", start, func(string) bool { return false })
	if !ok || got != path || meta.ID != "01a1734d-e93f-79b3-b865-7fe3e94f36b9" {
		t.Fatalf("find new: %q %+v %v", got, meta, ok)
	}
	if _, _, ok := findNewRollout(home, "/other", start, func(string) bool { return false }); ok {
		t.Fatal("matched wrong cwd")
	}
	if _, _, ok := findNewRollout(home, "/work", start, func(p string) bool { return p == path }); ok {
		t.Fatal("matched claimed rollout")
	}
	if got := findRolloutByID(home, meta.ID); got != path {
		t.Fatalf("by id: %q", got)
	}
}
