package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	bridge "github.com/jclab-joseph/clicodebridge"
	"github.com/jclab-joseph/clicodebridge/internal/core"
	"github.com/jclab-joseph/clicodebridge/terminal"
)

const (
	defaultHold = 9 * time.Minute
	// maxHold keeps the hook timeout (hold + 30s) under Claude's 10m cap.
	maxHold = 9*time.Minute + 30*time.Second
	// readySettle is how long the session must have been ready before it
	// counts as idle; the TUI paints the editor lazily on startup.
	readySettle = 500 * time.Millisecond
	// interruptedTurnIdle ends a turn that never got a Stop hook (the user
	// pressed Esc without the bridge mod) once it has been idle this long.
	interruptedTurnIdle = 3 * time.Second
	// stopFlushGrace bounds how long a turn stays open after the Stop hook
	// while waiting for the transcript's turn_duration record. The hook can
	// fire before the final assistant message is flushed to disk.
	stopFlushGrace   = 2 * time.Second
	startupWarnAfter = 60 * time.Second
	// registryFallbackAfter is how long a missing registry file is
	// tolerated before polling `claude agents --json` instead.
	registryFallbackAfter = 8 * time.Second
)

type session struct {
	*core.Base
	p    *Provider
	opts bridge.SessionOptions
	co   Options
	cwd  string
	hold time.Duration
	prof profile
	mod  *modLink

	hookPrefix string
	unregister func()

	// ctl serializes operations that type into the TUI.
	ctl sync.Mutex

	mu           sync.Mutex
	term         *terminal.Terminal
	launch       launch
	restarting   bool
	closing      bool
	ready        bool
	readySince   time.Time
	startedAt    time.Time
	warned       bool
	modWarned    bool
	turnActive   bool
	turnStart    time.Time
	stopPending  bool // Stop hook seen; waiting for the transcript to catch up
	stopAt       time.Time
	stopText     string
	lastText     string // last assistant text of the current turn
	idleSince    time.Time
	promptSeen   chan struct{}
	trustReq     string
	trustBusy    bool
	expectTrust  bool
	reg          registry // last session-registry reading
	regFallback  bool     // polling `claude agents --json`
	version      string
	drift        driftState
	stop         chan struct{}
	shutdownOnce sync.Once

	tailMu sync.Mutex
	tail   *tailer
}

// hookPending is the provider data of a request raised by the
// PermissionRequest hook.
type hookPending struct {
	in        *hookInput
	held      bool // the hook call is still open (InteractionHook)
	answer    chan bridge.Response
	seen      bool // its dialog has been seen on screen
	missed    int  // consecutive screens without a dialog
	answering bool
}

type trustPending struct{}

// Start launches a Claude Code session.
func (p *Provider) Start(ctx context.Context, opts bridge.SessionOptions) (bridge.Session, error) {
	co, err := providerOptions(opts.Provider)
	if err != nil {
		return nil, err
	}
	cwd := opts.Cwd
	if cwd == "" {
		if cwd, err = os.Getwd(); err != nil {
			return nil, err
		}
	}
	if cwd, err = filepath.Abs(cwd); err != nil {
		return nil, err
	}
	l := launch{model: opts.Model, mode: normalizeMode(opts.Mode), extraArgs: opts.Args}
	if opts.Effort != "" {
		if l.effort = normalizeEffort(opts.Effort); l.effort == "" {
			return nil, fmt.Errorf("claude: unknown effort %q", opts.Effort)
		}
	}
	hold := co.HookHoldTimeout
	if hold <= 0 {
		hold = defaultHold
	}
	hold = min(hold, maxHold)

	s := &session{
		Base: core.NewBase("claude"),
		p:    p, opts: opts, co: co, cwd: cwd, hold: hold,
		prof: classicProfile{},
		mod:  newModLink(),
		stop: make(chan struct{}),
	}
	if co.ScreenReader {
		s.prof = screenReaderProfile{}
	}
	if accepted, ok := trustAccepted(p.configDir(), cwd); !ok || !accepted {
		s.expectTrust = true
	}
	if co.Bridge != BridgeOff {
		if dir, err := bridgeModDir(); err == nil {
			l.pluginDir = dir
		}
	}
	id := opts.Resume
	if id != "" {
		l.resume, l.fork = id, co.ForkSession
	} else {
		id = uuid.NewString()
		l.sessionID = id
	}
	if s.hookPrefix, s.unregister, err = p.hooks.Register(s.onHook); err != nil {
		return nil, fmt.Errorf("claude: start hook server: %w", err)
	}
	s.SetID(id)
	s.UpdateSettings(func(st *bridge.Settings) { st.Model, st.Effort, st.Mode = l.model, l.effort, l.mode })
	s.tail = newTailer(transcriptPath(p.configDir(), cwd, id))
	if err := s.startTerm(l); err != nil {
		s.unregister()
		return nil, err
	}
	go s.screenLoop()
	go s.transcriptLoop()
	return s, nil
}

func (s *session) startTerm(l launch) error {
	settings, err := s.co.settingsJSON(s.hookPrefix, s.hold)
	if err != nil {
		return err
	}
	args, err := s.co.args(l, settings)
	if err != nil {
		return err
	}
	env := core.Environ(sessionMarkerEnv, append(append([]string(nil), s.p.cfg.Env...), s.opts.Env...)...)
	env = core.SetEnv(env, "TERM", "xterm-256color")
	if !core.HasEnv(env, "CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION") {
		env = append(env, "CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=false")
	}
	if l.pluginDir != "" {
		env = core.SetEnv(env, "CLICODEBRIDGE_MOD_URL", s.hookPrefix)
	}
	t, err := terminal.Start(terminal.Options{
		Path: s.p.cfg.Executable, Args: args, Dir: s.cwd, Env: env,
		Cols: s.opts.Cols, Rows: s.opts.Rows,
	})
	if err != nil {
		return fmt.Errorf("claude: %w", err)
	}
	s.mu.Lock()
	s.term, s.launch, s.ready, s.startedAt, s.warned, s.modWarned = t, l, false, time.Now(), false, false
	s.reg = registry{}
	s.mu.Unlock()
	go s.watchExit(t)
	return nil
}

func (s *session) currentTerm() *terminal.Terminal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.term
}

// Terminal returns the PTY the CLI currently runs on.
func (s *session) Terminal() *terminal.Terminal { return s.currentTerm() }

// io returns a screen driver bound to the current terminal and profile.
func (s *session) io() *screenIO { return &screenIO{t: s.currentTerm(), prof: s.prof, hint: s.hint} }

// hint collects what structured channels say about the screen.
func (s *session) hint() screenHint {
	s.mu.Lock()
	reg, ready, expectTrust := s.reg, s.ready, s.expectTrust
	s.mu.Unlock()
	h := screenHint{registryKnown: reg.ok && reg.Status != "", expectTrust: expectTrust && !ready}
	if reg.Status == "waiting" {
		h.waitingFor = reg.WaitingFor
	}
	for _, p := range s.PendingWhere(func(p *core.Pending) bool { return p.Req.Kind == bridge.RequestQuestion }) {
		h.questions = p.Req.Questions
		break
	}
	return h
}

func (s *session) cliVersion() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

func (s *session) setCLIVersion(v string) {
	if v == "" {
		return
	}
	s.mu.Lock()
	changed := s.version != v
	s.version = v
	s.mu.Unlock()
	if changed {
		s.noteVersion(v)
	}
}

func (s *session) watchExit(t *terminal.Terminal) {
	err := t.Wait()
	s.mu.Lock()
	replaced := s.restarting || s.term != t
	closing := s.closing
	s.mu.Unlock()
	if replaced {
		return
	}
	reason := "claude exited"
	switch {
	case closing:
		reason = "closed"
	case err != nil:
		reason = "claude exited: " + err.Error()
	}
	s.shutdown(reason)
}

func (s *session) shutdown(reason string) {
	s.shutdownOnce.Do(func() {
		close(s.stop)
		s.unregister()
		s.flushTranscript()
		if t := s.currentTerm(); t != nil {
			_ = t.Close()
		}
		s.Finish(reason)
	})
}

// --- background loops ------------------------------------------------------

func (s *session) screenLoop() {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-tick.C:
		}
		t := s.currentTerm()
		s.readRegistry(t)
		io := &screenIO{t: t, prof: s.prof, hint: s.hint}
		s.observe(t, io.info())
	}
}

// readRegistry refreshes s.reg from the session record (cheap: one small
// file) or, when that file is missing, from `claude agents --json`.
func (s *session) readRegistry(t *terminal.Terminal) {
	reg, ok := readRegistryFile(s.p.configDir(), t.Pid())
	if ok && reg.SessionID != "" && reg.SessionID != s.ID() {
		ok = false // a stale record of a recycled pid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		s.reg = reg
		return
	}
	if s.regFallback || time.Since(s.startedAt) < registryFallbackAfter {
		return
	}
	s.regFallback = true
	go func() {
		for {
			r, ok := readRegistryCommand(context.Background(), s.p.cfg.Executable, s.ID())
			s.mu.Lock()
			if ok {
				s.reg = r
			}
			s.mu.Unlock()
			select {
			case <-s.stop:
				return
			case <-time.After(3 * time.Second):
			}
			if _, ok := readRegistryFile(s.p.configDir(), s.currentTerm().Pid()); ok {
				s.mu.Lock()
				s.regFallback = false
				s.mu.Unlock()
				return
			}
		}
	}()
}

func (s *session) observe(t *terminal.Terminal, info screenInfo) {
	now := time.Now()
	endInterrupted := false
	s.mu.Lock()
	if s.term != t || s.restarting {
		s.mu.Unlock()
		return
	}
	reg := s.reg
	engineUp := reg.ok && reg.Status != ""
	if (engineUp || info.InputBox) && !s.ready {
		s.ready, s.readySince = true, now
	}
	if !s.ready && !s.warned && now.Sub(s.startedAt) > startupWarnAfter && info.Trust == nil {
		s.warned = true
		go s.Errorf(fmt.Errorf("claude has not shown its prompt after %s; screen:\n%s", startupWarnAfter, t.Screen().Text()))
	}
	modMissing := s.launch.pluginDir != "" && s.ready && !s.modWarned && now.Sub(s.readySince) > 20*time.Second && !s.mod.isReady()
	if modMissing {
		s.modWarned = true
	}
	idleNow := info.idle()
	if engineUp {
		idleNow = reg.Status == "idle"
	}
	if s.turnActive && idleNow && !s.HasPending() {
		if s.idleSince.IsZero() {
			s.idleSince = now
		} else if now.Sub(s.idleSince) > interruptedTurnIdle && now.Sub(s.turnStart) > interruptedTurnIdle {
			endInterrupted = true
		}
	} else {
		s.idleSince = time.Time{}
	}
	ready := s.ready && now.Sub(s.readySince) >= readySettle
	s.mu.Unlock()

	if modMissing {
		s.Emit(bridge.Event{Type: bridge.EventNotice, Kind: "bridge_mod",
			Text: "bridge mod did not connect; prompts and interrupts use the keyboard"})
	}
	if endInterrupted {
		s.endTurn("", true)
	}
	s.setCLIVersion(reg.Version)
	s.UpdateSettings(func(st *bridge.Settings) {
		if info.Mode != "" {
			st.Mode = info.Mode
		}
		if info.Effort != "" {
			st.Effort = info.Effort
		}
		if u := remoteControlURL(reg.BridgeSessionID); u != "" {
			st.RemoteControlURL = u
		} else if info.RemoteURL != "" {
			st.RemoteControlURL = info.RemoteURL
		}
	})
	s.observeTrust(info)
	s.trackDialogs(info, reg)
	s.checkDrift(reg, info, ready)

	s.mu.Lock()
	turn := s.turnActive
	s.mu.Unlock()
	waiting := s.HasPending() || info.dialogVisible() || (engineUp && reg.Status == "waiting")
	busy := info.Busy || turn
	if engineUp {
		busy = reg.Status == "busy" || turn
	}
	switch {
	case waiting:
		s.SetState(bridge.StateWaiting)
	case !ready:
		s.SetState(bridge.StateStarting)
	case busy:
		s.SetState(bridge.StateBusy)
	default:
		s.SetState(bridge.StateIdle)
	}
}

// observeTrust surfaces or answers the workspace trust dialog.
func (s *session) observeTrust(info screenInfo) {
	s.mu.Lock()
	reqID, busy := s.trustReq, s.trustBusy
	if info.Trust == nil {
		s.trustReq = ""
		s.mu.Unlock()
		if reqID != "" {
			s.Resolve(reqID, bridge.ResolvedExternally)
		}
		return
	}
	if busy || reqID != "" {
		s.mu.Unlock()
		return
	}
	if s.co.TrustWorkspace {
		s.trustBusy = true
		s.mu.Unlock()
		go func() {
			err := s.prof.answerTrust(context.Background(), s.io(), true)
			time.Sleep(time.Second) // let the dialog close before re-checking
			s.mu.Lock()
			s.trustBusy = false
			s.mu.Unlock()
			if err != nil {
				s.Errorf(fmt.Errorf("claude: answer workspace trust: %w", err))
			}
		}()
		return
	}
	p := &core.Pending{Data: trustPending{}, Req: bridge.Request{
		ID: core.NewID(), Kind: bridge.RequestPermission, Title: "Trust this folder?",
		Permission: &bridge.Permission{Detail: s.cwd, Options: []bridge.PermissionOption{
			{ID: "trust", Label: "Yes, I trust this folder", Kind: bridge.AllowOnce},
			{ID: "exit", Label: "No, exit", Kind: bridge.Deny},
		}},
	}}
	s.trustReq = p.Req.ID
	s.mu.Unlock()
	s.AddPending(p, true)
}

// trackDialogs resolves terminal-mode requests whose dialog disappeared
// without Respond: someone answered in the TUI or via remote control.
func (s *session) trackDialogs(info screenInfo, reg registry) {
	visible := info.Permission != nil || info.Question != nil || info.Review != nil || info.TextEntry > 0
	if reg.ok && reg.Status != "" {
		visible = reg.Status == "waiting" && (reg.WaitingFor == waitingPermission || reg.WaitingFor == waitingInput)
	}
	var gone []string
	s.mu.Lock()
	for _, p := range s.PendingWhere(func(p *core.Pending) bool { _, ok := p.Data.(*hookPending); return ok }) {
		hp := p.Data.(*hookPending)
		if hp.held || hp.answering {
			continue
		}
		if visible {
			hp.seen, hp.missed = true, 0
		} else if hp.seen {
			if hp.missed++; hp.missed >= 3 {
				gone = append(gone, p.Req.ID)
			}
		}
	}
	s.mu.Unlock()
	for _, id := range gone {
		s.Resolve(id, bridge.ResolvedExternally)
	}
}

func (s *session) transcriptLoop() {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-tick.C:
			s.flushTranscript()
			s.mu.Lock()
			late := s.stopPending && time.Since(s.stopAt) > stopFlushGrace
			s.mu.Unlock()
			if late {
				s.endTurn("", false)
			}
		}
	}
}

func (s *session) flushTranscript() {
	s.tailMu.Lock()
	defer s.tailMu.Unlock()
	lines, err := s.tail.poll()
	if err != nil {
		s.Errorf(fmt.Errorf("claude: read transcript: %w", err))
		return
	}
	for _, line := range lines {
		p := parseRecord(line)
		for _, ev := range p.events {
			if ev.Type == bridge.EventAssistantText {
				s.mu.Lock()
				s.lastText = ev.Text
				s.mu.Unlock()
			}
			s.Emit(ev)
		}
		if p.remoteURL != "" || p.mode != "" || p.model != "" {
			s.UpdateSettings(func(st *bridge.Settings) {
				if p.remoteURL != "" {
					st.RemoteControlURL = p.remoteURL
				}
				if p.mode != "" {
					st.Mode = p.mode
				}
				if p.model != "" && st.Model == "" {
					st.Model = p.model
				}
			})
		}
		if p.turnEnd {
			s.endTurn("", false)
		}
	}
}

// --- turns -----------------------------------------------------------------

func (s *session) startTurn(prompt string) {
	s.mu.Lock()
	if s.promptSeen != nil {
		close(s.promptSeen)
		s.promptSeen = nil
	}
	if s.turnActive {
		s.mu.Unlock()
		return
	}
	s.turnActive, s.turnStart = true, time.Now()
	s.stopPending, s.stopText, s.lastText = false, "", ""
	s.mu.Unlock()
	s.Emit(bridge.Event{Type: bridge.EventTurnStarted, Text: prompt})
	s.SetState(bridge.StateBusy)
}

// stopTurn records the Stop hook; the turn ends once the transcript has
// caught up (turn_duration) or after stopFlushGrace.
func (s *session) stopTurn(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnActive {
		s.stopPending, s.stopAt = true, time.Now()
		if text != "" {
			s.stopText = text
		}
	}
}

// endTurn emits EventTurnCompleted once per turn. An empty text falls back
// to the Stop hook's message, then to the last assistant text seen.
func (s *session) endTurn(text string, interrupted bool) {
	s.mu.Lock()
	if !s.turnActive {
		s.mu.Unlock()
		return
	}
	if text == "" {
		text = s.stopText
	}
	if text == "" && !interrupted {
		text = s.lastText
	}
	s.turnActive, s.stopPending = false, false
	s.mu.Unlock()
	s.ResolveAll(bridge.ResolvedCancelled)
	s.Emit(bridge.Event{Type: bridge.EventTurnCompleted, Text: text, Interrupted: interrupted})
}

// --- hooks -----------------------------------------------------------------

func (s *session) onHook(ctx context.Context, event string, body []byte) []byte {
	if strings.HasPrefix(event, "mod.") {
		return s.onModHook(ctx, event, body)
	}
	var in hookInput
	if err := json.Unmarshal(body, &in); err != nil {
		return nil
	}
	if in.SessionID != "" {
		s.SetID(in.SessionID) // differs from the launch id after --fork-session
	}
	if in.TranscriptPath != "" {
		s.tailMu.Lock()
		if s.tail.path != in.TranscriptPath {
			s.tail = newTailer(in.TranscriptPath)
		}
		s.tailMu.Unlock()
	}
	s.UpdateSettings(func(st *bridge.Settings) {
		if m := normalizeMode(in.PermissionMode); m != "" {
			st.Mode = m
		}
		if in.Effort != nil && normalizeEffort(in.Effort.Level) != "" {
			st.Effort = normalizeEffort(in.Effort.Level)
		}
	})
	switch event {
	case "UserPromptSubmit":
		s.flushTranscript()
		s.startTurn(in.Prompt)
	case "Stop":
		s.stopTurn(in.LastAssistantMessage)
	case "Notification":
		s.Emit(bridge.Event{Type: bridge.EventNotice, Text: in.Message, Kind: in.NotificationType})
	case "PostToolUse":
		s.resolveByTool(&in)
	case "PermissionRequest":
		return s.onPermissionRequest(ctx, &in)
	}
	return nil
}

// resolveByTool resolves terminal-mode requests for a tool that has now run.
func (s *session) resolveByTool(in *hookInput) {
	for _, p := range s.PendingWhere(func(p *core.Pending) bool {
		hp, ok := p.Data.(*hookPending)
		return ok && !hp.held && !hp.answering && hp.in.ToolName == in.ToolName
	}) {
		s.Resolve(p.Req.ID, bridge.ResolvedExternally)
	}
}

func (s *session) onPermissionRequest(ctx context.Context, in *hookInput) []byte {
	req := bridge.Request{ID: core.NewID(), Kind: bridge.RequestPermission}
	if in.ToolName == askToolName {
		qs, err := questionsFromAsk(in.ToolInput)
		if err != nil || len(qs) == 0 {
			return nil // let the TUI handle it
		}
		req.Kind, req.Questions, req.Title = bridge.RequestQuestion, qs, "Claude has a question"
		if len(qs) == 1 {
			req.Title = qs[0].Text
		}
	} else {
		req.Title = "Allow " + in.ToolName + "?"
		req.Permission = &bridge.Permission{
			ToolName: in.ToolName, Input: in.ToolInput,
			Detail:  permissionDetail(in.ToolName, in.ToolInput),
			Options: permissionOptions(in.PermissionSuggestion),
		}
	}
	hp := &hookPending{in: in}
	p := &core.Pending{Req: req, Data: hp}
	if s.co.Interaction == InteractionTerminal {
		s.AddPending(p, true)
		return nil // Claude shows its dialog; Respond answers it by keyboard
	}
	hp.held, hp.answer = true, make(chan bridge.Response, 1)
	s.AddPending(p, true)
	timer := time.NewTimer(s.hold)
	defer timer.Stop()

	var r bridge.Response
	select {
	case r = <-hp.answer:
	case <-ctx.Done(): // Claude abandoned the call (interrupt)
		s.Resolve(req.ID, bridge.ResolvedCancelled)
		return nil
	case <-s.stop:
		return nil
	case <-timer.C:
		s.mu.Lock()
		select {
		case r = <-hp.answer: // answered at the last moment
			s.mu.Unlock()
		default:
			hp.held = false // fall back to the TUI dialog, same request id
			s.mu.Unlock()
			return nil
		}
	}
	out, err := hookResponse(&p.Req, in, r)
	if err != nil {
		s.Errorf(fmt.Errorf("claude: encode answer: %w", err))
		s.mu.Lock()
		hp.held = false
		s.mu.Unlock()
		return nil
	}
	s.Resolve(req.ID, bridge.ResolvedByClient)
	return out
}
