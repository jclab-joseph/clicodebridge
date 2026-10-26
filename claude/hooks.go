package claude

import (
	"encoding/json"
	"fmt"
	"strings"

	bridge "github.com/jclab-joseph/clicodebridge"
)

// hookInput is the subset of Claude hook payloads this package reads.
type hookInput struct {
	SessionID            string            `json:"session_id"`
	TranscriptPath       string            `json:"transcript_path"`
	HookEventName        string            `json:"hook_event_name"`
	PermissionMode       string            `json:"permission_mode"`
	Prompt               string            `json:"prompt"`
	ToolName             string            `json:"tool_name"`
	ToolInput            json.RawMessage   `json:"tool_input"`
	ToolUseID            string            `json:"tool_use_id"`
	PermissionSuggestion []json.RawMessage `json:"permission_suggestions"`
	Message              string            `json:"message"`
	NotificationType     string            `json:"notification_type"`
	LastAssistantMessage string            `json:"last_assistant_message"`
	Effort               *struct {
		Level string `json:"level"`
	} `json:"effort"`
}

// askInput is the AskUserQuestion tool input.
type askInput struct {
	Questions []struct {
		Question    string `json:"question"`
		Header      string `json:"header"`
		MultiSelect bool   `json:"multiSelect"`
		Options     []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
	} `json:"questions"`
}

const askToolName = "AskUserQuestion"

func questionsFromAsk(raw json.RawMessage) ([]bridge.Question, error) {
	var in askInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("claude: parse AskUserQuestion input: %w", err)
	}
	var qs []bridge.Question
	for _, q := range in.Questions {
		bq := bridge.Question{Header: q.Header, Text: q.Question, MultiSelect: q.MultiSelect, AllowText: true}
		for _, o := range q.Options {
			bq.Options = append(bq.Options, bridge.QuestionOption{Label: o.Label, Description: o.Description})
		}
		qs = append(qs, bq)
	}
	return qs, nil
}

// permissionOptions derives answer options from a PermissionRequest hook:
// allow once, one option per permission suggestion, deny.
func permissionOptions(suggestions []json.RawMessage) []bridge.PermissionOption {
	opts := []bridge.PermissionOption{{ID: "allow", Label: "Allow", Kind: bridge.AllowOnce}}
	for i, s := range suggestions {
		label, kind := describeSuggestion(s)
		opts = append(opts, bridge.PermissionOption{ID: fmt.Sprintf("suggestion:%d", i), Label: label, Kind: kind})
	}
	return append(opts, bridge.PermissionOption{ID: "deny", Label: "Deny", Kind: bridge.Deny})
}

func describeSuggestion(raw json.RawMessage) (string, bridge.PermissionKind) {
	var s struct {
		Type        string   `json:"type"`
		Mode        string   `json:"mode"`
		Directories []string `json:"directories"`
		Rules       []struct {
			ToolName    string `json:"toolName"`
			RuleContent string `json:"ruleContent"`
		} `json:"rules"`
		Destination string `json:"destination"`
	}
	_ = json.Unmarshal(raw, &s)
	where := ""
	switch s.Destination {
	case "session":
		where = " for this session"
	case "projectSettings", "localSettings":
		where = " in this project"
	case "userSettings":
		where = " everywhere"
	}
	switch s.Type {
	case "setMode":
		return "Allow and switch to " + s.Mode + " mode", bridge.OtherOption
	case "addDirectories":
		return "Always allow access to " + strings.Join(s.Directories, ", ") + where, bridge.AllowAlways
	case "addRules", "replaceRules":
		var rules []string
		for _, r := range s.Rules {
			if r.RuleContent != "" {
				rules = append(rules, r.ToolName+"("+r.RuleContent+")")
			} else {
				rules = append(rules, r.ToolName)
			}
		}
		return "Always allow " + strings.Join(rules, ", ") + where, bridge.AllowAlways
	}
	return "Allow (" + s.Type + ")" + where, bridge.AllowAlways
}

// permissionDetail summarizes a tool input for display.
func permissionDetail(tool string, input json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(input, &m) == nil {
		for _, k := range []string{"command", "file_path", "url", "plan", "pattern", "path", "query"} {
			if v, ok := m[k].(string); ok && v != "" {
				return v
			}
		}
	}
	s := string(input)
	if len(s) > 500 {
		s = s[:500] + "…"
	}
	return s
}

// permissionDecision renders the PermissionRequest hook response.
func permissionDecision(behavior string, extra map[string]any) []byte {
	d := map[string]any{"behavior": behavior}
	for k, v := range extra {
		d[k] = v
	}
	b, _ := json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{"hookEventName": "PermissionRequest", "decision": d},
	})
	return b
}

// hookResponse turns a client Response into the hook reply.
func hookResponse(req *bridge.Request, in *hookInput, r bridge.Response) ([]byte, error) {
	if req.Kind == bridge.RequestQuestion {
		if r.Cancel {
			return permissionDecision("deny", map[string]any{"message": "The user dismissed the question."}), nil
		}
		input := map[string]any{}
		if err := json.Unmarshal(in.ToolInput, &input); err != nil {
			return nil, err
		}
		input["answers"] = answerMap(req.Questions, r.Answers)
		return permissionDecision("allow", map[string]any{"updatedInput": input}), nil
	}
	opt, err := req.Permission.ResolveOption(r)
	if err != nil {
		return nil, err
	}
	switch {
	case opt.Kind == bridge.Deny:
		extra := map[string]any{}
		if r.Message != "" {
			extra["message"] = r.Message
		}
		return permissionDecision("deny", extra), nil
	case strings.HasPrefix(opt.ID, "suggestion:"):
		var i int
		fmt.Sscanf(opt.ID, "suggestion:%d", &i)
		return permissionDecision("allow", map[string]any{"updatedPermissions": []json.RawMessage{in.PermissionSuggestion[i]}}), nil
	}
	return permissionDecision("allow", nil), nil
}

// answerMap renders answers the way the TUI reports them: question text to
// comma-joined labels, with free text last.
func answerMap(qs []bridge.Question, answers []bridge.Answer) map[string]string {
	m := map[string]string{}
	for i, q := range qs {
		parts := append([]string(nil), answers[i].Selected...)
		if answers[i].Text != "" {
			parts = append(parts, answers[i].Text)
		}
		m[q.Text] = strings.Join(parts, ", ")
	}
	return m
}
