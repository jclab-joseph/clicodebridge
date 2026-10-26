// Package codex is the OpenAI Codex CLI provider.
//
// A session runs the interactive `codex` TUI on a PTY, tails its rollout
// JSONL for content and turn boundaries, and recognizes approval and
// request_user_input dialogs on the virtual screen. Codex hooks are not used.
package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bridge "github.com/jclab-joseph/clicodebridge"
)

// Modes. The first three are sandbox/approval presets applied at launch;
// ModePlan toggles Codex's Plan collaboration mode with Shift+Tab.
const (
	ModeReadOnly   = "read-only"   // -s read-only -a on-request
	ModeAuto       = "auto"        // -s workspace-write -a on-request
	ModeFullAccess = "full-access" // -s danger-full-access -a never
	ModePlan       = "plan"
)

func presetArgs(mode string) []string {
	switch mode {
	case ModeReadOnly:
		return []string{"-s", "read-only", "-a", "on-request"}
	case ModeAuto:
		return []string{"-s", "workspace-write", "-a", "on-request"}
	case ModeFullAccess:
		return []string{"-s", "danger-full-access", "-a", "never"}
	}
	return nil
}

// presetFromPolicy names the preset a turn_context corresponds to.
func presetFromPolicy(sandbox, approval string) string {
	switch {
	case sandbox == "read-only":
		return ModeReadOnly
	case sandbox == "workspace-write" && approval == "on-request":
		return ModeAuto
	case sandbox == "danger-full-access":
		return ModeFullAccess
	}
	return ""
}

// Config configures the provider.
type Config struct {
	// Executable is the codex binary. Default "codex" from PATH.
	Executable string
	// Home is $CODEX_HOME. Default $CODEX_HOME, else ~/.codex.
	Home string
	// Env is appended to every session's environment.
	Env []string
}

// Options are Codex-specific session options (SessionOptions.Provider).
type Options struct {
	// TrustWorkspace answers a directory trust prompt automatically.
	TrustWorkspace bool
	Search         bool     // --search
	AddDirs        []string // --add-dir
	Config         []string // -c key=value overrides
	Profile        string   // -p
	Enable         []string // --enable <feature>
	Disable        []string // --disable <feature>
}

// Provider starts Codex sessions.
type Provider struct {
	cfg Config

	mu      sync.Mutex
	claimed map[string]bool // rollout paths owned by a session

	versionOnce sync.Once
	version     string
}

// New returns a Codex provider.
func New(cfg Config) *Provider {
	if cfg.Executable == "" {
		cfg.Executable = "codex"
	}
	return &Provider{cfg: cfg, claimed: map[string]bool{}}
}

var _ bridge.Provider = (*Provider)(nil)

func (p *Provider) Name() string { return "codex" }

func (p *Provider) home() string {
	if p.cfg.Home != "" {
		return p.cfg.Home
	}
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".codex")
}

func (p *Provider) claim(path string, on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if on {
		p.claimed[path] = true
	} else {
		delete(p.claimed, path)
	}
}

func (p *Provider) isClaimed(path string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.claimed[path]
}

// Info reads the model list from Codex's models cache.
func (p *Provider) Info(ctx context.Context) (bridge.Info, error) {
	p.versionOnce.Do(func() {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, p.cfg.Executable, "--version").Output(); err == nil {
			p.version = strings.TrimSpace(string(out))
		}
	})
	info := bridge.Info{
		Name: "codex", DisplayName: "Codex", Version: p.version,
		Models: []bridge.Choice{{ID: "", Name: "Default", Description: "The model from config.toml"}},
		Modes: []bridge.Choice{
			{ID: ModeReadOnly, Name: "Read only", Description: "Read-only sandbox, ask for approval"},
			{ID: ModeAuto, Name: "Auto", Description: "Workspace-write sandbox, ask when needed"},
			{ID: ModeFullAccess, Name: "Full access", Description: "No sandbox, never ask"},
			{ID: ModePlan, Name: "Plan", Description: "Plan collaboration mode"},
		},
	}
	efforts := map[string]bool{}
	var cache struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Description string `json:"description"`
			Levels      []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if b, err := os.ReadFile(filepath.Join(p.home(), "models_cache.json")); err == nil && json.Unmarshal(b, &cache) == nil {
		for _, m := range cache.Models {
			name := m.DisplayName
			if name == "" {
				name = m.Slug
			}
			info.Models = append(info.Models, bridge.Choice{ID: m.Slug, Name: name, Description: m.Description})
			for _, l := range m.Levels {
				if !efforts[l.Effort] {
					efforts[l.Effort] = true
					info.Efforts = append(info.Efforts, bridge.Choice{ID: l.Effort, Name: l.Effort})
				}
			}
		}
	}
	if len(info.Efforts) == 0 {
		for _, e := range []string{"low", "medium", "high", "xhigh"} {
			info.Efforts = append(info.Efforts, bridge.Choice{ID: e, Name: e})
		}
	}
	return info, nil
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
	return Options{}, fmt.Errorf("codex: SessionOptions.Provider must be *codex.Options, got %T", v)
}
