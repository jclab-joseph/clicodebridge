package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	// readySettle is how long the input box must be visible before the
	// session counts as idle; the TUI paints the editor lazily on startup.
	readySettle = 500 * time.Millisecond
	// interruptedTurnIdle ends a turn that never got a Stop hook (the user
	// pressed Esc) once the screen has been idle this long.
	interruptedTurnIdle = 3 * time.Second
	// stopFlushGrace bounds how long a turn stays open after the Stop hook
	// while waiting for the transcript's turn_duration record. The hook can
	// fire before the final assistant message is flushed to disk.
	stopFlushGrace   = 2 * time.Second
	startupWarnAfter = 60 * time.Second
)

type session struct {
	*core.Base
	p    *Provider
	opts bridge.SessionOptions
	co   Options
	cwd  string
	hold time.Duration

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
		stop: make(chan struct{}),
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
	t, err := terminal.Start(terminal.Options{
		Path: s.p.cfg.Executable, Args: args, Dir: s.cwd, Env: env,
		Cols: s.opts.Cols, Rows: s.opts.Rows,
	})
	if err != nil {
		return fmt.Errorf("claude: %w", err)
	}
	s.mu.Lock()
	s.term, s.launch, s.ready, s.startedAt, s.warned = t, l, false, time.Now(), false
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
	var lastTerm *terminal.Terminal
	var lastVer uint64
	var lastEval time.Time
	for {
		select {
		case <-s.stop:
			return
		case <-tick.C:
		}
		t := s.currentTerm()
		v := t.Version()
		// Re-evaluate unchanged screens periodically for time-based rules.
		if t == lastTerm && v == lastVer && time.Since(lastEval) < 500*time.Millisecond {
			continue
		}
		lastTerm, lastVer, lastEval = t, v, time.Now()
		s.observe(t, analyze(t.Lines()))
	}
}

func (s *session) observe(t *terminal.Terminal, info screenInfo) {
	now := time.Now()
	endInterrupted := false
	s.mu.Lock()
	if s.term != t || s.restarting {
		s.mu.Unlock()
		return
	}
	if info.InputBox && !s.ready {
		s.ready, s.readySince = true, now
	}
	if !s.ready && !s.warned && now.Sub(s.startedAt) > startupWarnAfter && info.Trust == nil {
		s.warned = true
		go s.Errorf(fmt.Errorf("claude has not shown its prompt after %s; screen:\n%s", startupWarnAfter, strings.Join(t.Lines(), "\n")))
	}
	if s.turnActive && info.idle() && !s.HasPending() {
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

	if endInterrupted {
		s.endTurn("", true)
	}
	s.UpdateSettings(func(st *bridge.Settings) {
		if info.Mode != "" {
			st.Mode = info.Mode
		}
		if info.Effort != "" {
			st.Effort = info.Effort
		}
		if info.RemoteURL != "" {
			st.RemoteControlURL = info.RemoteURL
		}
	})
	s.observeTrust(info)
	s.trackDialogs(info)

	s.mu.Lock()
	turn := s.turnActive
	s.mu.Unlock()
	switch {
	case s.HasPending() || info.dialogVisible():
		s.SetState(bridge.StateWaiting)
	case !ready:
		s.SetState(bridge.StateStarting)
	case info.Busy || turn:
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
			err := s.answerTrust(context.Background(), true)
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
func (s *session) trackDialogs(info screenInfo) {
	visible := info.Permission != nil || info.Question != nil || info.Review != nil
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
		s.stopPending, s.stopAt, s.stopText = true, time.Now(), text
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

// --- Session API -------------------------------------------------------------

func (s *session) waitStarted(ctx context.Context) error {
	return s.WaitFor(ctx, 100*time.Millisecond, func() bool {
		st := s.State()
		return st != bridge.StateStarting
	})
}

// waitInputReady waits for StateIdle. When no turn is running and nothing is
// pending, the screen may still lag a just-finished turn, so it waits a
// little instead of failing; otherwise it returns ErrBusy.
func (s *session) waitInputReady(ctx context.Context) error {
	if err := s.waitStarted(ctx); err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_ = s.WaitFor(wctx, 50*time.Millisecond, func() bool {
		if s.State() == bridge.StateIdle {
			return true
		}
		s.mu.Lock()
		active := s.turnActive
		s.mu.Unlock()
		return active || s.HasPending()
	})
	if s.State() == bridge.StateIdle {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return bridge.ErrBusy
}

// Send submits a prompt.
func (s *session) Send(ctx context.Context, prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return errors.New("claude: empty prompt")
	}
	s.ctl.Lock()
	defer s.ctl.Unlock()
	if err := s.waitInputReady(ctx); err != nil {
		return err
	}
	t := s.currentTerm()
	seen := make(chan struct{})
	s.mu.Lock()
	s.promptSeen = seen
	s.mu.Unlock()
	if err := core.Press(ctx, t, terminal.KeyCtrlU, terminal.BracketedPaste(prompt)); err != nil {
		return err
	}
	core.Settle(ctx, t, 150*time.Millisecond, 1500*time.Millisecond)
	// Claude sometimes drops an Enter that arrives while it is still
	// processing pasted input; UserPromptSubmit confirms acceptance.
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := t.WriteString(terminal.KeyEnter); err != nil {
			return err
		}
		select {
		case <-seen:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-s.Done():
			return bridge.ErrExited
		case <-time.After(2500 * time.Millisecond):
		}
		if analyze(t.Lines()).Busy {
			return nil
		}
	}
	return errors.New("claude: the prompt was not accepted")
}

// Command runs a slash command.
func (s *session) Command(ctx context.Context, command string) error {
	s.ctl.Lock()
	defer s.ctl.Unlock()
	return s.commandLocked(ctx, command)
}

func (s *session) commandLocked(ctx context.Context, command string) error {
	if err := s.waitInputReady(ctx); err != nil {
		return err
	}
	if !strings.HasPrefix(command, "/") {
		command = "/" + command
	}
	t := s.currentTerm()
	if err := core.Press(ctx, t, terminal.KeyCtrlU, command); err != nil {
		return err
	}
	core.Settle(ctx, t, 200*time.Millisecond, 1500*time.Millisecond)
	if _, err := t.WriteString(terminal.KeyEnter); err != nil {
		return err
	}
	core.Settle(ctx, t, 300*time.Millisecond, 3*time.Second)
	return nil
}

// Interrupt presses Esc, which stops the running turn (or cancels a dialog).
func (s *session) Interrupt(ctx context.Context) error {
	_, err := s.currentTerm().WriteString(terminal.KeyEscape)
	return err
}

// Respond answers a pending request.
func (s *session) Respond(ctx context.Context, id string, r bridge.Response) error {
	p := s.FindPending(id)
	if p == nil {
		return bridge.ErrUnknownRequest
	}
	if !r.Cancel {
		switch p.Req.Kind {
		case bridge.RequestQuestion:
			if err := bridge.ValidateAnswers(p.Req.Questions, r.Answers); err != nil {
				return err
			}
		case bridge.RequestPermission:
			if _, err := p.Req.Permission.ResolveOption(r); err != nil {
				return err
			}
		}
	}
	switch d := p.Data.(type) {
	case trustPending:
		opt, _ := p.Req.Permission.ResolveOption(r)
		s.ctl.Lock()
		err := s.answerTrust(ctx, !r.Cancel && opt.Kind != bridge.Deny)
		s.ctl.Unlock()
		if err == nil {
			s.Resolve(id, bridge.ResolvedByClient)
		}
		return err
	case *hookPending:
		s.mu.Lock()
		if d.held {
			select {
			case d.answer <- r:
			default:
				s.mu.Unlock()
				return errors.New("claude: request already answered")
			}
			s.mu.Unlock()
			return nil
		}
		if d.answering {
			s.mu.Unlock()
			return errors.New("claude: request is already being answered")
		}
		d.answering = true
		s.mu.Unlock()
		s.ctl.Lock()
		var err error
		if p.Req.Kind == bridge.RequestQuestion {
			err = s.answerQuestion(ctx, &p.Req, r)
		} else {
			err = s.answerPermission(ctx, &p.Req, r)
		}
		s.ctl.Unlock()
		s.mu.Lock()
		d.answering = false
		s.mu.Unlock()
		if err == nil {
			s.Resolve(id, bridge.ResolvedByClient)
		}
		return err
	}
	return bridge.ErrUnknownRequest
}

// --- answering dialogs by keyboard -----------------------------------------

func (s *session) answerTrust(ctx context.Context, trust bool) error {
	t := s.currentTerm()
	want := trustRows[1]
	if trust {
		want = trustRows[0]
	}
	err := core.Focus(ctx, t, func(lines []string) (int, int, bool) {
		d := analyze(lines).Trust
		if d == nil || d.Focused < 0 {
			return 0, 0, false
		}
		return d.Focused, d.index(want), d.index(want) >= 0
	})
	if err != nil {
		return err
	}
	_, err = t.WriteString(terminal.KeyEnter)
	return err
}

func (s *session) answerPermission(ctx context.Context, req *bridge.Request, r bridge.Response) error {
	t := s.currentTerm()
	lines, err := core.WaitLines(ctx, t, 10*time.Second, func(l []string) bool { return analyze(l).Permission != nil })
	if err != nil {
		return fmt.Errorf("claude: permission dialog not on screen: %w", err)
	}
	if r.Cancel {
		_, err := t.WriteString(terminal.KeyEscape)
		return err
	}
	opt, _ := req.Permission.ResolveOption(r)
	d := analyze(lines).Permission
	ord := screenOrdinal(d, opt)
	if ord == 0 {
		return fmt.Errorf("claude: no on-screen option matches %q", opt.Label)
	}
	_, err = t.WriteString(strconv.Itoa(ord))
	return err
}

// screenOrdinal maps a request option to the TUI option to press.
func screenOrdinal(d *permissionDialog, opt bridge.PermissionOption) int {
	if opt.Kind == bridge.OtherOption {
		// "Allow and switch to <mode> mode" -> "Yes, and switch to <mode> mode".
		if words := strings.Fields(strings.ToLower(opt.Label)); len(words) >= 2 {
			mode := words[len(words)-2]
			for _, o := range d.Options {
				l := strings.ToLower(o.Label)
				if strings.Contains(l, "switch to") && strings.Contains(l, mode) {
					return o.Ordinal
				}
			}
		}
	}
	for _, o := range d.Options {
		if classifyPermissionLabel(o.Label) == opt.Kind {
			return o.Ordinal
		}
	}
	if opt.Kind == bridge.OtherOption {
		for _, o := range d.Options {
			if classifyPermissionLabel(o.Label) == bridge.AllowOnce {
				return o.Ordinal
			}
		}
	}
	return 0
}

func normalizeSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// sameQuestion matches the on-screen question (possibly wrapped, so only its
// first row) against the full question text.
func sameQuestion(onScreen, text string) bool {
	a, b := normalizeSpace(onScreen), normalizeSpace(text)
	return a != "" && strings.HasPrefix(b, a)
}

func (s *session) answerQuestion(ctx context.Context, req *bridge.Request, r bridge.Response) error {
	t := s.currentTerm()
	focus := func(kind questionRowKind) error {
		return core.Focus(ctx, t, func(lines []string) (int, int, bool) {
			d := analyze(lines).Question
			if d == nil {
				return 0, 0, false
			}
			return d.focused(), d.find(kind, 0), d.focused() >= 0 && d.find(kind, 0) >= 0
		})
	}
	typeText := func(text string) error {
		text = strings.Join(strings.Fields(text), " ")
		if _, err := t.WriteString(text); err != nil {
			return err
		}
		core.Settle(ctx, t, 150*time.Millisecond, 1500*time.Millisecond)
		return nil
	}
	for i, q := range req.Questions {
		lines, err := core.WaitLines(ctx, t, 10*time.Second, func(l []string) bool {
			d := analyze(l).Question
			return d != nil && sameQuestion(d.Question, q.Text)
		})
		if err != nil {
			return fmt.Errorf("claude: question %d not on screen: %w", i+1, err)
		}
		if r.Cancel {
			_, err := t.WriteString(terminal.KeyEscape)
			return err
		}
		a := r.Answers[i]
		if !q.MultiSelect {
			if a.Text != "" {
				if err := focus(rowText); err != nil {
					return err
				}
				if err := typeText(a.Text); err != nil {
					return err
				}
				if _, err := t.WriteString(terminal.KeyEnter); err != nil {
					return err
				}
			} else if _, err := t.WriteString(strconv.Itoa(q.OptionIndex(a.Selected[0]) + 1)); err != nil {
				return err
			}
		} else {
			d := analyze(lines).Question
			for _, label := range a.Selected {
				idx := q.OptionIndex(label)
				if row := d.find(rowOption, idx+1); row >= 0 && d.Rows[row].Checked {
					continue // already checked; pressing would uncheck it
				}
				if err := core.Press(ctx, t, strconv.Itoa(idx+1)); err != nil {
					return err
				}
				core.Settle(ctx, t, 100*time.Millisecond, time.Second)
			}
			if a.Text != "" {
				if err := focus(rowText); err != nil {
					return err
				}
				if err := typeText(a.Text); err != nil {
					return err
				}
			}
			if err := focus(rowSubmit); err != nil {
				return err
			}
			if _, err := t.WriteString(terminal.KeyEnter); err != nil {
				return err
			}
		}
		core.Settle(ctx, t, 150*time.Millisecond, 1500*time.Millisecond)
	}
	// Multi-question and multi-select dialogs end on a review tab.
	lines, _ := core.WaitLines(ctx, t, 2*time.Second, func(l []string) bool {
		info := analyze(l)
		return info.Review != nil || (info.Question == nil && !info.dialogVisible())
	})
	if rv := analyze(lines).Review; rv != nil {
		if ord := rv.submitOrdinal(); ord > 0 {
			_, err := t.WriteString(strconv.Itoa(ord))
			return err
		}
	}
	return nil
}

// --- settings ----------------------------------------------------------------

func inCycle(mode string) bool {
	for _, m := range shiftTabCycle {
		if m == mode {
			return true
		}
	}
	return false
}

// SetMode switches the permission mode, with Shift+Tab when the mode is in
// the TUI's cycle, otherwise by relaunching.
func (s *session) SetMode(ctx context.Context, mode string) error {
	m := normalizeMode(mode)
	switch m {
	case ModeManual, ModeAcceptEdits, ModePlan, ModeAuto, ModeDontAsk, ModeBypass:
	default:
		return fmt.Errorf("claude: unknown mode %q", mode)
	}
	s.ctl.Lock()
	defer s.ctl.Unlock()
	if err := s.waitStarted(ctx); err != nil {
		return err
	}
	if st := s.State(); st != bridge.StateIdle && st != bridge.StateBusy {
		return bridge.ErrBusy
	}
	t := s.currentTerm()
	if inCycle(m) {
		for i := 0; i < len(shiftTabCycle)+2; i++ {
			cur := analyze(t.Lines()).Mode
			if cur == m {
				s.mu.Lock()
				s.launch.mode = m
				s.mu.Unlock()
				s.UpdateSettings(func(st *bridge.Settings) { st.Mode = m })
				return nil
			}
			if cur == "" {
				break
			}
			v := t.Version()
			if _, err := t.WriteString(terminal.KeyShiftTab); err != nil {
				return err
			}
			_, _ = core.WaitLines(ctx, t, time.Second, func([]string) bool { return t.Version() != v })
			core.Settle(ctx, t, 150*time.Millisecond, time.Second)
		}
	}
	if s.State() != bridge.StateIdle {
		return bridge.ErrBusy
	}
	return s.restartLocked(ctx, func(l *launch) { l.mode = m })
}

// SetModel switches the model.
func (s *session) SetModel(ctx context.Context, model string) error {
	if strings.TrimSpace(model) == "" {
		return errors.New("claude: empty model")
	}
	s.ctl.Lock()
	defer s.ctl.Unlock()
	if !s.co.LiveSettingChanges {
		if err := s.requireIdle(ctx); err != nil {
			return err
		}
		return s.restartLocked(ctx, func(l *launch) { l.model = model })
	}
	if err := s.commandLocked(ctx, "/model "+model); err != nil {
		return err
	}
	t := s.currentTerm()
	lines, _ := core.WaitLines(ctx, t, 2*time.Second, func(l []string) bool { return analyze(l).ModelSwap })
	if analyze(lines).ModelSwap {
		if _, err := t.WriteString("1"); err != nil {
			return err
		}
	}
	s.UpdateSettings(func(st *bridge.Settings) { st.Model = model })
	return nil
}

// SetEffort switches the reasoning effort.
func (s *session) SetEffort(ctx context.Context, effort string) error {
	e := normalizeEffort(effort)
	if e == "" {
		return fmt.Errorf("claude: unknown effort %q", effort)
	}
	s.ctl.Lock()
	defer s.ctl.Unlock()
	if !s.co.LiveSettingChanges {
		if err := s.requireIdle(ctx); err != nil {
			return err
		}
		return s.restartLocked(ctx, func(l *launch) { l.effort = e })
	}
	if err := s.commandLocked(ctx, "/effort "+e); err != nil {
		return err
	}
	s.UpdateSettings(func(st *bridge.Settings) { st.Effort = e })
	return nil
}

func (s *session) requireIdle(ctx context.Context) error { return s.waitInputReady(ctx) }

// restartLocked relaunches the CLI on the same conversation with changed
// launch flags. Caller holds s.ctl and has checked the session is idle.
func (s *session) restartLocked(ctx context.Context, mutate func(*launch)) error {
	s.mu.Lock()
	old := s.term
	l := s.launch
	mutate(&l)
	l.resume, l.sessionID, l.fork = s.Base.ID(), "", false
	s.restarting = true
	s.mu.Unlock()
	s.SetState(bridge.StateStarting)

	_ = core.Press(ctx, old, terminal.KeyCtrlU, "/exit")
	core.Settle(ctx, old, 150*time.Millisecond, time.Second)
	_, _ = old.WriteString(terminal.KeyEnter)
	select {
	case <-old.Done():
	case <-time.After(5 * time.Second):
	}
	_ = old.Close()

	err := s.startTerm(l)
	s.mu.Lock()
	s.restarting = false
	s.mu.Unlock()
	if err != nil {
		s.shutdown("restart failed: " + err.Error())
		return err
	}
	s.UpdateSettings(func(st *bridge.Settings) {
		if l.model != "" {
			st.Model = l.model
		}
		if l.effort != "" {
			st.Effort = l.effort
		}
		if l.mode != "" {
			st.Mode = l.mode
		}
	})
	s.Emit(bridge.Event{Type: bridge.EventRestarted})
	wctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return s.waitStarted(wctx)
}

// Close exits the CLI.
func (s *session) Close() error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		<-s.Done()
		return nil
	}
	s.closing = true
	t := s.term
	s.mu.Unlock()
	// Ctrl+C twice is Claude's exit shortcut from any screen.
	_, _ = t.WriteString(terminal.KeyCtrlC)
	time.Sleep(150 * time.Millisecond)
	_, _ = t.WriteString(terminal.KeyCtrlC)
	select {
	case <-t.Done():
	case <-time.After(5 * time.Second):
	}
	_ = t.Close()
	<-s.Done()
	return nil
}
