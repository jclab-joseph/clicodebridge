// Package clicodebridge drives interactive coding-agent CLIs (Claude Code,
// Codex, ...) from Go.
//
// Instead of a vendor SDK, each provider runs the real interactive CLI on a
// pseudo-terminal, reads the CLI's own transcript files for canonical
// content, and recognizes dialogs on the virtual screen. Everything the
// interactive CLI can do (computer use, Chrome, remote control, plugins)
// therefore stays available.
//
// A Provider starts Sessions. A Session streams Events, accepts prompts, and
// surfaces permission prompts and questions as Requests that the caller
// answers with Respond.
package clicodebridge

import (
	"context"
	"errors"

	"github.com/jclab-joseph/clicodebridge/terminal"
)

// Provider starts sessions of one CLI.
type Provider interface {
	// Name is a stable identifier such as "claude" or "codex".
	Name() string
	// Info describes the models, effort levels and modes the CLI accepts.
	Info(ctx context.Context) (Info, error)
	// Start launches the CLI. The session is ready for Send once its state
	// leaves StateStarting; Send waits for that.
	Start(ctx context.Context, opts SessionOptions) (Session, error)
}

// Session is one running CLI conversation.
type Session interface {
	// ID is the CLI's own session id. Some CLIs assign it lazily, in which
	// case it is empty until known (an EventSessionID is emitted).
	ID() string
	Provider() string

	// Events streams everything that happens. It is never closed before
	// EventExited is delivered, and is closed right after it. Events are
	// buffered without bound, so a slow reader never stalls the CLI.
	Events() <-chan Event

	State() State
	Settings() Settings
	// Pending returns requests waiting for Respond.
	Pending() []Request

	// Send submits a prompt. It waits until the CLI is ready and returns
	// ErrBusy if a turn is already running or a request is pending.
	Send(ctx context.Context, prompt string) error
	// Respond answers a pending Request.
	Respond(ctx context.Context, requestID string, resp Response) error
	// Interrupt stops the running turn.
	Interrupt(ctx context.Context) error
	// Command runs a CLI slash command such as "/compact".
	Command(ctx context.Context, command string) error

	SetModel(ctx context.Context, model string) error
	SetEffort(ctx context.Context, effort string) error
	SetMode(ctx context.Context, mode string) error

	// Terminal is the PTY the CLI currently runs on. It changes when a
	// settings change restarts the CLI (EventRestarted).
	Terminal() *terminal.Terminal

	// Close stops the CLI. The conversation stays on disk and can be resumed
	// with SessionOptions.Resume.
	Close() error
	// Done is closed when the session has ended.
	Done() <-chan struct{}
}

// SessionOptions are the provider-neutral launch options.
type SessionOptions struct {
	// Cwd is the working directory. Defaults to the process working directory.
	Cwd string
	// Resume continues an existing conversation by its session id.
	Resume string

	// Model, Effort and Mode use ids from Provider.Info. Empty means the
	// CLI's configured default.
	Model  string
	Effort string
	Mode   string

	// Env is appended to the inherited environment.
	Env []string
	// Args are extra raw CLI arguments.
	Args []string

	// Cols and Rows set the PTY size. Defaults 120x40.
	Cols, Rows int

	// Provider holds provider-specific options, e.g. *claude.Options or
	// *codex.Options. Nil means defaults.
	Provider any
}

// Info describes what a provider accepts.
type Info struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName"`
	Version     string   `json:"version,omitempty"`
	Models      []Choice `json:"models"`
	Efforts     []Choice `json:"efforts"`
	Modes       []Choice `json:"modes"`
}

// Choice is one selectable value.
type Choice struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// Settings are the session's current model, effort and mode as far as they
// are known. Empty means the CLI default.
type Settings struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	Mode   string `json:"mode,omitempty"`
	// RemoteControlURL is set once the CLI reports a remote control link.
	RemoteControlURL string `json:"remoteControlUrl,omitempty"`
}

// State is the coarse session state.
type State string

const (
	StateStarting State = "starting" // launching, not yet accepting prompts
	StateIdle     State = "idle"     // ready for Send
	StateBusy     State = "busy"     // a turn is running
	StateWaiting  State = "waiting"  // a Request is pending
	StateExited   State = "exited"
)

var (
	ErrBusy           = errors.New("clicodebridge: session is busy")
	ErrExited         = errors.New("clicodebridge: session has exited")
	ErrUnknownRequest = errors.New("clicodebridge: no pending request with that id")
	ErrUnsupported    = errors.New("clicodebridge: not supported by this provider")
	// ErrScreenTimeout means the CLI's screen never reached the expected
	// shape, usually because a CLI update changed its TUI.
	ErrScreenTimeout = errors.New("clicodebridge: timed out waiting for the expected screen")
)
