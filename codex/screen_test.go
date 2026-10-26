package codex

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

func TestIdleAndBusy(t *testing.T) {
	if s := fixture(t, "idle.txt"); !s.idle() || s.Plan {
		t.Fatalf("idle: %+v", s)
	}
	if s := fixture(t, "busy.txt"); !s.Busy || s.idle() {
		t.Fatalf("busy: %+v", s)
	}
	if s := fixture(t, "plan_idle.txt"); !s.idle() || !s.Plan {
		t.Fatalf("plan: %+v", s)
	}
}

func TestApproval(t *testing.T) {
	s := fixture(t, "approval.txt")
	d := s.Approval
	if d == nil || s.idle() || s.Question != nil {
		t.Fatalf("approval: %+v", s)
	}
	if d.Title != "Would you like to run the following command?" || !strings.Contains(d.Detail, "$ echo hello > out.txt") {
		t.Fatalf("title/detail: %q / %q", d.Title, d.Detail)
	}
	want := []bridge.PermissionKind{bridge.AllowOnce, bridge.AllowAlways, bridge.Deny}
	if len(d.Options) != 3 {
		t.Fatalf("options: %+v", d.Options)
	}
	for i, o := range d.Options {
		if k := classifyLabel(cleanLabel(o.Label)); k != want[i] {
			t.Errorf("option %d %q = %s", i, o.Label, k)
		}
	}
	if cleanLabel(d.Options[0].Label) != "Yes, proceed" || !d.Options[0].Focused {
		t.Errorf("option 0: %+v", d.Options[0])
	}
}

func TestQuestion(t *testing.T) {
	s := fixture(t, "question.txt")
	d := s.Question
	if d == nil || s.Busy || s.idle() || s.Approval != nil {
		t.Fatalf("question: %+v", s)
	}
	if d.Index != 1 || d.Total != 1 || d.Question != "Which language should the plan use?" {
		t.Fatalf("header: %+v", d)
	}
	if len(d.Options) != 3 || d.Options[0].Label != "Go (Recommended)" || d.Options[1].Label != "Rust" {
		t.Fatalf("options: %+v", d.Options)
	}
	if d.noneIndex() != 2 || d.focused() != 0 {
		t.Fatalf("none=%d focused=%d", d.noneIndex(), d.focused())
	}
}
