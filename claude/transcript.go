package claude

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	bridge "github.com/jclab-joseph/clicodebridge"
)

// Claude writes each conversation to
// <config dir>/projects/<sanitized cwd>/<session id>.jsonl, one JSON record
// per line. The transcript is the canonical source of message content; the
// screen is only used for control.

var unsafePathChars = regexp.MustCompile(`[^a-zA-Z0-9]`)

func transcriptPath(configDir, cwd, sessionID string) string {
	return filepath.Join(configDir, "projects", unsafePathChars.ReplaceAllString(cwd, "-"), sessionID+".jsonl")
}

// tailer reads complete lines appended to a file since the last poll.
type tailer struct {
	path    string
	offset  int64
	partial []byte
}

// newTailer starts at the current end of path (or at 0 if it does not
// exist yet), so only new records are reported.
func newTailer(path string) *tailer {
	t := &tailer{path: path}
	if fi, err := os.Stat(path); err == nil {
		t.offset = fi.Size()
	}
	return t
}

func (t *tailer) poll() ([][]byte, error) {
	f, err := os.Open(t.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() < t.offset {
		t.offset, t.partial = 0, nil // truncated or replaced
	}
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
		if line := bytes.TrimSpace(data[:i]); len(line) > 0 {
			lines = append(lines, append([]byte(nil), line...))
		}
		data = data[i+1:]
	}
	t.partial = append([]byte(nil), data...)
	return lines, nil
}

// record is the subset of transcript records this package reads.
type record struct {
	Type            string          `json:"type"`
	UUID            string          `json:"uuid"`
	ToolUseResult   json.RawMessage `json:"toolUseResult"`
	Subtype         string          `json:"subtype"`
	IsSidechain     bool            `json:"isSidechain"`
	IsMeta          bool            `json:"isMeta"`
	BridgeSessionID string          `json:"bridgeSessionId"`
	PermissionMode  string          `json:"permissionMode"`
	Message         json.RawMessage `json:"message"`
}

type message struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Source    *blockSource    `json:"source"`
}

// blockSource is an image or document block's payload.
type blockSource struct {
	Type      string `json:"type"` // "base64" (others carry no inline data)
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// sentFiles is the toolUseResult of SendUserFile / SendUserMessage.
type sentFiles struct {
	Caption     string `json:"caption"`
	Message     string `json:"message"`
	Attachments []struct {
		Path      string `json:"path"`
		Size      int64  `json:"size"`
		MediaType string `json:"media_type"`
	} `json:"attachments"`
}

// parsed is what one transcript record contributes.
type parsed struct {
	events    []bridge.Event
	turnEnd   bool
	remoteURL string
	mode      string
	model     string
}

func parseRecord(line []byte) parsed {
	var out parsed
	var r record
	if json.Unmarshal(line, &r) != nil || r.IsSidechain {
		return out
	}
	switch r.Type {
	case "system":
		out.turnEnd = r.Subtype == "turn_duration"
		return out
	case "bridge-session":
		if id, ok := strings.CutPrefix(r.BridgeSessionID, "cse_"); ok {
			out.remoteURL = "https://claude.ai/code/session_" + id
		}
		return out
	case "permission-mode":
		out.mode = normalizeMode(r.PermissionMode)
		return out
	case "user", "assistant":
	default:
		return out
	}
	if r.IsMeta {
		return out
	}
	var m message
	if json.Unmarshal(r.Message, &m) != nil {
		return out
	}
	if m.Model == "<synthetic>" {
		return out // a placeholder Claude writes itself (e.g. on resume), not model output
	}
	out.model = m.Model
	raw := json.RawMessage(line)
	var text string
	if json.Unmarshal(m.Content, &text) == nil {
		if ev, ok := textEvent(r.Type, text, raw); ok {
			out.events = append(out.events, ev)
		}
		return out
	}
	var blocks []contentBlock
	if json.Unmarshal(m.Content, &blocks) != nil {
		return out
	}
	// Images the user attached ride on the prompt's event.
	var userFiles []bridge.Attachment
	for i, b := range blocks {
		if a, ok := inlineAttachment(b, fmt.Sprintf("%s:%d", r.UUID, i)); ok && r.Type == "user" {
			userFiles = append(userFiles, a)
		}
	}
	firstUserText := -1
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if ev, ok := textEvent(r.Type, b.Text, raw); ok {
				if ev.Type == bridge.EventUserMessage && firstUserText < 0 {
					firstUserText = len(out.events)
				}
				out.events = append(out.events, ev)
			}
		case "thinking":
			if strings.TrimSpace(b.Thinking) != "" {
				out.events = append(out.events, bridge.Event{Type: bridge.EventThinking, Text: b.Thinking, Raw: raw})
			}
		case "tool_use":
			out.events = append(out.events, bridge.Event{Type: bridge.EventToolCall, Raw: raw,
				Tool: &bridge.ToolEvent{ID: b.ID, Name: b.Name, Input: b.Input}})
		case "tool_result":
			out.events = append(out.events, bridge.Event{Type: bridge.EventToolResult, Raw: raw,
				Tool:        &bridge.ToolEvent{ID: b.ToolUseID, Output: contentText(b.Content), IsError: b.IsError},
				Attachments: resultAttachments(b)})
			if ev, ok := sentFilesEvent(b.ToolUseID, r.ToolUseResult, raw); ok {
				out.events = append(out.events, ev)
			}
		}
	}
	if len(userFiles) > 0 {
		if firstUserText >= 0 {
			out.events[firstUserText].Attachments = userFiles
		} else {
			out.events = append(out.events, bridge.Event{Type: bridge.EventUserMessage, Attachments: userFiles, Raw: raw})
		}
	}
	return out
}

// inlineAttachment decodes an image or document block.
func inlineAttachment(b contentBlock, id string) (bridge.Attachment, bool) {
	if (b.Type != "image" && b.Type != "document") || b.Source == nil || b.Source.Type != "base64" {
		return bridge.Attachment{}, false
	}
	data, err := base64.StdEncoding.DecodeString(b.Source.Data)
	if err != nil {
		return bridge.Attachment{}, false
	}
	name := b.Type
	if exts, _ := mime.ExtensionsByType(b.Source.MediaType); len(exts) > 0 {
		name += exts[len(exts)-1]
	}
	return bridge.Attachment{ID: id, Name: name, MediaType: b.Source.MediaType, Size: int64(len(data)), Data: data}, true
}

// resultAttachments returns the images and documents a tool result holds.
func resultAttachments(b contentBlock) []bridge.Attachment {
	var blocks []contentBlock
	if json.Unmarshal(b.Content, &blocks) != nil {
		return nil
	}
	var out []bridge.Attachment
	for i, c := range blocks {
		if a, ok := inlineAttachment(c, fmt.Sprintf("%s:%d", b.ToolUseID, i)); ok {
			out = append(out, a)
		}
	}
	return out
}

// sentFilesEvent turns a SendUserFile / SendUserMessage result into
// EventFiles. Those tools exist when Remote Control is on.
func sentFilesEvent(toolUseID string, raw json.RawMessage, rec json.RawMessage) (bridge.Event, bool) {
	var sf sentFiles
	if len(raw) == 0 || json.Unmarshal(raw, &sf) != nil || len(sf.Attachments) == 0 {
		return bridge.Event{}, false
	}
	ev := bridge.Event{Type: bridge.EventFiles, Text: sf.Caption, Raw: rec}
	if ev.Text == "" {
		ev.Text = sf.Message
	}
	for i, f := range sf.Attachments {
		if f.Path == "" {
			continue // a reference to a file only the user's device holds
		}
		a := bridge.FileAttachment(fmt.Sprintf("%s:sent:%d", toolUseID, i), f.Path, sf.Caption)
		if f.MediaType != "" {
			a.MediaType = f.MediaType
		}
		if a.Size == 0 {
			a.Size = f.Size
		}
		ev.Attachments = append(ev.Attachments, a)
	}
	return ev, len(ev.Attachments) > 0
}

var (
	commandNameRe = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	commandArgsRe = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
	localOutputRe = regexp.MustCompile(`(?s)<local-command-(?:stdout|stderr)>(.*?)</local-command-(?:stdout|stderr)>`)
)

func textEvent(role, text string, raw json.RawMessage) (bridge.Event, bool) {
	if strings.TrimSpace(text) == "" {
		return bridge.Event{}, false
	}
	if role == "assistant" {
		return bridge.Event{Type: bridge.EventAssistantText, Text: text, Raw: raw}, true
	}
	// Slash commands are recorded as tagged pseudo-messages.
	if m := commandNameRe.FindStringSubmatch(text); m != nil {
		cmd := strings.TrimSpace(m[1])
		if a := commandArgsRe.FindStringSubmatch(text); a != nil && strings.TrimSpace(a[1]) != "" {
			cmd += " " + strings.TrimSpace(a[1])
		}
		return bridge.Event{Type: bridge.EventUserMessage, Text: cmd, Kind: "command", Raw: raw}, true
	}
	if m := localOutputRe.FindStringSubmatch(text); m != nil {
		if strings.TrimSpace(m[1]) == "" {
			return bridge.Event{}, false
		}
		return bridge.Event{Type: bridge.EventNotice, Text: strings.TrimSpace(m[1]), Kind: "command_output", Raw: raw}, true
	}
	if strings.HasPrefix(text, "<local-command-caveat>") || strings.HasPrefix(text, "<system-reminder>") {
		return bridge.Event{}, false
	}
	if strings.HasPrefix(text, "[Request interrupted by user") {
		return bridge.Event{Type: bridge.EventNotice, Text: text, Kind: "interrupted", Raw: raw}, true
	}
	return bridge.Event{Type: bridge.EventUserMessage, Text: text, Raw: raw}, true
}

// contentText flattens a tool_result content (string or block list).
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		} else if b.Type != "" {
			parts = append(parts, "["+b.Type+"]")
		}
	}
	return strings.Join(parts, "\n")
}
