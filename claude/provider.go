// Package claude is the Claude Code provider.
//
// A session runs the interactive `claude` CLI on a PTY and combines three
// channels:
//   - HTTP hooks (injected with --settings) for turn boundaries, permission
//     prompts and AskUserQuestion, with structured payloads;
//   - the transcript JSONL for message and tool content;
//   - the virtual screen for readiness, busy state, mode/effort indicators,
//     the workspace trust dialog, and answering dialogs by keyboard.
package claude

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bridge "github.com/jclab-joseph/clicodebridge"
	"github.com/jclab-joseph/clicodebridge/internal/hookserver"
)

// Config configures the provider.
type Config struct {
	// Executable is the claude binary. Default "claude" from PATH.
	Executable string
	// ConfigDir is Claude's config directory, used to locate transcripts.
	// Default $CLAUDE_CONFIG_DIR, else ~/.claude.
	ConfigDir string
	// Env is appended to every session's environment.
	Env []string
}

// Provider starts Claude Code sessions.
type Provider struct {
	cfg   Config
	hooks hookserver.Server

	versionOnce sync.Once
	version     string
}

// New returns a Claude Code provider.
func New(cfg Config) *Provider {
	if cfg.Executable == "" {
		cfg.Executable = "claude"
	}
	return &Provider{cfg: cfg}
}

var _ bridge.Provider = (*Provider)(nil)

func (p *Provider) Name() string { return "claude" }

// Close stops the shared hook server. Close sessions first.
func (p *Provider) Close() error { return p.hooks.Close() }

func (p *Provider) configDir() string {
	if p.cfg.ConfigDir != "" {
		return p.cfg.ConfigDir
	}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// Info lists the values SessionOptions and the Set* methods accept.
func (p *Provider) Info(ctx context.Context) (bridge.Info, error) {
	p.versionOnce.Do(func() {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, p.cfg.Executable, "--version").Output()
		if err == nil {
			p.version = strings.TrimSpace(string(out))
		}
	})
	return bridge.Info{
		Name:        "claude",
		DisplayName: "Claude Code",
		Version:     p.version,
		Models: []bridge.Choice{
			{ID: "", Name: "Default", Description: "The model from your Claude Code settings"},
			{ID: "opus", Name: "Opus"},
			{ID: "sonnet", Name: "Sonnet"},
			{ID: "haiku", Name: "Haiku"},
			{ID: "fable", Name: "Fable"},
		},
		Efforts: []bridge.Choice{
			{ID: "low", Name: "Low"}, {ID: "medium", Name: "Medium"}, {ID: "high", Name: "High"},
			{ID: "xhigh", Name: "Extra high"}, {ID: "max", Name: "Max"},
		},
		Modes: []bridge.Choice{
			{ID: ModeManual, Name: "Manual", Description: "Ask before risky actions"},
			{ID: ModeAcceptEdits, Name: "Accept edits", Description: "Auto-approve file edits"},
			{ID: ModePlan, Name: "Plan", Description: "Plan without making changes"},
			{ID: ModeAuto, Name: "Auto", Description: "A classifier approves safe actions"},
			{ID: ModeDontAsk, Name: "Don't ask", Description: "Deny anything that would prompt"},
			{ID: ModeBypass, Name: "Bypass permissions", Description: "Skip all permission checks"},
		},
	}, nil
}

// sessionMarkerEnv are variables a parent Claude Code process sets for its
// children. Inherited, they make the spawned CLI believe it is a nested
// session (it then disables transcript saving, among other things).
var sessionMarkerEnv = []string{
	"CLAUDECODE", "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ID", "CLAUDE_PID",
	"CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_EXECPATH", "CLAUDE_CODE_MESSAGING_SOCKET",
	"CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_AGENT_SDK_VERSION", "CLAUDE_CODE_SESSION_ATTENDED",
	"CLAUDE_CODE_EMIT_STARTUP_TIMING", "CLAUDE_EFFORT", "AI_AGENT",
	"CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING", "CLAUDE_CODE_QUESTION_PREVIEW_FORMAT",
	"CLAUDE_CODE_ENABLE_TASKS", "MCP_CONNECTION_NONBLOCKING",
}

func providerOptions(v any) (Options, error) {
	switch o := v.(type) {
	case nil:
		return Options{}, nil
	case *Options:
		if o == nil {
			return Options{}, nil
		}
		return *o, nil
	case Options:
		return o, nil
	}
	return Options{}, fmt.Errorf("claude: SessionOptions.Provider must be *claude.Options, got %T", v)
}
