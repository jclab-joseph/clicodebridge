package claude

import (
	"io"
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

// A 1x1 PNG.
const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNgYGD4DwABBAEAwS2OUAAAAABJRU5ErkJggg=="

func TestTranscriptAttachments(t *testing.T) {
	// A tool result carrying an image (a Read of a picture, a screenshot).
	p := parseRecord([]byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pngB64 + `"}}]}]}}`))
	if len(p.events) != 1 || len(p.events[0].Attachments) != 1 {
		t.Fatalf("tool result: %+v", p.events)
	}
	a := p.events[0].Attachments[0]
	if a.ID != "t1:0" || a.MediaType != "image/png" || a.Sent || len(a.Data) == 0 || a.Kind() != bridge.KindImage {
		t.Fatalf("attachment: %+v", a)
	}

	// An image the user pasted, next to the prompt text.
	p = parseRecord([]byte(`{"type":"user","uuid":"u2","message":{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + pngB64 + `"}}]}}`))
	if len(p.events) != 1 || p.events[0].Text != "look" || len(p.events[0].Attachments) != 1 || p.events[0].Attachments[0].ID != "u2:1" {
		t.Fatalf("user image: %+v", p.events)
	}

	// SendUserFile (Remote Control sessions): the record's toolUseResult.
	dir := t.TempDir()
	path := filepath.Join(dir, "report.md")
	os.WriteFile(path, []byte("# hi\n"), 0o644)
	line := `{"type":"user","uuid":"u3","toolUseResult":{"caption":"the report","attachments":[{"path":"` + path + `","size":5,"isImage":false,"media_type":"text/markdown","file_uuid":"x"}]},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t9","content":"1 file delivered to user."}]}}`
	p = parseRecord([]byte(line))
	if len(p.events) != 2 || p.events[1].Type != bridge.EventFiles || p.events[1].Text != "the report" {
		t.Fatalf("sent files: %+v", p.events)
	}
	f := p.events[1].Attachments[0]
	if !f.Sent || f.Path != path || f.Name != "report.md" || f.Kind() != bridge.KindText || f.ID != "t9:sent:0" {
		t.Fatalf("sent attachment: %+v", f)
	}
	r, err := f.Open()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	if string(b) != "# hi\n" {
		t.Fatalf("content %q", b)
	}
}
