package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	bridge "github.com/jclab-joseph/clicodebridge"
	"github.com/jclab-joseph/clicodebridge/internal/core"
	"github.com/jclab-joseph/clicodebridge/terminal"
)

const (
	readySettle         = 500 * time.Millisecond
	interruptedTurnIdle = 5 * time.Second
	askWait             = 1500 * time.Millisecond // wait for the rollout's request_user_input record
	startupWarnAfter    = 60 * time.Second
)

// inheritedEnv are variables a parent agent sets for its children.
var inheritedEnv = []string{
	"CODEX_SANDBOX", "CODEX_SANDBOX_NETWORK_DISABLED", "CODEX_THREAD_ID",
	"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID", "AI_AGENT",
}

type launch struct {
	resume string
	model  string
	effort string
	preset string
	extra  []string
}

type dialogKind int

const (
	kindApproval dialogKind = iota
	kindQuestion
	kindTrust
)

// screenPending is the provider data of a request raised from the screen.
type screenPending struct {
	kind      dialogKind
	fp        string
	missed    int
	answering bool
}

type session struct {
	*core.Base
	p    *Provider
	opts bridge.SessionOptions
	co   Options
	cwd  string

	ctl sync.Mutex

	mu         sync.Mutex
	term       *terminal.Terminal
	launch     launch
	restarting bool
	closing    bool
	ready      bool
	readySince time.Time
	startedAt  time.Time
	warned     bool
	turnActive bool
	turnStart  time.Time
	idleSince  time.Time
	lastText   string
	lastAsk    *askCall
	askSince   time.Time // when an unmatched question dialog appeared
	trustBusy  bool

	stop         chan struct{}
	shutdownOnce sync.Once

	tailMu sync.Mutex
	tail   *tailer
}

// Start launches a Codex session.
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
	l := launch{model: opts.Model, effort: opts.Effort, extra: opts.Args}
	plan := false
	switch opts.Mode {
	case "":
	case ModePlan:
		plan = true
	case ModeReadOnly, ModeAuto, ModeFullAccess:
		l.preset = opts.Mode
	default:
		return nil, fmt.Errorf("codex: unknown mode %q", opts.Mode)
	}
	s := &session{Base: core.NewBase("codex"), p: p, opts: opts, co: co, cwd: cwd, stop: make(chan struct{})}
	if opts.Resume != "" {
		l.resume = opts.Resume
		s.SetID(opts.Resume)
		if path := findRolloutByID(p.home(), opts.Resume); path != "" {
			s.attach(path, true)
		}
	}
	s.UpdateSettings(func(st *bridge.Settings) { st.Model, st.Effort, st.Mode = l.model, l.effort, l.preset })
	if err := s.startTerm(l); err != nil {
		return nil, err
	}
	go s.screenLoop()
	go s.rolloutLoop()
	if plan {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if err := s.SetMode(ctx, ModePlan); err != nil {
				s.Errorf(fmt.Errorf("codex: enter plan mode: %w", err))
			}
		}()
	}
	return s, nil
}

func (s *session) args(l launch) []string {
	var a []string
	if l.resume != "" {
		a = append(a, "resume")
	}
	a = append(a, "-C", s.cwd)
	if l.model != "" {
		a = append(a, "-m", l.model)
	}
	if l.effort != "" {
		a = append(a, "-c", "model_reasoning_effort="+strconv.Quote(l.effort))
	}
	a = append(a, presetArgs(l.preset)...)
	if s.co.Search {
		a = append(a, "--search")
	}
	for _, d := range s.co.AddDirs {
		a = append(a, "--add-dir", d)
	}
	for _, c := range s.co.Config {
		a = append(a, "-c", c)
	}
	if s.co.Profile != "" {
		a = append(a, "-p", s.co.Profile)
	}
	for _, f := range s.co.Enable {
		a = append(a, "--enable", f)
	}
	for _, f := range s.co.Disable {
		a = append(a, "--disable", f)
	}
	a = append(a, l.extra...)
	if l.resume != "" {
		a = append(a, l.resume)
	}
	return a
}

func (s *session) startTerm(l launch) error {
	env := core.Environ(inheritedEnv, append(append([]string(nil), s.p.cfg.Env...), s.opts.Env...)...)
	env = core.SetEnv(env, "TERM", "xterm-256color")
	t, err := terminal.Start(terminal.Options{
		Path: s.p.cfg.Executable, Args: s.args(l), Dir: s.cwd, Env: env,
		Cols: s.opts.Cols, Rows: s.opts.Rows,
	})
	if err != nil {
		return fmt.Errorf("codex: %w", err)
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
	reason := "codex exited"
	switch {
	case closing:
		reason = "closed"
	case err != nil:
		reason = "codex exited: " + err.Error()
	}
	s.shutdown(reason)
}

func (s *session) shutdown(reason string) {
	s.shutdownOnce.Do(func() {
		close(s.stop)
		s.flushRollout()
		s.tailMu.Lock()
		if s.tail != nil {
			s.p.claim(s.tail.path, false)
		}
		s.tailMu.Unlock()
		if t := s.currentTerm(); t != nil {
			_ = t.Close()
		}
		s.Finish(reason)
	})
}

// --- rollout -------------------------------------------------------------------

// attach starts tailing a rollout file. atEnd skips existing history.
func (s *session) attach(path string, atEnd bool) {
	s.tailMu.Lock()
	defer s.tailMu.Unlock()
	if s.tail != nil && s.tail.path == path {
		return
	}
	if s.tail != nil {
		s.p.claim(s.tail.path, false)
	}
	t := &tailer{path: path}
	if fi, err := os.Stat(path); err == nil && atEnd {
		t.offset = fi.Size()
	}
	s.tail = t
	s.p.claim(path, true)
}

func (s *session) rolloutLoop() {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-tick.C:
		}
		s.tailMu.Lock()
		attached := s.tail != nil
		s.tailMu.Unlock()
		if !attached {
			s.mu.Lock()
			start := s.startedAt
			s.mu.Unlock()
			if path, meta, ok := findNewRollout(s.p.home(), s.cwd, start, s.p.isClaimed); ok {
				s.attach(path, false)
				s.SetID(meta.ID)
			}
		}
		s.flushRollout()
	}
}

func (s *session) flushRollout() {
	s.tailMu.Lock()
	defer s.tailMu.Unlock()
	if s.tail == nil {
		return
	}
	lines, err := s.tail.poll()
	if err != nil {
		if !os.IsNotExist(err) {
			s.Errorf(fmt.Errorf("codex: read rollout: %w", err))
		}
		return
	}
	for _, line := range lines {
		p := parseLine(line)
		if p.turnStarted {
			s.startTurn()
		}
		for _, ev := range p.events {
			if ev.Type == bridge.EventAssistantText {
				s.mu.Lock()
				s.lastText = ev.Text
				s.mu.Unlock()
			}
			s.Emit(ev)
		}
		if p.ask != nil {
			s.mu.Lock()
			s.lastAsk = p.ask
			s.mu.Unlock()
		}
		if tc := p.settings; tc != nil {
			s.mu.Lock()
			preset := s.launch.preset
			s.mu.Unlock()
			if pp := presetFromPolicy(tc.SandboxPolicy.Type, tc.ApprovalPolicy); pp != "" {
				preset = pp
			}
			s.UpdateSettings(func(st *bridge.Settings) {
				st.Model, st.Effort = tc.Model, tc.Effort
				if tc.CollaborationMode.Mode == "plan" {
					st.Mode = ModePlan
				} else {
					st.Mode = preset
				}
			})
		}
		if p.turnEnded {
			s.endTurn(p.finalText, p.interrupted)
		}
	}
}

func (s *session) startTurn() {
	s.mu.Lock()
	if s.turnActive {
		s.mu.Unlock()
		return
	}
	s.turnActive, s.turnStart, s.lastText = true, time.Now(), ""
	s.mu.Unlock()
	s.Emit(bridge.Event{Type: bridge.EventTurnStarted})
	s.SetState(bridge.StateBusy)
}

func (s *session) endTurn(text string, interrupted bool) {
	s.mu.Lock()
	if !s.turnActive {
		s.mu.Unlock()
		return
	}
	if text == "" && !interrupted {
		text = s.lastText
	}
	s.turnActive = false
	s.mu.Unlock()
	s.ResolveAll(bridge.ResolvedCancelled)
	s.Emit(bridge.Event{Type: bridge.EventTurnCompleted, Text: text, Interrupted: interrupted})
}

// --- screen ----------------------------------------------------------------------

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
		go s.Errorf(fmt.Errorf("codex has not shown its prompt after %s; screen:\n%s", startupWarnAfter, strings.Join(t.Lines(), "\n")))
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
	preset := s.launch.preset
	s.mu.Unlock()

	if endInterrupted {
		s.endTurn("", true)
	}
	if info.InputBox {
		s.UpdateSettings(func(st *bridge.Settings) {
			if info.Plan {
				st.Mode = ModePlan
			} else if st.Mode == ModePlan {
				st.Mode = preset
			}
		})
	}
	s.observeDialogs(info)

	s.mu.Lock()
	turn := s.turnActive
	s.mu.Unlock()
	switch {
	case !ready && !s.HasPending():
		s.SetState(bridge.StateStarting) // startup dialogs answered automatically
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

func optionsFromScreen(opts []screenOption) []bridge.PermissionOption {
	var out []bridge.PermissionOption
	for _, o := range opts {
		label := cleanLabel(o.Label)
		out = append(out, bridge.PermissionOption{ID: "opt:" + strconv.Itoa(o.Ordinal), Label: label, Kind: classifyLabel(label)})
	}
	return out
}

func dialogFingerprint(d *approvalDialog) string {
	var b strings.Builder
	b.WriteString(d.Title + "\x00" + d.Detail)
	for _, o := range d.Options {
		b.WriteString("\x00" + o.Label)
	}
	return b.String()
}

func normalizeSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

func sameQuestion(onScreen, text string) bool {
	a, b := normalizeSpace(onScreen), normalizeSpace(text)
	return a != "" && strings.HasPrefix(b, a)
}

// observeDialogs raises requests for dialogs on screen and resolves those
// whose dialog went away without Respond.
func (s *session) observeDialogs(info screenInfo) {
	current := ""
	switch {
	case info.Trust != nil:
		current = "t:" + dialogFingerprint(info.Trust)
		s.ensureRequest(kindTrust, current, func() bridge.Request {
			return bridge.Request{Kind: bridge.RequestPermission, Title: info.Trust.Title,
				Permission: &bridge.Permission{Detail: s.cwd, Options: optionsFromScreen(info.Trust.Options)}}
		})
	case info.Approval != nil:
		d := info.Approval
		current = "a:" + dialogFingerprint(d)
		s.ensureRequest(kindApproval, current, func() bridge.Request {
			tool := ""
			switch {
			case strings.Contains(d.Title, "run the following command"):
				tool = "exec_command"
			case strings.Contains(d.Title, "edits"):
				tool = "apply_patch"
			}
			return bridge.Request{Kind: bridge.RequestPermission, Title: d.Title,
				Permission: &bridge.Permission{ToolName: tool, Detail: d.Detail, Options: optionsFromScreen(d.Options)}}
		})
	case info.Question != nil:
		d := info.Question
		s.mu.Lock()
		ask := s.lastAsk
		if ask != nil && !askMatches(ask, d.Question) {
			ask = nil
		}
		if ask == nil && s.askSince.IsZero() {
			s.askSince = time.Now()
		}
		waited := !s.askSince.IsZero() && time.Since(s.askSince) > askWait
		s.mu.Unlock()
		switch {
		case ask != nil:
			current = "q:" + ask.CallID
			s.ensureRequest(kindQuestion, current, func() bridge.Request {
				return bridge.Request{Kind: bridge.RequestQuestion, Title: ask.Questions[0].Text, Questions: ask.Questions}
			})
		case waited:
			current = fmt.Sprintf("qs:%d/%d:%s", d.Index, d.Total, d.Question)
			s.ensureRequest(kindQuestion, current, func() bridge.Request {
				q := bridge.Question{Text: d.Question, AllowText: d.noneIndex() >= 0}
				for i, o := range d.Options {
					if i != d.noneIndex() {
						q.Options = append(q.Options, bridge.QuestionOption{Label: o.Label})
					}
				}
				return bridge.Request{Kind: bridge.RequestQuestion, Title: d.Question, Questions: []bridge.Question{q}}
			})
		default:
			return // give the rollout a moment to provide the structured call
		}
	}
	if info.Question == nil {
		s.mu.Lock()
		s.askSince = time.Time{}
		s.mu.Unlock()
	}

	var gone []string
	s.mu.Lock()
	for _, p := range s.PendingWhere(func(*core.Pending) bool { return true }) {
		sp := p.Data.(*screenPending)
		if sp.answering {
			continue
		}
		if sp.fp == current {
			sp.missed = 0
		} else if sp.missed++; sp.missed >= 3 {
			gone = append(gone, p.Req.ID)
		}
	}
	s.mu.Unlock()
	for _, id := range gone {
		s.Resolve(id, bridge.ResolvedExternally)
	}
}

func askMatches(a *askCall, onScreen string) bool {
	for _, q := range a.Questions {
		if sameQuestion(onScreen, q.Text) {
			return true
		}
	}
	return false
}

func (s *session) ensureRequest(kind dialogKind, fp string, build func() bridge.Request) {
	if len(s.PendingWhere(func(p *core.Pending) bool { return p.Data.(*screenPending).fp == fp })) > 0 {
		return
	}
	if kind == kindTrust && s.co.TrustWorkspace {
		s.mu.Lock()
		busy := s.trustBusy
		s.trustBusy = true
		s.mu.Unlock()
		if !busy {
			go func() {
				req := build()
				opt, err := req.Permission.ResolveOption(bridge.Allow())
				if err == nil {
					s.ctl.Lock()
					err = s.pressOption(context.Background(), opt.ID)
					s.ctl.Unlock()
				}
				if err != nil {
					s.Errorf(fmt.Errorf("codex: answer trust prompt: %w", err))
				}
				time.Sleep(time.Second)
				s.mu.Lock()
				s.trustBusy = false
				s.mu.Unlock()
			}()
		}
		return
	}
	s.AddPending(&core.Pending{Req: build(), Data: &screenPending{kind: kind, fp: fp}}, true)
}

// --- Session API ------------------------------------------------------------------

func (s *session) waitStarted(ctx context.Context) error {
	return s.WaitFor(ctx, 100*time.Millisecond, func() bool { return s.State() != bridge.StateStarting })
}

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
		return errors.New("codex: empty prompt")
	}
	s.ctl.Lock()
	defer s.ctl.Unlock()
	if err := s.waitInputReady(ctx); err != nil {
		return err
	}
	return s.typeAndSubmit(ctx, terminal.BracketedPaste(prompt))
}

func (s *session) typeAndSubmit(ctx context.Context, text string) error {
	t := s.currentTerm()
	if err := core.Press(ctx, t, terminal.KeyCtrlU, text); err != nil {
		return err
	}
	core.Settle(ctx, t, 150*time.Millisecond, 1500*time.Millisecond)
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := t.WriteString(terminal.KeyEnter); err != nil {
			return err
		}
		_, err := core.WaitLines(ctx, t, 3*time.Second, func(l []string) bool {
			s.mu.Lock()
			active := s.turnActive
			s.mu.Unlock()
			info := analyze(l)
			return active || info.Busy || info.dialogVisible()
		})
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return errors.New("codex: the prompt was not accepted")
}

// Command runs a slash command such as "/status".
func (s *session) Command(ctx context.Context, command string) error {
	s.ctl.Lock()
	defer s.ctl.Unlock()
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
	_, err := t.WriteString(terminal.KeyEnter)
	core.Settle(ctx, t, 300*time.Millisecond, 3*time.Second)
	return err
}

// Interrupt presses Esc.
func (s *session) Interrupt(ctx context.Context) error {
	_, err := s.currentTerm().WriteString(terminal.KeyEscape)
	return err
}

// Respond answers a pending request by keyboard.
func (s *session) Respond(ctx context.Context, id string, r bridge.Response) error {
	p := s.FindPending(id)
	if p == nil {
		return bridge.ErrUnknownRequest
	}
	sp := p.Data.(*screenPending)
	var opt bridge.PermissionOption
	if !r.Cancel {
		var err error
		if p.Req.Kind == bridge.RequestQuestion {
			err = bridge.ValidateAnswers(p.Req.Questions, r.Answers)
		} else {
			opt, err = p.Req.Permission.ResolveOption(r)
		}
		if err != nil {
			return err
		}
	}
	s.mu.Lock()
	if sp.answering {
		s.mu.Unlock()
		return errors.New("codex: request is already being answered")
	}
	sp.answering = true
	s.mu.Unlock()
	s.ctl.Lock()
	var err error
	switch {
	case r.Cancel:
		_, err = s.currentTerm().WriteString(terminal.KeyEscape)
	case p.Req.Kind == bridge.RequestQuestion:
		err = s.answerQuestion(ctx, &p.Req, r)
	default:
		err = s.pressOption(ctx, opt.ID)
		if err == nil && opt.Kind == bridge.Deny && r.Message != "" {
			// Codex returns to the composer so the user can say what to do
			// differently; send the message as the follow-up prompt.
			t := s.currentTerm()
			if _, werr := core.WaitLines(ctx, t, 5*time.Second, func(l []string) bool { return analyze(l).InputBox }); werr == nil {
				err = s.typeAndSubmit(ctx, terminal.BracketedPaste(r.Message))
			}
		}
	}
	s.ctl.Unlock()
	s.mu.Lock()
	sp.answering = false
	s.mu.Unlock()
	if err == nil {
		s.Resolve(id, bridge.ResolvedByClient)
	}
	return err
}

// pressOption presses the digit of an "opt:N" option on the visible
// approval or trust dialog.
func (s *session) pressOption(ctx context.Context, optionID string) error {
	n, ok := strings.CutPrefix(optionID, "opt:")
	if !ok {
		return fmt.Errorf("codex: bad option id %q", optionID)
	}
	t := s.currentTerm()
	if _, err := core.WaitLines(ctx, t, 10*time.Second, func(l []string) bool {
		info := analyze(l)
		return info.Approval != nil || info.Trust != nil
	}); err != nil {
		return fmt.Errorf("codex: dialog not on screen: %w", err)
	}
	_, err := t.WriteString(n)
	return err
}

func (s *session) answerQuestion(ctx context.Context, req *bridge.Request, r bridge.Response) error {
	t := s.currentTerm()
	for i, q := range req.Questions {
		lines, err := core.WaitLines(ctx, t, 10*time.Second, func(l []string) bool {
			d := analyze(l).Question
			return d != nil && sameQuestion(d.Question, q.Text)
		})
		if err != nil {
			return fmt.Errorf("codex: question %d not on screen: %w", i+1, err)
		}
		a := r.Answers[i]
		if a.Text != "" {
			err := core.Focus(ctx, t, func(l []string) (int, int, bool) {
				d := analyze(l).Question
				if d == nil {
					return 0, 0, false
				}
				return d.focused(), d.noneIndex(), d.focused() >= 0 && d.noneIndex() >= 0
			})
			if err != nil {
				return err
			}
			if err := core.Press(ctx, t, terminal.KeyTab, strings.Join(strings.Fields(a.Text), " ")); err != nil {
				return err
			}
			core.Settle(ctx, t, 150*time.Millisecond, 1500*time.Millisecond)
			if _, err := t.WriteString(terminal.KeyEnter); err != nil {
				return err
			}
		} else {
			d := analyze(lines).Question
			ord := 0
			for _, o := range d.Options {
				if o.Label == a.Selected[0] {
					ord = o.Ordinal
				}
			}
			if ord == 0 {
				return fmt.Errorf("codex: option %q not on screen", a.Selected[0])
			}
			if _, err := t.WriteString(strconv.Itoa(ord)); err != nil {
				return err
			}
		}
		core.Settle(ctx, t, 150*time.Millisecond, 1500*time.Millisecond)
	}
	return nil
}

// SetMode switches between presets (by relaunching) and Plan mode (with
// Shift+Tab).
func (s *session) SetMode(ctx context.Context, mode string) error {
	switch mode {
	case ModePlan, ModeReadOnly, ModeAuto, ModeFullAccess:
	default:
		return fmt.Errorf("codex: unknown mode %q", mode)
	}
	s.ctl.Lock()
	defer s.ctl.Unlock()
	if err := s.waitInputReady(ctx); err != nil {
		return err
	}
	t := s.currentTerm()
	wantPlan := mode == ModePlan
	for i := 0; i < 4 && analyze(t.Lines()).Plan != wantPlan; i++ {
		v := t.Version()
		if _, err := t.WriteString(terminal.KeyShiftTab); err != nil {
			return err
		}
		_, _ = core.WaitLines(ctx, t, time.Second, func([]string) bool { return t.Version() != v })
		core.Settle(ctx, t, 200*time.Millisecond, 1500*time.Millisecond)
	}
	if analyze(t.Lines()).Plan != wantPlan {
		return fmt.Errorf("codex: could not toggle plan mode")
	}
	if wantPlan {
		s.UpdateSettings(func(st *bridge.Settings) { st.Mode = ModePlan })
		return nil
	}
	s.mu.Lock()
	same := s.launch.preset == mode
	s.mu.Unlock()
	if same {
		s.UpdateSettings(func(st *bridge.Settings) { st.Mode = mode })
		return nil
	}
	return s.restartLocked(ctx, func(l *launch) { l.preset = mode })
}

// SetModel relaunches with -m.
func (s *session) SetModel(ctx context.Context, model string) error {
	if strings.TrimSpace(model) == "" {
		return errors.New("codex: empty model")
	}
	s.ctl.Lock()
	defer s.ctl.Unlock()
	if err := s.waitInputReady(ctx); err != nil {
		return err
	}
	return s.restartLocked(ctx, func(l *launch) { l.model = model })
}

// SetEffort relaunches with model_reasoning_effort.
func (s *session) SetEffort(ctx context.Context, effort string) error {
	if strings.TrimSpace(effort) == "" {
		return errors.New("codex: empty effort")
	}
	s.ctl.Lock()
	defer s.ctl.Unlock()
	if err := s.waitInputReady(ctx); err != nil {
		return err
	}
	return s.restartLocked(ctx, func(l *launch) { l.effort = effort })
}

func (s *session) restartLocked(ctx context.Context, mutate func(*launch)) error {
	s.mu.Lock()
	old := s.term
	l := s.launch
	mutate(&l)
	l.resume = s.Base.ID() // empty before the first prompt: nothing to resume
	s.restarting = true
	s.mu.Unlock()
	s.SetState(bridge.StateStarting)

	_ = core.Press(ctx, old, terminal.KeyCtrlU, "/quit")
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
	if l.resume != "" {
		if path := findRolloutByID(s.p.home(), l.resume); path != "" {
			s.attach(path, true)
		}
	} else {
		s.tailMu.Lock()
		if s.tail != nil {
			s.p.claim(s.tail.path, false)
			s.tail = nil
		}
		s.tailMu.Unlock()
	}
	s.UpdateSettings(func(st *bridge.Settings) {
		if l.model != "" {
			st.Model = l.model
		}
		if l.effort != "" {
			st.Effort = l.effort
		}
		if l.preset != "" {
			st.Mode = l.preset
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
