# clicodebridge

Drive interactive coding-agent CLIs (Claude Code, Codex, …) from Go.

Vendor SDKs expose only part of what the CLIs can do. clicodebridge runs the
**real interactive CLI** on a pseudo-terminal instead, so everything the CLI
supports keeps working: computer use, Claude in Chrome, Remote Control,
plugins, skills, slash commands. On top of that it gives you a typed Go API
for prompts, streamed events, permission prompts and multiple-choice
questions.

```go
p := claude.New(claude.Config{})
s, err := p.Start(ctx, clicodebridge.SessionOptions{
    Cwd:   "/path/to/project",
    Model: "sonnet", Effort: "high", Mode: claude.ModeManual,
    Provider: &claude.Options{RemoteControl: claude.Bool(true)},
})

go func() {
    for ev := range s.Events() {
        switch ev.Type {
        case clicodebridge.EventAssistantText:
            fmt.Println(ev.Text)
        case clicodebridge.EventRequest:
            if ev.Request.Kind == clicodebridge.RequestPermission {
                s.Respond(ctx, ev.Request.ID, clicodebridge.Allow())
            } else { // a question: pick options and/or type an answer
                s.Respond(ctx, ev.Request.ID, clicodebridge.Response{
                    Answers: []clicodebridge.Answer{{Selected: []string{"Rust"}}},
                })
            }
        }
    }
}()

s.Send(ctx, "Add a health check endpoint")
```

## How it works

Each provider combines three channels:

| Channel | Used for |
| --- | --- |
| **PTY + virtual screen** (`terminal` package, [charmbracelet/x/vt](https://github.com/charmbracelet/x)) | Readiness, busy state, mode and effort indicators, the workspace trust dialog, and answering dialogs by keyboard. |
| **Transcript files** (`~/.claude/projects/…/<id>.jsonl`, `~/.codex/sessions/…/rollout-*.jsonl`) | Canonical message, thinking and tool content; turn boundaries. |
| **Hooks** (Claude only, `type: "http"` hooks injected with `--settings`) | Structured permission requests and AskUserQuestion payloads, prompt submission, turn end. |
| **Session registry** (Claude only, `~/.claude/sessions/<pid>.json`, as `claude agents --json` prints it) | The engine's own `idle`/`busy`/`waiting` status and what it waits for (`permission prompt`, `input needed`, `dialog open`). |
| **Bridge mod** (Claude only, an embedded function-hooks plugin loaded with `--plugin-dir`) | Submitting prompts and aborting turns without keystrokes; a structured "turn aborted" signal. |

The layout follows [claude-code-cli-acp](https://github.com/moabualruz/claude-code-cli-acp)
(PTY + transcript + screen recognizers), extended with a provider
abstraction, hooks, and question support.

```
clicodebridge/       Provider, Session, Event, Request, Response (provider-neutral)
├── terminal/        PTY process + virtual screen + raw output fan-out
├── claude/          Claude Code provider (bridgemod/: the embedded plugin)
├── codex/           Codex provider
├── cmd/cbcapture/   records Claude screens as recognizer fixtures
├── internal/        event queue, hook server, shared session bookkeeping
└── examples/
    ├── basic/       console client
    └── webremote/   web UI to control sessions from a browser
```

## Core API

`Provider.Start` returns a `Session`:

| Method | |
| --- | --- |
| `Send(ctx, prompt)` | Submit a prompt. Waits for startup; `ErrBusy` while a turn runs or a request is pending. |
| `Respond(ctx, requestID, Response)` | Answer a permission prompt or question. |
| `Interrupt(ctx)` | Stop the running turn. |
| `Command(ctx, "/compact")` | Run a slash command. |
| `SetModel` / `SetEffort` / `SetMode` | Change settings mid-session (see the provider notes). |
| `Events()` | Unbounded event stream; closed after `EventExited`. |
| `State()`, `Settings()`, `Pending()` | Current snapshot. |
| `Terminal()` | The live PTY: `Text()`, `Subscribe()` for raw output, `Write()` for keystrokes. |
| `Close()` | Exit the CLI. The conversation stays on disk; resume with `SessionOptions.Resume`. |

Events: `session_id`, `state`, `user_message`, `assistant_text`, `thinking`,
`tool_call`, `tool_result`, `files`, `turn_started`, `turn_completed`,
`request`, `request_resolved`, `settings`, `notice`, `restarted`,
`diagnostic`, `error`, `exited`.
Prompts typed directly in the TUI or sent via Remote Control show up as
`user_message`/`turn_started` too.

### Images and files

Events carry `Attachments`:

| Event | Attachments | Content |
| --- | --- | --- |
| `tool_result` | images and documents a tool returned (a screenshot, a picture it read) | inline (`Data`) |
| `user_message` | images attached to the prompt | inline |
| `files` | files the agent sent to the user on purpose; `Text` is the caption | on disk (`Path`) |

`Attachment.Open()` reads either kind; `Kind()` says `image`, `video`,
`audio`, `pdf`, `text` or `file`. A sent file is read as it is on disk now.

Claude sends files with `SendUserFile`, which only exists while Remote
Control is on and uploads to claude.ai. The bridge mod therefore registers
its own `send_file` tool (`mcp__clicodebridge__send_file`), which works
without Remote Control and keeps files local. Models find it on their own
when asked to send something. Codex images (`input_image` in prompts and in
tool outputs such as `view_image`) arrive the same way.

### Requests

A `Request` is either a **permission** (`Permission.Options`, each with a
kind: `allow_once`, `allow_always`, `deny`, `other`) or one or more
**questions** (`Questions[i].Options`, `MultiSelect`, `AllowText`).

```go
clicodebridge.Allow()                  // first allow-once option
clicodebridge.AllowForever()           // first allow-always option
clicodebridge.Reject("use git status") // deny with feedback
clicodebridge.Response{OptionID: "suggestion:0"}
clicodebridge.Response{Answers: []clicodebridge.Answer{
    {Selected: []string{"Green"}},                     // single select
    {Selected: []string{"Cat", "Fish"}, Text: "Hamster"}, // multi select + free text
}}
clicodebridge.Response{Cancel: true}   // dismiss (Esc)
```

`request_resolved` reports whether a request was answered through `Respond`
(`client`), in the TUI or Remote Control (`external`), or withdrawn
(`cancelled`).

## Claude Code provider

`claude.Options` (passed as `SessionOptions.Provider`):

| Option | |
| --- | --- |
| `RemoteControl *bool`, `RemoteControlName`, `RemoteControlPrefix` | Turn Remote Control on (`--remote-control [name]`) or off (overrides `remoteControlAtStartup`). nil keeps the user's setting. `Settings().RemoteControlURL` carries the claude.ai link. |
| `Interaction` | `InteractionHook` (default) or `InteractionTerminal`, see below. |
| `TrustWorkspace` | Accept the workspace trust dialog automatically; otherwise it is surfaced as a permission request. |
| `LiveSettingChanges` | Use `/model` and `/effort` in-session (see below). |
| `ScreenReader` | Run with `--ax-screen-reader` and read its flat output (see below). |
| `Bridge` | `BridgeAuto` (default) loads the bridge mod; `BridgeOff` uses the keyboard only. |
| `DriftDir` | Save a fixture whenever a recognizer disagrees with the CLI (see below). |
| `Chrome *bool`, `AddDirs`, `MCPConfigs`, `StrictMCPConfig`, `AllowedTools`, `DisallowedTools`, `SystemPrompt`, `AppendSystemPrompt`, `Name`, `Agent`, `ForkSession`, `AllowDangerouslySkipPermissions` | Map to the CLI flags of the same name. |
| `Settings map[string]any` | Merged into `--settings`; your hooks are kept alongside ours. |

Modes: `manual`, `acceptEdits`, `plan`, `auto`, `dontAsk`, `bypassPermissions`.
Efforts: `low`, `medium`, `high`, `xhigh`, `max`.

**Interaction strategies**

- `InteractionHook`: the `PermissionRequest` hook is held open until you call
  `Respond`, and the answer goes back as the hook response (including
  AskUserQuestion answers via `updatedInput`). Structured, and independent of
  the TUI layout. While a request is held, the TUI and Remote Control do not
  show it. After `HookHoldTimeout` (default 9 min; Claude caps hooks at
  10 min) the request falls back to the TUI dialog under the same id.
- `InteractionTerminal`: Claude shows its normal dialog in the TUI and to
  Remote Control clients, and `Respond` answers it with keystrokes (the
  structured request data still comes from the hook). Whoever answers first
  wins; answers given elsewhere resolve as `external`.

**Settings changes.** Claude saves `/model` and `/effort` as the user's
global defaults (`~/.claude/settings.json`). So by default `SetModel` and
`SetEffort` instead relaunch the CLI with `--resume <id>` and the
session-scoped `--model`/`--effort` flags (`EventRestarted`; re-read
`Terminal()`). Set `LiveSettingChanges` to use the slash commands instead.
`SetMode` uses Shift+Tab for modes in the TUI cycle (manual → acceptEdits →
plan → auto) and relaunches for the others.

### How a session reads Claude's state

The screen is the last resort. Each fact comes from the most structured
source that has it:

| Fact | Source, in order |
| --- | --- |
| Ready, busy, waiting | session registry `status` → window title spinner → screen |
| Which dialog is up | registry `waitingFor` → hook payload → screen structure |
| Question and option texts | `PermissionRequest` hook payload |
| Turn start / end / abort | `UserPromptSubmit` / `Stop` hooks, transcript `turn_duration`, bridge mod `turn.complete` |
| Mode, effort | hook payloads (every event) → screen footer (between turns, e.g. after Shift+Tab) |
| Remote Control URL | registry `bridgeSessionId` → transcript → screen |
| Submitting a prompt, interrupting | bridge mod → paste + Enter / Esc |

Where the screen is still read (answering dialogs in `InteractionTerminal`
mode, the trust dialog, Shift+Tab mode changes), recognizers are
structure-first. They key on numbered rows, checkbox glyphs, the focused
row (the pointer glyph, else the one row drawn in an accent color), and the
question and option texts from the hook, not on English sentences. Option
kinds are positional: the first is "allow once", the last "deny". The
`language` setting does not localize the TUI as of 2.1.310; this keeps it
that way should it start to.

### Bridge mod

`claude/bridgemod` is a function-hooks plugin (`$.prompt.submit`,
`$.turn.abort`, `turn.start`/`turn.complete` hooks). The library extracts it
to a temp directory, passes `--plugin-dir`, and the mod calls back to the
session's loopback endpoint (it cannot listen; it long-polls for commands).
When it connects, `Send` and `Interrupt` stop typing into the TUI. The
function hooks API is early access, so a session whose mod never says hello
(older CLI, policy, protocol change) reports a `bridge_mod` notice and keeps
using the keyboard. Prompts submitted through it are recorded as the user's.

### Screen-reader profile

`ScreenReader: true` runs `--ax-screen-reader`: no borders or animation, and
every dialog ends with an instruction line that says how to answer it
(`Select with numbers [1-4]…`, `…comma- or space-separated for several…`,
`Enter text for option 3 (Other)…`, `Enter y/n:`). The profile parses that
grammar and answers with typed input plus Enter. It reads less like the
normal TUI in the Terminal view, but it is the rendering assistive
technology relies on.

### Drift detection

Each observed screen is cross-checked against the registry. When the engine
reports a permission prompt or question that the profile cannot read, or
reports idle while no prompt is recognized, for longer than a redraw takes,
the session emits `EventDiagnostic` (`Kind: "drift"`, with `Screen`). The
same happens when a keyboard action times out (`ErrScreenTimeout`). With
`DriftDir` set, the screen is saved as `<dir>/<version>/<profile>/drift-*.{ansi,txt,json}`,
the fixture layout the tests replay. A CLI outside `VerifiedVersions` is
reported once as a `unverified_version` notice.

The spawned CLI's environment is scrubbed of the session markers a parent
Claude Code sets (`CLAUDECODE`, `CLAUDE_CODE_CHILD_SESSION`, …). Otherwise a
library used from inside Claude Code would start nested sessions with
transcript saving turned off.

## Codex provider

`codex.Options`: `TrustWorkspace`, `Search`, `AddDirs`, `Config`
(`-c key=value`), `Profile`, `Enable`/`Disable` (features).

Modes: `read-only`, `auto` (workspace-write, ask on request), `full-access`
(no sandbox, never ask), and `plan` (Codex's Plan collaboration mode, toggled
with Shift+Tab). Models and efforts come from `~/.codex/models_cache.json`.
Model, effort and preset changes relaunch with `codex resume <id>`.

Codex cannot be given a session id up front, so a new session finds its
rollout file by working directory and start time (`EventSessionID` fires once
it is found). Approvals and `request_user_input` questions are read from the
screen. Question structure comes from the rollout when available.

## Adding a provider

Implement `clicodebridge.Provider`/`Session`. `terminal.Start` gives you the
PTY and screen. `internal/core.Base` handles events, state, settings and
pending requests. `core.Focus`/`core.WaitLines` drive TUI dialogs with
closed-loop navigation: press one key, re-read the screen, repeat.

## Web remote sample

```sh
go run ./examples/webremote -addr 127.0.0.1:8787   # optional: -token secret
```

Open the page, start a Claude Code or Codex session in a directory, and chat.
Permission prompts and questions appear as cards (buttons, radio/checkbox
choices, free-text answers). Files the agent sends appear as cards: images
inline, video and audio with players, text and Markdown previewed, the rest
as downloads. Screenshots and images from tool results show as thumbnails.
The server serves only attachments its sessions' events named, with a
sandboxing Content-Security-Policy. The model, effort and mode selectors call the
`Set*` methods. The **Terminal** tab streams the CLI's live screen
(xterm.js) and forwards your keystrokes. The server uses only the standard
library (SSE + JSON POST). Sessions run with your user's permissions, so keep
it on loopback or set `-token`.

## Testing

```sh
go test -race ./...                                   # unit tests, incl. recorded screens
go run ./cmd/cbcapture -out claude/testdata/fixtures  # record screens from the installed CLI
CLICODEBRIDGE_E2E_DIR=/tmp/cb-e2e go test -tags e2e ./claude -v
CLICODEBRIDGE_E2E_DIR=/tmp/cb-e2e CODEX_BIN=$(which codex) go test -tags e2e ./codex -v
```

`claude/testdata/fixtures/<version>/<profile>/` holds screens recorded by
`cbcapture` as styled ANSI snapshots. The tests replay them through the
emulator (so colors are checked too) and assert every scenario in both
profiles, with and without structured hints. After a CLI update, run
`cbcapture`, then `go test ./claude`: a failing case names the scenario that
drifted. Drift captures from `DriftDir` use the same layout.

The Claude e2e suite covers hook mode with the bridge mod, terminal mode
with and without it, and the screen-reader profile with and without it. Any
drift diagnostic fails a test. The e2e tests drive the installed,
logged-in CLIs. They spend tokens, leave transcripts under `~/.claude` /
`~/.codex`, and mark the scratch directories as trusted.

`.github/workflows/claude-drift.yml` runs all of this nightly against the
latest Claude Code (it needs an `ANTHROPIC_API_KEY` secret and has not been
run in CI yet).

## Compatibility and caveats

- Verified against Claude Code 2.1.296–2.1.311 (screen-reader profile:
  2.1.310–2.1.311) and codex-cli 0.164.0 on Linux. The session registry file format
  and the function hooks API are undocumented or early access. Both have
  fallbacks (`claude agents --json`, then the screen; the keyboard).
- Remaining screen dependencies on Claude: answering dialogs in
  `InteractionTerminal` mode, the trust dialog, and reading the mode after
  Shift+Tab. When they break, `Send`/`Respond`/`Set*` return errors wrapping
  `ErrScreenTimeout` and drift diagnostics fire. Codex still reads its
  screen for readiness and dialogs.
- The PTY layer uses `creack/pty` and targets Linux and macOS.
- Codex hooks are not used yet. Codex permission answers are keyboard-only,
  and a deny message is sent as the follow-up prompt.
- Transcripts contain your conversations in plain text. `Event.Raw` carries
  the original record; drop it before logging or sending events elsewhere,
  as the web sample does.
