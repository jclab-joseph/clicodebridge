package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bridge "github.com/jclab-joseph/clicodebridge"
	"github.com/jclab-joseph/clicodebridge/internal/core"
	"github.com/jclab-joseph/clicodebridge/terminal"
)

// TestDriftReportWritesFixture checks that a drift report emits a
// diagnostic and saves the screen in the fixture layout.
func TestDriftReportWritesFixture(t *testing.T) {
	tm, err := terminal.Start(terminal.Options{Path: "sh", Args: []string{"-c", "printf 'an unknown dialog\\n'; sleep 5"}, Cols: 40, Rows: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer tm.Close()
	for !strings.Contains(tm.Screen().Text(), "unknown dialog") {
		time.Sleep(20 * time.Millisecond)
	}
	dir := t.TempDir()
	s := &session{Base: core.NewBase("claude"), prof: classicProfile{}, co: Options{DriftDir: dir}, term: tm, version: "9.9.9"}
	s.reg = registry{ok: true, Status: "waiting", WaitingFor: waitingPermission}

	// The rule must hold past driftAfter before it fires, then fire once.
	s.checkDrift(s.reg, screenInfo{}, true)
	s.mu.Lock()
	s.drift.since["dialog_unrecognized"] = time.Now().Add(-2 * driftAfter)
	s.mu.Unlock()
	s.checkDrift(s.reg, screenInfo{}, true)
	s.checkDrift(s.reg, screenInfo{}, true)

	var diags []bridge.Event
	s.Finish("done")
	for ev := range s.Events() {
		if ev.Type == bridge.EventDiagnostic {
			diags = append(diags, ev)
		}
	}
	if len(diags) != 1 || diags[0].Kind != "drift" || !strings.Contains(diags[0].Screen, "unknown dialog") {
		t.Fatalf("diagnostics = %+v", diags)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "9.9.9", "classic", "drift-dialog_unrecognized-*"))
	if len(files) != 3 {
		t.Fatalf("drift files = %v", files)
	}
	for _, f := range files {
		if strings.HasSuffix(f, ".ansi") {
			b, _ := os.ReadFile(f)
			if !strings.Contains(terminal.Emulate(40, 5, b).Text(), "unknown dialog") {
				t.Fatalf("fixture does not replay: %q", b)
			}
		}
	}
}
