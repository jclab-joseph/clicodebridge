package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bridge "github.com/jclab-joseph/clicodebridge"
	"github.com/jclab-joseph/clicodebridge/terminal"
)

// The fixtures under testdata/fixtures/<cli version>/<profile>/ are screens
// recorded from the real CLI by cmd/cbcapture. Every version directory is
// checked, so a new capture after a CLI update shows what drifted.

var fixtureQuestions = map[string][]bridge.Question{
	"colors": {
		{Text: "Which color?", Header: "Color", AllowText: true, Options: opts("Red", "Green", "Blue")},
		{Text: "Which pets?", Header: "Pets", AllowText: true, MultiSelect: true, Options: opts("Cat", "Dog", "Fish")},
	},
	"drink": {{Text: "Which drink?", Header: "Drink", AllowText: true, Options: opts("Tea", "Coffee")}},
}

func opts(labels ...string) []bridge.QuestionOption {
	var out []bridge.QuestionOption
	for _, l := range labels {
		out = append(out, bridge.QuestionOption{Label: l})
	}
	return out
}

type fixtureCase struct {
	hint  screenHint
	check func(t *testing.T, prof string, s screenInfo)
}

func rowKinds(d *questionDialog) []questionRowKind {
	var k []questionRowKind
	for _, r := range d.Rows {
		k = append(k, r.Kind)
	}
	return k
}

func equalKinds(a, b []questionRowKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var fixtureCases = map[string]fixtureCase{
	"trust": {screenHint{expectTrust: true}, func(t *testing.T, prof string, s screenInfo) {
		if s.Trust == nil || !strings.Contains(s.Trust.Rows[s.Trust.Yes], "trust") || s.InputBox {
			t.Fatalf("trust: %+v", s)
		}
	}},
	"idle": {screenHint{registryKnown: true}, func(t *testing.T, prof string, s screenInfo) {
		if !s.idle() || s.Mode != ModeManual || s.Effort != "medium" {
			t.Fatalf("idle: %+v", s)
		}
	}},
	"busy": {screenHint{registryKnown: true}, func(t *testing.T, prof string, s screenInfo) {
		if !s.Busy || s.idle() {
			t.Fatalf("busy: %+v", s)
		}
	}},
	"permission": {screenHint{registryKnown: true, waitingFor: waitingPermission}, func(t *testing.T, prof string, s screenInfo) {
		d := s.Permission
		if d == nil || s.idle() || len(d.Options) != 4 || d.Title != "Bash command" {
			t.Fatalf("permission: %+v / %+v", s, d)
		}
		want := []bridge.PermissionKind{bridge.AllowOnce, bridge.AllowAlways, bridge.OtherOption, bridge.Deny}
		for i, k := range permissionKinds(d.Options) {
			if k != want[i] {
				t.Errorf("option %d %q kind %s", i+1, d.Options[i].Label, k)
			}
		}
		if prof == "classic" && !d.Options[0].Focused {
			t.Error("first option not focused")
		}
	}},
	"question_tabs": {screenHint{registryKnown: true, waitingFor: waitingInput, questions: fixtureQuestions["colors"]}, func(t *testing.T, prof string, s screenInfo) {
		d := s.Question
		if d == nil || d.Question != "Which color?" || d.Multi || s.Review != nil {
			t.Fatalf("question: %+v", d)
		}
		if want := []questionRowKind{rowOption, rowOption, rowOption, rowText, rowChat}; !equalKinds(rowKinds(d), want) {
			t.Fatalf("rows: %+v", d.Rows)
		}
		if prof == "classic" && d.focused() != 0 {
			t.Fatalf("focused %d", d.focused())
		}
	}},
	"question_multi": {screenHint{registryKnown: true, waitingFor: waitingInput, questions: fixtureQuestions["colors"]}, func(t *testing.T, prof string, s screenInfo) {
		d := s.Question
		if d == nil || d.Question != "Which pets?" || !d.Multi {
			t.Fatalf("question: %+v", d)
		}
		want := []questionRowKind{rowOption, rowOption, rowOption, rowText, rowSubmit, rowChat}
		if prof != "classic" {
			want = []questionRowKind{rowOption, rowOption, rowOption, rowText}
		}
		if !equalKinds(rowKinds(d), want) {
			t.Fatalf("rows: %+v", d.Rows)
		}
	}},
	"question_multi_submit": {screenHint{registryKnown: true, waitingFor: waitingInput, questions: fixtureQuestions["colors"]}, func(t *testing.T, prof string, s screenInfo) {
		d := s.Question
		if d == nil || !d.Multi || d.focused() != d.find(rowSubmit, 0) {
			t.Fatalf("question: %+v", d)
		}
		if !d.Rows[0].Checked || d.Rows[1].Checked || !d.Rows[2].Checked {
			t.Fatalf("checked: %+v", d.Rows)
		}
	}},
	"review": {screenHint{registryKnown: true, waitingFor: waitingInput, questions: fixtureQuestions["colors"]}, func(t *testing.T, prof string, s screenInfo) {
		want := "1"
		if prof != "classic" {
			want = "y"
		}
		if s.Review == nil || s.Review.Submit != want || s.Question != nil {
			t.Fatalf("review: %+v", s)
		}
	}},
	"question_single": {screenHint{registryKnown: true, waitingFor: waitingInput, questions: fixtureQuestions["drink"]}, func(t *testing.T, prof string, s screenInfo) {
		d := s.Question
		if d == nil || d.Question != "Which drink?" || d.Multi {
			t.Fatalf("question: %+v", d)
		}
		if want := []questionRowKind{rowOption, rowOption, rowText, rowChat}; !equalKinds(rowKinds(d), want) {
			t.Fatalf("rows: %+v", d.Rows)
		}
	}},
	"question_text_typed": {screenHint{registryKnown: true, waitingFor: waitingInput, questions: fixtureQuestions["drink"]}, func(t *testing.T, prof string, s screenInfo) {
		d := s.Question
		if d == nil {
			t.Fatalf("question: %+v", s)
		}
		i := d.find(rowText, 0)
		if i < 0 || d.Rows[i].Label != "Water" || d.focused() != i {
			t.Fatalf("text row: %+v", d.Rows)
		}
	}},
	"question_text_prompt": {screenHint{registryKnown: true, waitingFor: waitingInput, questions: fixtureQuestions["drink"]}, func(t *testing.T, prof string, s screenInfo) {
		if s.TextEntry != 3 || s.Question == nil {
			t.Fatalf("text entry: %+v", s)
		}
	}},
}

// noHint keeps only what a session without registry or hooks would know.
func noHint(h screenHint) screenHint { return screenHint{expectTrust: h.expectTrust} }

func profileByDir(name string) profile {
	if name == "ax" {
		return screenReaderProfile{}
	}
	return classicProfile{}
}

func loadFixture(t *testing.T, path string) *terminal.Screen {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cols, rows := 120, 40
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(path)), "meta.json")); err == nil {
		var m struct{ Cols, Rows int }
		if json.Unmarshal(b, &m) == nil && m.Cols > 0 {
			cols, rows = m.Cols, m.Rows
		}
	}
	return terminal.Emulate(cols, rows, data)
}

func TestFixtures(t *testing.T) {
	files, _ := filepath.Glob("testdata/fixtures/*/*/*.ansi")
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	for _, f := range files {
		scenario := strings.TrimSuffix(filepath.Base(f), ".ansi")
		c, ok := fixtureCases[scenario]
		if !ok {
			continue // a drift capture without a case yet
		}
		profDir := filepath.Base(filepath.Dir(f))
		prof := profileByDir(profDir)
		scr := loadFixture(t, f)
		name := strings.TrimPrefix(f, "testdata/fixtures/")
		t.Run(name, func(t *testing.T) {
			c.check(t, prof.name(), prof.analyze(screenInput{scr: scr, hint: c.hint}))
		})
		t.Run(name+"/no-hint", func(t *testing.T) {
			c.check(t, prof.name(), prof.analyze(screenInput{scr: scr, hint: noHint(c.hint)}))
		})
	}
}

// TestFocusByStyle removes the pointer glyph: the focused row must still be
// found by its color.
func TestFocusByStyle(t *testing.T) {
	cases := map[string]func(s screenInfo) bool{
		"question_tabs": func(s screenInfo) bool { return s.Question != nil && s.Question.focused() == 0 },
		"question_multi_submit": func(s screenInfo) bool {
			return s.Question != nil && s.Question.focused() == s.Question.find(rowSubmit, 0)
		},
		"permission": func(s screenInfo) bool { return s.Permission != nil && s.Permission.Options[0].Focused },
	}
	for scenario, ok := range cases {
		files, _ := filepath.Glob("testdata/fixtures/*/classic/" + scenario + ".ansi")
		for _, f := range files {
			data, _ := os.ReadFile(f)
			data = bytes.ReplaceAll(data, []byte("❯"), []byte(" "))
			info := classicProfile{}.analyze(screenInput{scr: terminal.Emulate(120, 40, data), hint: fixtureCases[scenario].hint})
			if !ok(info) {
				t.Errorf("%s: focus by style failed: %+v %+v", f, info.Question, info.Permission)
			}
		}
	}
}

func TestExtraScreens(t *testing.T) {
	read := func(name string) screenInfo {
		b, err := os.ReadFile("testdata/extra/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return classicProfile{}.analyze(screenInput{scr: terminal.PlainScreen(strings.Split(strings.TrimRight(string(b), "\n"), "\n"))})
	}
	if s := read("model_swap.txt"); !s.Other || s.Permission != nil || s.Question != nil {
		t.Fatalf("model swap: %+v", s)
	}
	s := read("remote.txt")
	if s.RemoteURL != "https://claude.ai/code/session_01MZfdQGev8SE5GHGHkGCg4Z" || s.Mode != ModeAcceptEdits || !s.idle() {
		t.Fatalf("remote: %+v", s)
	}
}

// TestScreenReaderInterruptedPrompt is the screen a drift diagnostic caught:
// after an interrupt the restored prompt follows "$" and a no-break space.
func TestScreenReaderInterruptedPrompt(t *testing.T) {
	b, err := os.ReadFile("testdata/extra/ax_interrupted_prompt.txt")
	if err != nil {
		t.Fatal(err)
	}
	s := screenReaderProfile{}.analyze(screenInput{scr: terminal.PlainScreen(strings.Split(strings.TrimRight(string(b), "\n"), "\n")),
		hint: screenHint{registryKnown: true}})
	if !s.idle() {
		t.Fatalf("interrupted prompt not recognized: %+v", s)
	}
}

func TestScreenOrdinal(t *testing.T) {
	d := &permissionDialog{Options: []screenOption{
		{Ordinal: 1, Label: "Yes"}, {Ordinal: 2, Label: "Yes, and always allow access to /w from this project"},
		{Ordinal: 3, Label: "Yes, and switch to auto mode"}, {Ordinal: 4, Label: "No"},
	}}
	for _, c := range []struct {
		opt  bridge.PermissionOption
		want int
	}{
		{bridge.PermissionOption{Kind: bridge.AllowOnce}, 1},
		{bridge.PermissionOption{Kind: bridge.AllowAlways}, 2},
		{bridge.PermissionOption{Kind: bridge.OtherOption, Label: "Allow and switch to auto mode"}, 3},
		{bridge.PermissionOption{Kind: bridge.Deny}, 4},
	} {
		if got := screenOrdinal(d, c.opt); got != c.want {
			t.Errorf("%+v -> %d, want %d", c.opt, got, c.want)
		}
	}
}

func TestDriftHelpers(t *testing.T) {
	if compareVersions("2.1.310", "2.1.296") != 1 || compareVersions("2.1.296", "2.1.296") != 0 || compareVersions("2.0.9", "2.1.0") != -1 {
		t.Fatal("compareVersions")
	}
	reg := registry{ok: true, Status: "waiting", WaitingFor: waitingPermission}
	if !driftRules[0].holds(reg, screenInfo{}, true) || driftRules[0].holds(reg, screenInfo{Permission: &permissionDialog{}}, true) {
		t.Fatal("dialog_unrecognized rule")
	}
	if !driftRules[1].holds(registry{ok: true, Status: "idle"}, screenInfo{}, true) {
		t.Fatal("prompt_unrecognized rule")
	}
	if remoteControlURL("cse_01AB") != "https://claude.ai/code/session_01AB" || remoteControlURL("x") != "" {
		t.Fatal("remoteControlURL")
	}
}
