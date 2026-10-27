package claude

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// Every running Claude Code session keeps a record in
// <config dir>/sessions/<pid>.json, the same data `claude agents --json`
// prints "for scripting". Its status is the engine's own view of the
// session, so it replaces reading readiness, busy state and dialog presence
// off the screen:
//
//	status  idle | busy | waiting
//	waitingFor (when waiting)  "permission prompt", "input needed"
//	    (AskUserQuestion, MCP elicitation), "dialog open" (a local dialog such
//	    as /model), "sandbox request", "worker request", "goal proposal"
//
// The file format itself is undocumented, so a missing or unreadable file
// falls back to `claude agents --json`, and then to the screen.

// registry is one reading of the session record.
type registry struct {
	ok              bool
	Status          string `json:"status"`
	WaitingFor      string `json:"waitingFor"`
	SessionID       string `json:"sessionId"`
	BridgeSessionID string `json:"bridgeSessionId"`
	Version         string `json:"version"`
}

// Values of registry.WaitingFor this package acts on.
const (
	waitingPermission = "permission prompt"
	waitingInput      = "input needed"
	waitingDialog     = "dialog open"
)

func readRegistryFile(configDir string, pid int) (registry, bool) {
	b, err := os.ReadFile(filepath.Join(configDir, "sessions", strconv.Itoa(pid)+".json"))
	if err != nil {
		return registry{}, false
	}
	var r registry
	if json.Unmarshal(b, &r) != nil {
		return registry{}, false
	}
	r.ok = true
	return r, true
}

// readRegistryCommand asks `claude agents --json` for the session.
func readRegistryCommand(ctx context.Context, exe, sessionID string) (registry, bool) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, "agents", "--json").Output()
	if err != nil {
		return registry{}, false
	}
	var all []registry
	if json.Unmarshal(out, &all) != nil {
		return registry{}, false
	}
	for _, r := range all {
		if r.SessionID == sessionID {
			r.ok = true
			return r, true
		}
	}
	return registry{}, false
}

// remoteControlURL derives the claude.ai link from bridgeSessionId.
func remoteControlURL(bridgeSessionID string) string {
	if len(bridgeSessionID) > 4 && bridgeSessionID[:4] == "cse_" {
		return "https://claude.ai/code/session_" + bridgeSessionID[4:]
	}
	return ""
}

// trustAccepted reports whether the global config records the workspace
// trust dialog as accepted for dir; ok is false when it cannot tell. It is
// a hint for recognizing the dialog, never written.
func trustAccepted(configDir, dir string) (accepted, ok bool) {
	path := filepath.Join(filepath.Dir(configDir), ".claude.json")
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		path = filepath.Join(d, ".claude.json")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	var cfg struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return false, false
	}
	for d := dir; ; d = filepath.Dir(d) {
		if p, found := cfg.Projects[d]; found && p.HasTrustDialogAccepted {
			return true, true
		}
		if parent := filepath.Dir(d); parent == d {
			break
		}
	}
	return false, true
}
