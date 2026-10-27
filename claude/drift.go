package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	bridge "github.com/jclab-joseph/clicodebridge"
)

// Drift detection: the registry, the hooks and the mod each tell what the
// TUI should be showing; when the screen recognizer disagrees for longer
// than a redraw takes, the CLI has probably changed its layout. The session
// then emits EventDiagnostic (Kind "drift") with the screen and, when
// Options.DriftDir is set, saves the screen in the fixture layout
// cbcapture writes, ready to become a test case.

// VerifiedVersions is the Claude Code range the recognizers were checked
// against, per profile. Newer versions run, and report once that they are
// unverified.
var VerifiedVersions = map[string][2]string{
	"classic":       {"2.1.296", "2.1.311"},
	"screen-reader": {"2.1.310", "2.1.311"},
}

const (
	driftAfter = 2500 * time.Millisecond
)

type driftRule struct {
	name string
	// holds reports whether the disagreement is present now.
	holds func(reg registry, info screenInfo, ready bool) bool
}

var driftRules = []driftRule{
	{"dialog_unrecognized", func(reg registry, info screenInfo, ready bool) bool {
		// The engine shows a dialog the profile cannot read.
		return reg.ok && reg.Status == "waiting" &&
			(reg.WaitingFor == waitingPermission || reg.WaitingFor == waitingInput) && !info.dialogVisible()
	}},
	{"prompt_unrecognized", func(reg registry, info screenInfo, ready bool) bool {
		// The engine is idle but no prompt is recognized.
		return ready && reg.ok && reg.Status == "idle" && !info.InputBox && !info.dialogVisible()
	}},
}

type driftState struct {
	since    map[string]time.Time
	reported map[string]bool
	version  bool // unverified-version notice sent
}

// checkDrift evaluates the rules for one observed screen.
func (s *session) checkDrift(reg registry, info screenInfo, ready bool) {
	now := time.Now()
	s.mu.Lock()
	if s.closing || s.restarting {
		s.mu.Unlock()
		return // exit and relaunch screens are expected to disagree
	}
	ds := &s.drift
	if ds.since == nil {
		ds.since, ds.reported = map[string]time.Time{}, map[string]bool{}
	}
	var fire []string
	for _, r := range driftRules {
		if !r.holds(reg, info, ready) {
			delete(ds.since, r.name)
			ds.reported[r.name] = false
			continue
		}
		if t, ok := ds.since[r.name]; !ok {
			ds.since[r.name] = now
		} else if now.Sub(t) > driftAfter && !ds.reported[r.name] {
			ds.reported[r.name] = true
			fire = append(fire, r.name)
		}
	}
	s.mu.Unlock()
	for _, name := range fire {
		s.reportDrift(name, fmt.Sprintf("registry says %s/%s for %s but the %s recognizer disagrees",
			reg.Status, reg.WaitingFor, driftAfter, s.prof.name()))
	}
}

// reportActionDrift is called when driving the screen failed because it
// never took the expected shape.
func (s *session) reportActionDrift(op string, err error) {
	if err != nil && errors.Is(err, bridge.ErrScreenTimeout) {
		s.reportDrift("action_failed_"+op, err.Error())
	}
}

func (s *session) reportDrift(rule, msg string) {
	t := s.currentTerm()
	scr := t.Screen()
	s.Emit(bridge.Event{Type: bridge.EventDiagnostic, Kind: "drift", Text: rule + ": " + msg, Screen: scr.Text()})
	if s.co.DriftDir == "" {
		return
	}
	version := s.cliVersion()
	if version == "" {
		version = "unknown"
	}
	dir := filepath.Join(s.co.DriftDir, version, s.prof.name())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	base := filepath.Join(dir, fmt.Sprintf("drift-%s-%d", rule, time.Now().Unix()))
	snap, _, cancel := t.Subscribe(1)
	cancel()
	_ = os.WriteFile(base+".ansi", snap, 0o644)
	_ = os.WriteFile(base+".txt", []byte(scr.Text()+"\n"), 0o644)
	s.mu.Lock()
	reg := s.reg
	s.mu.Unlock()
	meta, _ := json.MarshalIndent(map[string]any{
		"rule": rule, "message": msg, "profile": s.prof.name(), "version": version,
		"registry": map[string]string{"status": reg.Status, "waitingFor": reg.WaitingFor},
		"pending":  s.Pending(),
	}, "", "  ")
	_ = os.WriteFile(base+".json", meta, 0o644)
}

var semverRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// compareVersions compares dotted versions; unparsable ones compare equal.
func compareVersions(a, b string) int {
	ma, mb := semverRe.FindStringSubmatch(a), semverRe.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return 0
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// noteVersion emits a notice once if the CLI is outside the verified range.
func (s *session) noteVersion(version string) {
	r, ok := VerifiedVersions[s.prof.name()]
	if !ok || version == "" {
		return
	}
	s.mu.Lock()
	done := s.drift.version
	s.drift.version = true
	s.mu.Unlock()
	if done {
		return
	}
	if compareVersions(version, r[0]) < 0 || compareVersions(version, r[1]) > 0 {
		s.Emit(bridge.Event{Type: bridge.EventNotice, Kind: "unverified_version",
			Text: fmt.Sprintf("Claude Code %s is outside the range the %s recognizers were verified on (%s–%s); drift diagnostics will report mismatches",
				strings.TrimSpace(version), s.prof.name(), r[0], r[1])})
	}
}
