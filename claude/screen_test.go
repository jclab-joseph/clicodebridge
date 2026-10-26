package claude

import (
	"os"
	"strings"
	"testing"

	bridge "github.com/jclab-joseph/clicodebridge"
)

func fixture(t *testing.T, name string) screenInfo {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return analyze(strings.Split(strings.TrimRight(string(b), "\n"), "\n"))
}

func TestIdleScreen(t *testing.T) {
	s := fixture(t, "idle.txt")
	if !s.idle() {
		t.Fatalf("idle screen not recognized: %+v", s)
	}
	if s.Mode != ModeManual || s.Effort != "medium" {
		t.Fatalf("mode/effort = %q/%q", s.Mode, s.Effort)
	}
}

func TestBusyScreen(t *testing.T) {
	s := fixture(t, "busy.txt")
	if !s.Busy || s.idle() || !s.InputBox {
		t.Fatalf("busy screen: %+v", s)
	}
}

func TestTrustDialog(t *testing.T) {
	s := fixture(t, "trust.txt")
	if s.Trust == nil || s.InputBox {
		t.Fatalf("trust dialog: %+v", s)
	}
	if got := s.Trust.Rows[s.Trust.Focused]; got != "No, exit" {
		t.Fatalf("focused = %q", got)
	}
	if s.Trust.index("Yes, I trust this folder") < 0 {
		t.Fatal("trust row missing")
	}
}

func TestPermissionDialog(t *testing.T) {
	s := fixture(t, "permission.txt")
	d := s.Permission
	if d == nil || s.idle() {
		t.Fatalf("permission dialog: %+v", s)
	}
	if d.Title != "Bash command" {
		t.Fatalf("title = %q", d.Title)
	}
	want := []bridge.PermissionKind{bridge.AllowOnce, bridge.AllowAlways, bridge.OtherOption, bridge.Deny}
	if len(d.Options) != len(want) {
		t.Fatalf("options = %+v", d.Options)
	}
	for i, o := range d.Options {
		if o.Ordinal != i+1 || classifyPermissionLabel(o.Label) != want[i] {
			t.Errorf("option %d = %+v (%s)", i, o, classifyPermissionLabel(o.Label))
		}
	}
	if !strings.HasSuffix(d.Options[1].Label, "from this project") {
		t.Errorf("wrapped label not folded: %q", d.Options[1].Label)
	}
	if !d.Options[0].Focused {
		t.Error("first option should be focused")
	}
}

func TestQuestionTabs(t *testing.T) {
	s := fixture(t, "question_tabs.txt")
	d := s.Question
	if d == nil || s.Permission != nil || s.idle() {
		t.Fatalf("question dialog: %+v", s)
	}
	if d.Question != "Which color?" || d.Multi {
		t.Fatalf("question = %+v", d)
	}
	kinds := []questionRowKind{rowOption, rowOption, rowOption, rowText, rowChat}
	if len(d.Rows) != len(kinds) {
		t.Fatalf("rows = %+v", d.Rows)
	}
	for i, k := range kinds {
		if d.Rows[i].Kind != k {
			t.Errorf("row %d kind = %v", i, d.Rows[i].Kind)
		}
	}
	if d.focused() != 0 || d.find(rowText, 0) != 3 {
		t.Fatalf("focused=%d text=%d", d.focused(), d.find(rowText, 0))
	}
}

func TestQuestionMulti(t *testing.T) {
	d := fixture(t, "question_multi_typed.txt").Question
	if d == nil || !d.Multi || d.Question != "Which pets?" {
		t.Fatalf("question = %+v", d)
	}
	if !d.Rows[0].Checked || d.Rows[1].Checked || !d.Rows[2].Checked {
		t.Fatalf("checked = %+v", d.Rows)
	}
	ti := d.find(rowText, 0)
	if ti != 3 || d.Rows[ti].Label != "Hamster" || d.focused() != 3 {
		t.Fatalf("text row = %d %+v focused=%d", ti, d.Rows, d.focused())
	}
	if d.find(rowSubmit, 0) != 4 {
		t.Fatalf("submit row = %d", d.find(rowSubmit, 0))
	}

	d = fixture(t, "question_multi_submit.txt").Question
	if d == nil || d.focused() != d.find(rowSubmit, 0) {
		t.Fatalf("submit focus: %+v", d)
	}
}

func TestQuestionSingle(t *testing.T) {
	d := fixture(t, "question_single.txt").Question
	if d == nil || d.Question != "Which drink?" || d.Multi || d.find(rowText, 0) != 2 || d.find(rowSubmit, 0) != -1 {
		t.Fatalf("question = %+v", d)
	}
}

func TestReview(t *testing.T) {
	s := fixture(t, "review.txt")
	if s.Review == nil || s.Review.submitOrdinal() != 1 || s.Question != nil {
		t.Fatalf("review: %+v", s)
	}
}

func TestModelSwapAndRemote(t *testing.T) {
	if s := fixture(t, "model_swap.txt"); !s.ModelSwap || s.Permission != nil {
		t.Fatalf("model swap: %+v", s)
	}
	s := fixture(t, "remote.txt")
	if s.RemoteURL != "https://claude.ai/code/session_01MZfdQGev8SE5GHGHkGCg4Z" || s.Mode != ModeAcceptEdits || !s.idle() {
		t.Fatalf("remote: %+v", s)
	}
}
