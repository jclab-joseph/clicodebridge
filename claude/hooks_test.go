package claude

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	bridge "github.com/jclab-joseph/clicodebridge"
)

func decision(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var out struct {
		HookSpecificOutput struct {
			HookEventName string         `json:"hookEventName"`
			Decision      map[string]any `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.HookSpecificOutput.HookEventName != "PermissionRequest" {
		t.Fatalf("bad hook output %s: %v", b, err)
	}
	return out.HookSpecificOutput.Decision
}

func TestPermissionHookResponse(t *testing.T) {
	in := &hookInput{
		ToolName:  "Bash",
		ToolInput: json.RawMessage(`{"command":"ls"}`),
		PermissionSuggestion: []json.RawMessage{
			json.RawMessage(`{"type":"addDirectories","directories":["/w"],"destination":"session"}`),
		},
	}
	req := &bridge.Request{Kind: bridge.RequestPermission, Permission: &bridge.Permission{Options: permissionOptions(in.PermissionSuggestion)}}
	if got := req.Permission.Options[1].Label; got != "Always allow access to /w for this session" {
		t.Errorf("suggestion label = %q", got)
	}

	out, _ := hookResponse(req, in, bridge.Allow())
	if d := decision(t, out); d["behavior"] != "allow" || d["updatedPermissions"] != nil {
		t.Errorf("allow = %v", d)
	}
	out, _ = hookResponse(req, in, bridge.AllowForever())
	if d := decision(t, out); d["behavior"] != "allow" || len(d["updatedPermissions"].([]any)) != 1 {
		t.Errorf("allow always = %v", d)
	}
	out, _ = hookResponse(req, in, bridge.Reject("use git status"))
	if d := decision(t, out); d["behavior"] != "deny" || d["message"] != "use git status" {
		t.Errorf("deny = %v", d)
	}
}

func TestQuestionHookResponse(t *testing.T) {
	in := &hookInput{ToolName: askToolName, ToolInput: json.RawMessage(`{"questions":[
		{"question":"Which color?","header":"Color","multiSelect":false,"options":[{"label":"Red","description":"r"},{"label":"Green","description":"g"}]},
		{"question":"Which pets?","header":"Pets","multiSelect":true,"options":[{"label":"Cat","description":""},{"label":"Dog","description":""}]}]}`)}
	qs, err := questionsFromAsk(in.ToolInput)
	if err != nil || len(qs) != 2 || !qs[1].MultiSelect || !qs[0].AllowText {
		t.Fatalf("questions = %+v %v", qs, err)
	}
	req := &bridge.Request{Kind: bridge.RequestQuestion, Questions: qs}
	answers := []bridge.Answer{{Selected: []string{"Green"}}, {Selected: []string{"Cat"}, Text: "Hamster"}}
	if err := bridge.ValidateAnswers(qs, answers); err != nil {
		t.Fatal(err)
	}
	out, _ := hookResponse(req, in, bridge.Response{Answers: answers})
	d := decision(t, out)
	input := d["updatedInput"].(map[string]any)
	got := input["answers"].(map[string]any)
	if got["Which color?"] != "Green" || got["Which pets?"] != "Cat, Hamster" || input["questions"] == nil {
		t.Fatalf("updatedInput = %v", input)
	}
	out, _ = hookResponse(req, in, bridge.Response{Cancel: true})
	if d := decision(t, out); d["behavior"] != "deny" {
		t.Fatalf("cancel = %v", d)
	}
	if err := bridge.ValidateAnswers(qs, []bridge.Answer{{Selected: []string{"Red", "Green"}}, {Text: "x"}}); err == nil {
		t.Fatal("two answers to a single-select question accepted")
	}
}

func TestArgsAndSettings(t *testing.T) {
	o := &Options{RemoteControl: Bool(true), RemoteControlName: "dev", Chrome: Bool(false),
		Settings: map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []any{}}}}}}
	settings, err := o.settingsJSON("http://127.0.0.1:1/h/tok", 9*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks map[string][]map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(settings), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Hooks["Stop"]) != 2 || len(s.Hooks["PermissionRequest"]) != 1 {
		t.Fatalf("hooks not merged: %v", s.Hooks)
	}
	ph := s.Hooks["PermissionRequest"][0]["hooks"].([]any)[0].(map[string]any)
	if ph["url"] != "http://127.0.0.1:1/h/tok/PermissionRequest" || ph["timeout"].(float64) != 570 {
		t.Fatalf("permission hook = %v", ph)
	}

	args, err := o.args(launch{sessionID: "id", model: "haiku", effort: "low", mode: ModeManual}, settings)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--session-id id", "--model haiku", "--effort low", "--permission-mode manual", "--remote-control dev", "--no-chrome"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args lack %q: %s", want, joined)
		}
	}
	if _, err := o.args(launch{extraArgs: []string{"-p"}}, settings); err == nil {
		t.Error("-p accepted")
	}
	off := &Options{RemoteControl: Bool(false)}
	settings, _ = off.settingsJSON("u", time.Minute)
	if !strings.Contains(settings, `"remoteControlAtStartup":false`) {
		t.Errorf("remote control off not applied: %s", settings)
	}
}
