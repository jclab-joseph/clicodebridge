package claude

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Permission modes accepted by --permission-mode (Claude Code 2.1.x).
const (
	ModeManual      = "manual" // ask before risky actions ("default" in older releases)
	ModeAcceptEdits = "acceptEdits"
	ModePlan        = "plan"
	ModeAuto        = "auto"
	ModeDontAsk     = "dontAsk"
	ModeBypass      = "bypassPermissions"
)

// shiftTabCycle is the order Shift+Tab walks through in the TUI. Modes not
// in it can only be set at launch.
var shiftTabCycle = []string{ModeManual, ModeAcceptEdits, ModePlan, ModeAuto}

// normalizeMode maps aliases (including the hook payload's "default") to the
// ids used by this package.
func normalizeMode(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "":
		return ""
	case "default", "manual":
		return ModeManual
	case "acceptedits":
		return ModeAcceptEdits
	case "plan":
		return ModePlan
	case "auto":
		return ModeAuto
	case "dontask":
		return ModeDontAsk
	case "bypasspermissions", "bypass":
		return ModeBypass
	}
	return m
}

func normalizeEffort(e string) string {
	switch strings.ToLower(strings.TrimSpace(e)) {
	case "low", "medium", "high", "xhigh", "max":
		return strings.ToLower(strings.TrimSpace(e))
	}
	return ""
}

// Interaction selects how permission prompts and questions are answered.
type Interaction int

const (
	// InteractionHook (default) holds Claude's PermissionRequest hook until
	// Respond is called and answers through the hook response. It is
	// structured and does not depend on the TUI layout, but while a request
	// is held the TUI and remote control do not show it.
	InteractionHook Interaction = iota
	// InteractionTerminal lets Claude show its normal dialog (in the TUI and
	// to remote control clients) and answers by pressing keys. Requests
	// answered elsewhere resolve as ResolvedExternally.
	InteractionTerminal
)

// Options are Claude-specific session options, passed as
// SessionOptions.Provider.
type Options struct {
	// RemoteControl turns Remote Control on (true) or off (false) for this
	// session. Nil keeps the user's remoteControlAtStartup setting.
	RemoteControl *bool
	// RemoteControlName names the Remote Control session.
	RemoteControlName string
	// RemoteControlPrefix sets --remote-control-session-name-prefix.
	RemoteControlPrefix string

	// Chrome enables (true) or disables (false) Claude in Chrome.
	Chrome *bool

	Interaction Interaction

	// ScreenReader runs the CLI with --ax-screen-reader and reads its flat,
	// instruction-labelled output instead of the classic layout. The
	// terminal shows that plainer rendering too.
	ScreenReader bool

	// Bridge controls the in-process bridge mod (prompt submission and
	// interrupts without keystrokes). Default BridgeAuto.
	Bridge Bridge

	// DriftDir, when set, receives a fixture (screen .ansi/.txt plus .json
	// context) whenever a recognizer disagrees with the CLI's own state.
	DriftDir string
	// HookHoldTimeout bounds how long InteractionHook holds a request.
	// After it, the request falls back to the TUI dialog (same request id).
	// Default 9m; Claude caps hook timeouts at 10m.
	HookHoldTimeout time.Duration

	// TrustWorkspace answers the workspace trust dialog automatically.
	// Otherwise it is surfaced as a permission Request.
	TrustWorkspace bool

	// LiveSettingChanges applies SetModel/SetEffort with /model and /effort
	// inside the running session. Claude saves those as the user's global
	// defaults, so by default the session is instead relaunched with
	// --resume and the session-scoped --model/--effort flags.
	LiveSettingChanges bool

	// Settings are merged into the --settings JSON. Hook entries are merged
	// with the ones this package installs.
	Settings map[string]any

	AddDirs            []string
	MCPConfigs         []string // files or JSON strings for --mcp-config
	StrictMCPConfig    bool
	AllowedTools       []string
	DisallowedTools    []string
	SystemPrompt       string
	AppendSystemPrompt string
	Name               string // --name
	Agent              string // --agent
	ForkSession        bool   // with SessionOptions.Resume
	// AllowDangerouslySkipPermissions adds bypassPermissions to the modes
	// reachable at runtime.
	AllowDangerouslySkipPermissions bool
}

// Bool returns a pointer to b, for the tri-state options.
func Bool(b bool) *bool { return &b }

// launch is everything needed to (re)start the CLI process.
type launch struct {
	sessionID string // --session-id for a new conversation
	resume    string // --resume
	fork      bool
	model     string
	effort    string
	mode      string
	extraArgs []string
	pluginDir string // the bridge mod, when loaded
}

// args builds the CLI arguments.
func (o *Options) args(l launch, settingsJSON string) ([]string, error) {
	var a []string
	if l.resume != "" {
		a = append(a, "--resume", l.resume)
		if l.fork {
			a = append(a, "--fork-session")
		}
	} else {
		a = append(a, "--session-id", l.sessionID)
	}
	a = append(a, "--settings", settingsJSON)
	if l.model != "" {
		a = append(a, "--model", l.model)
	}
	if l.effort != "" {
		a = append(a, "--effort", l.effort)
	}
	if l.mode != "" {
		a = append(a, "--permission-mode", l.mode)
	}
	if o.RemoteControl != nil && *o.RemoteControl {
		a = append(a, "--remote-control")
		if o.RemoteControlName != "" {
			a = append(a, o.RemoteControlName)
		}
	}
	if o.RemoteControlPrefix != "" {
		a = append(a, "--remote-control-session-name-prefix", o.RemoteControlPrefix)
	}
	if o.Chrome != nil {
		if *o.Chrome {
			a = append(a, "--chrome")
		} else {
			a = append(a, "--no-chrome")
		}
	}
	for _, d := range o.AddDirs {
		a = append(a, "--add-dir", d)
	}
	for _, c := range o.MCPConfigs {
		a = append(a, "--mcp-config", c)
	}
	if o.StrictMCPConfig {
		a = append(a, "--strict-mcp-config")
	}
	if len(o.AllowedTools) > 0 {
		a = append(a, "--allowed-tools", strings.Join(o.AllowedTools, ","))
	}
	if len(o.DisallowedTools) > 0 {
		a = append(a, "--disallowed-tools", strings.Join(o.DisallowedTools, ","))
	}
	if o.SystemPrompt != "" {
		a = append(a, "--system-prompt", o.SystemPrompt)
	}
	if o.AppendSystemPrompt != "" {
		a = append(a, "--append-system-prompt", o.AppendSystemPrompt)
	}
	if o.Name != "" {
		a = append(a, "--name", o.Name)
	}
	if o.Agent != "" {
		a = append(a, "--agent", o.Agent)
	}
	if o.AllowDangerouslySkipPermissions {
		a = append(a, "--allow-dangerously-skip-permissions")
	}
	if o.ScreenReader {
		a = append(a, screenReaderProfile{}.args()...)
	}
	if l.pluginDir != "" {
		a = append(a, "--plugin-dir", l.pluginDir)
	}
	for _, x := range l.extraArgs {
		if x == "-p" || x == "--print" || strings.HasPrefix(x, "--print=") {
			return nil, fmt.Errorf("claude: %s is not supported; sessions drive the interactive CLI", x)
		}
	}
	return append(a, l.extraArgs...), nil
}

// hookEvents are the hooks installed into every session. Only events that
// accept http hooks are listed; an unknown key would invalidate the whole
// --settings value.
var hookEvents = []string{"UserPromptSubmit", "PermissionRequest", "PostToolUse", "Stop", "Notification"}

// settingsJSON renders --settings: the caller's settings plus our hooks.
func (o *Options) settingsJSON(hookPrefix string, holdTimeout time.Duration) (string, error) {
	s := map[string]any{}
	for k, v := range o.Settings {
		s[k] = v
	}
	hooks := map[string]any{}
	if user, ok := s["hooks"].(map[string]any); ok {
		for k, v := range user {
			hooks[k] = v
		}
	}
	for _, ev := range hookEvents {
		timeout := 30
		if ev == "PermissionRequest" {
			// Slightly above the hold timeout so we, not Claude, end the wait.
			timeout = int(holdTimeout/time.Second) + 30
		}
		entry := map[string]any{
			"hooks": []any{map[string]any{"type": "http", "url": hookPrefix + "/" + ev, "timeout": timeout}},
		}
		if ev == "PermissionRequest" || ev == "PostToolUse" {
			entry["matcher"] = "*"
		}
		existing, _ := hooks[ev].([]any)
		hooks[ev] = append(existing, entry)
	}
	s["hooks"] = hooks
	if o.RemoteControl != nil && !*o.RemoteControl {
		s["remoteControlAtStartup"] = false
	}
	b, err := json.Marshal(s)
	return string(b), err
}
