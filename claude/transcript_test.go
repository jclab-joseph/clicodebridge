package claude

import (
	"os"
	"path/filepath"
	"testing"

	bridge "github.com/jclab-joseph/clicodebridge"
)

func TestTranscriptPath(t *testing.T) {
	got := transcriptPath("/h/.claude", "/tmp/claude-1000/-home-x/work1", "abc")
	want := "/h/.claude/projects/-tmp-claude-1000--home-x-work1/abc.jsonl"
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestParseRecords(t *testing.T) {
	cases := []struct {
		line string
		want []bridge.EventType
	}{
		{`{"type":"user","message":{"role":"user","content":"hello"}}`, []bridge.EventType{bridge.EventUserMessage}},
		{`{"type":"assistant","message":{"role":"assistant","model":"m","content":[{"type":"thinking","thinking":"hm"},{"type":"text","text":"hi"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`,
			[]bridge.EventType{bridge.EventThinking, bridge.EventAssistantText, bridge.EventToolCall}},
		{`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"out"}]}]}}`,
			[]bridge.EventType{bridge.EventToolResult}},
		{`{"type":"user","isSidechain":true,"message":{"role":"user","content":"sub"}}`, nil},
		{`{"type":"user","isMeta":true,"message":{"role":"user","content":"meta"}}`, nil},
		{`{"type":"user","message":{"role":"user","content":"<command-name>/effort</command-name>\n<command-message>effort</command-message>\n<command-args>low</command-args>"}}`,
			[]bridge.EventType{bridge.EventUserMessage}},
		{`{"type":"attachment","attachment":{}}`, nil},
		{`{"type":"assistant","message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"No response requested."}]}}`, nil},
	}
	for _, c := range cases {
		p := parseRecord([]byte(c.line))
		if len(p.events) != len(c.want) {
			t.Errorf("%s: got %d events", c.line, len(p.events))
			continue
		}
		for i, ev := range p.events {
			if ev.Type != c.want[i] {
				t.Errorf("%s: event %d = %s", c.line, i, ev.Type)
			}
		}
	}
	p := parseRecord([]byte(`{"type":"user","message":{"role":"user","content":"<command-name>/effort</command-name><command-args>low</command-args>"}}`))
	if p.events[0].Text != "/effort low" {
		t.Errorf("command text = %q", p.events[0].Text)
	}
	if !parseRecord([]byte(`{"type":"system","subtype":"turn_duration"}`)).turnEnd {
		t.Error("turn_duration not recognized")
	}
	if u := parseRecord([]byte(`{"type":"bridge-session","bridgeSessionId":"cse_01ABC"}`)).remoteURL; u != "https://claude.ai/code/session_01ABC" {
		t.Errorf("remote url = %q", u)
	}
	if m := parseRecord([]byte(`{"type":"permission-mode","permissionMode":"default"}`)).mode; m != ModeManual {
		t.Errorf("mode = %q", m)
	}
}

func TestTailerPartialLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	tl := newTailer(path)
	if lines, err := tl.poll(); err != nil || len(lines) != 0 {
		t.Fatalf("missing file: %v %v", lines, err)
	}
	f, _ := os.Create(path)
	defer f.Close()
	f.WriteString(`{"a":1}` + "\n" + `{"b":`)
	lines, _ := tl.poll()
	if len(lines) != 1 {
		t.Fatalf("lines = %q", lines)
	}
	f.WriteString(`2}` + "\n")
	lines, _ = tl.poll()
	if len(lines) != 1 || string(lines[0]) != `{"b":2}` {
		t.Fatalf("lines = %q", lines)
	}
}
