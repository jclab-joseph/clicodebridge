package claude

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	bridge "github.com/jclab-joseph/clicodebridge"
	"github.com/jclab-joseph/clicodebridge/terminal"
)

func (s *session) waitStarted(ctx context.Context) error {
	return s.WaitFor(ctx, 100*time.Millisecond, func() bool { return s.State() != bridge.StateStarting })
}

// waitInputReady waits for StateIdle. When no turn is running and nothing is
// pending, the state may still lag a just-finished turn, so it waits a
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

// Send submits a prompt: through the bridge mod when it is connected (no
// keystrokes), else by pasting into the prompt box.
func (s *session) Send(ctx context.Context, prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return errors.New("claude: empty prompt")
	}
	s.ctl.Lock()
	defer s.ctl.Unlock()
	if err := s.waitInputReady(ctx); err != nil {
		return err
	}
	seen := make(chan struct{})
	s.mu.Lock()
	s.promptSeen = seen
	s.mu.Unlock()
	if s.mod.isReady() {
		// The ack arrives once the prompt entered the session.
		return s.mod.call(ctx, "submit", prompt)
	}
	err := s.typePrompt(ctx, seen, prompt)
	s.reportActionDrift("send", err)
	return err
}

func (s *session) typePrompt(ctx context.Context, seen chan struct{}, prompt string) error {
	io := s.io()
	if err := io.press(terminal.KeyCtrlU); err != nil {
		return err
	}
	if err := io.press(terminal.BracketedPaste(prompt)); err != nil {
		return err
	}
	io.settle(ctx, 150*time.Millisecond, 1500*time.Millisecond)
	// Claude sometimes drops an Enter that arrives while it is still
	// processing pasted input; UserPromptSubmit confirms acceptance.
	for attempt := 0; attempt < 3; attempt++ {
		if err := io.press(terminal.KeyEnter); err != nil {
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
		if s.State() == bridge.StateBusy {
			return nil
		}
	}
	return fmt.Errorf("claude: the prompt was not accepted: %w", bridge.ErrScreenTimeout)
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
	io := s.io()
	if err := io.press(terminal.KeyCtrlU); err != nil {
		return err
	}
	if err := io.keys(ctx, command); err != nil {
		return err
	}
	io.settle(ctx, 200*time.Millisecond, 1500*time.Millisecond)
	if err := io.press(terminal.KeyEnter); err != nil {
		return err
	}
	io.settle(ctx, 300*time.Millisecond, 3*time.Second)
	return nil
}

// Interrupt stops the running turn: through the bridge mod when it is
// connected, else with Esc (which also cancels an open dialog).
func (s *session) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	active := s.turnActive
	s.mu.Unlock()
	if active && s.mod.isReady() && !s.HasPending() {
		if err := s.mod.call(ctx, "abort", ""); err == nil {
			return nil
		}
	}
	return s.io().press(terminal.KeyEscape)
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
		err := s.prof.answerTrust(ctx, s.io(), !r.Cancel && opt.Kind != bridge.Deny)
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
			err = s.prof.answerQuestion(ctx, s.io(), &p.Req, r)
		} else {
			err = s.answerPermission(ctx, &p.Req, r)
		}
		s.ctl.Unlock()
		s.mu.Lock()
		d.answering = false
		s.mu.Unlock()
		s.reportActionDrift("respond", err)
		if err == nil {
			s.Resolve(id, bridge.ResolvedByClient)
		}
		return err
	}
	return bridge.ErrUnknownRequest
}

func (s *session) answerPermission(ctx context.Context, req *bridge.Request, r bridge.Response) error {
	io := s.io()
	info, err := io.wait(ctx, 10*time.Second, "permission dialog on screen", func(si screenInfo) bool { return si.Permission != nil })
	if err != nil {
		return err
	}
	if r.Cancel {
		return io.press(terminal.KeyEscape)
	}
	opt, _ := req.Permission.ResolveOption(r)
	ord := screenOrdinal(info.Permission, opt)
	if ord == 0 {
		return fmt.Errorf("claude: no on-screen option matches %q", opt.Label)
	}
	return s.prof.answerPermission(ctx, io, ord)
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
	io := s.io()
	if inCycle(m) {
		for i := 0; i < len(shiftTabCycle)+2; i++ {
			cur := io.info().Mode
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
			v := io.t.Version()
			if err := io.press(terminal.KeyShiftTab); err != nil {
				return err
			}
			_, _ = io.wait(ctx, time.Second, "redraw", func(screenInfo) bool { return io.t.Version() != v })
			io.settle(ctx, 150*time.Millisecond, time.Second)
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
		if err := s.waitInputReady(ctx); err != nil {
			return err
		}
		return s.restartLocked(ctx, func(l *launch) { l.model = model })
	}
	if err := s.commandLocked(ctx, "/model "+model); err != nil {
		return err
	}
	io := s.io()
	// The switch asks for confirmation when the conversation is cached.
	if info, err := io.wait(ctx, 2*time.Second, "model confirmation", func(si screenInfo) bool { return si.Other || si.Permission != nil }); err == nil && (info.Other || info.Permission != nil) {
		if err := s.prof.confirm(ctx, io); err != nil {
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
		if err := s.waitInputReady(ctx); err != nil {
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
	s.mod.reset()
	s.SetState(bridge.StateStarting)

	io := &screenIO{t: old, prof: s.prof, hint: s.hint}
	_ = io.press(terminal.KeyCtrlU)
	_ = io.keys(ctx, "/exit")
	io.settle(ctx, 150*time.Millisecond, time.Second)
	_ = io.press(terminal.KeyEnter)
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
