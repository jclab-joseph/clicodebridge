package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	bridge "github.com/jclab-joseph/clicodebridge"
)

// Codex records each session in
// $CODEX_HOME/sessions/YYYY/MM/DD/rollout-<local time>-<id>.jsonl, one
// {"type": ..., "payload": {...}} record per line. The CLI cannot be told a
// session id in advance, so a new session's file is found by its
// session_meta (cwd) and modification time.

type rolloutLine struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type sessionMeta struct {
	ID         string `json:"id"`
	Cwd        string `json:"cwd"`
	Originator string `json:"originator"`
}

// readMeta returns the session_meta of a rollout file.
func readMeta(path string) (sessionMeta, bool) {
	f, err := os.Open(path)
	if err != nil {
		return sessionMeta{}, false
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64*1024)
	line, err := r.ReadBytes('\n')
	if err != nil && err != io.EOF {
		return sessionMeta{}, false
	}
	var rl rolloutLine
	var m sessionMeta
	if json.Unmarshal(line, &rl) != nil || rl.Type != "session_meta" || json.Unmarshal(rl.Payload, &m) != nil {
		return sessionMeta{}, false
	}
	return m, m.ID != ""
}

func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// findNewRollout looks for a rollout for cwd modified since start that no
// other session has claimed.
func findNewRollout(home, cwd string, start time.Time, claimed func(string) bool) (string, sessionMeta, bool) {
	for _, day := range []time.Time{start, start.Add(-24 * time.Hour), time.Now()} {
		dir := filepath.Join(home, "sessions", day.Format("2006"), day.Format("01"), day.Format("02"))
		matches, _ := filepath.Glob(filepath.Join(dir, "rollout-*.jsonl"))
		for _, p := range matches {
			fi, err := os.Stat(p)
			if err != nil || fi.ModTime().Before(start.Add(-2*time.Second)) || claimed(p) {
				continue
			}
			if m, ok := readMeta(p); ok && samePath(m.Cwd, cwd) {
				return p, m, true
			}
		}
	}
	return "", sessionMeta{}, false
}

// findRolloutByID locates the rollout of an existing session.
func findRolloutByID(home, id string) string {
	matches, _ := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "rollout-*-"+id+".jsonl"))
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
}

// tailer reads complete lines appended to a file since the last poll.
type tailer struct {
	path    string
	offset  int64
	partial []byte
}

func (t *tailer) poll() ([][]byte, error) {
	f, err := os.Open(t.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	t.offset += int64(len(data))
	data = append(t.partial, data...)
	var lines [][]byte
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		if l := bytes.TrimSpace(data[:i]); len(l) > 0 {
			lines = append(lines, append([]byte(nil), l...))
		}
		data = data[i+1:]
	}
	t.partial = append([]byte(nil), data...)
	return lines, nil
}

// --- record parsing ---------------------------------------------------------------

type responseItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Summary   []textPart      `json:"summary"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     json.RawMessage `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
}

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type eventMsg struct {
	Type             string `json:"type"`
	LastAgentMessage string `json:"last_agent_message"`
}

type turnContext struct {
	Model          string `json:"model"`
	Effort         string `json:"effort"`
	ApprovalPolicy string `json:"approval_policy"`
	SandboxPolicy  struct {
		Type string `json:"type"`
	} `json:"sandbox_policy"`
	CollaborationMode struct {
		Mode string `json:"mode"`
	} `json:"collaboration_mode"`
}

// askCall is a request_user_input call.
type askCall struct {
	CallID    string
	Questions []bridge.Question
}

type parsed struct {
	events      []bridge.Event
	turnStarted bool
	turnEnded   bool
	interrupted bool
	finalText   string
	settings    *turnContext
	ask         *askCall
}

func parseLine(line []byte) parsed {
	var out parsed
	var rl rolloutLine
	if json.Unmarshal(line, &rl) != nil {
		return out
	}
	raw := json.RawMessage(line)
	switch rl.Type {
	case "event_msg":
		var e eventMsg
		_ = json.Unmarshal(rl.Payload, &e)
		switch e.Type {
		case "task_started":
			out.turnStarted = true
		case "task_complete":
			out.turnEnded, out.finalText = true, e.LastAgentMessage
		case "turn_aborted":
			out.turnEnded, out.interrupted = true, true
		}
	case "turn_context":
		var tc turnContext
		if json.Unmarshal(rl.Payload, &tc) == nil {
			out.settings = &tc
		}
	case "response_item":
		var it responseItem
		if json.Unmarshal(rl.Payload, &it) != nil {
			return out
		}
		switch it.Type {
		case "message":
			text := partsText(it.Content)
			switch {
			case strings.TrimSpace(text) == "":
			case it.Role == "assistant":
				out.events = append(out.events, bridge.Event{Type: bridge.EventAssistantText, Text: text, Raw: raw})
			case it.Role == "user" && !isInjectedContext(text):
				out.events = append(out.events, bridge.Event{Type: bridge.EventUserMessage, Text: text, Raw: raw})
			}
		case "reasoning":
			var parts []string
			for _, s := range it.Summary {
				if s.Text != "" {
					parts = append(parts, s.Text)
				}
			}
			if len(parts) > 0 {
				out.events = append(out.events, bridge.Event{Type: bridge.EventThinking, Text: strings.Join(parts, "\n"), Raw: raw})
			}
		case "function_call", "custom_tool_call", "local_shell_call":
			input := it.Input
			if it.Arguments != "" {
				input = json.RawMessage(it.Arguments)
				if !json.Valid(input) {
					input, _ = json.Marshal(it.Arguments)
				}
			}
			if len(input) > 0 && !json.Valid(input) {
				input = nil
			}
			out.events = append(out.events, bridge.Event{Type: bridge.EventToolCall, Raw: raw,
				Tool: &bridge.ToolEvent{ID: it.CallID, Name: it.Name, Input: input}})
			if it.Name == "request_user_input" {
				out.ask = parseAsk(it.CallID, it.Arguments)
			}
		case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
			out.events = append(out.events, bridge.Event{Type: bridge.EventToolResult, Raw: raw,
				Tool: &bridge.ToolEvent{ID: it.CallID, Output: outputText(it.Output)}})
		}
	}
	return out
}

// isInjectedContext recognizes the context blocks Codex adds as user
// messages (environment, AGENTS.md, skills).
func isInjectedContext(text string) bool {
	t := strings.TrimSpace(text)
	for _, p := range []string{"<environment_context>", "<user_instructions>", "<skills_instructions>", "# AGENTS.md instructions", "<turn_aborted>"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

func partsText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []textPart
	_ = json.Unmarshal(raw, &parts)
	var out []string
	for _, p := range parts {
		if p.Text != "" {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n")
}

func outputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Content string `json:"content"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Content != "" {
		return obj.Content
	}
	if t := partsText(raw); t != "" {
		return t
	}
	return string(raw)
}

func parseAsk(callID, args string) *askCall {
	var in struct {
		Questions []struct {
			ID       string `json:"id"`
			Header   string `json:"header"`
			Question string `json:"question"`
			Options  []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	if json.Unmarshal([]byte(args), &in) != nil || len(in.Questions) == 0 {
		return nil
	}
	a := &askCall{CallID: callID}
	for _, q := range in.Questions {
		bq := bridge.Question{ID: q.ID, Header: q.Header, Text: q.Question, AllowText: true}
		for _, o := range q.Options {
			bq.Options = append(bq.Options, bridge.QuestionOption{Label: o.Label, Description: o.Description})
		}
		a.Questions = append(a.Questions, bq)
	}
	return a
}
