package claude

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	bridge "github.com/jclab-joseph/clicodebridge"
	"github.com/jclab-joseph/clicodebridge/terminal"
)

// A profile is one way of rendering the TUI: the classic layout, or the
// flat text of --ax-screen-reader. Each turns a screen into a screenInfo and
// knows the keys that answer its dialogs.
//
// Recognizers are structure-first: dialog presence and kind come from the
// session registry (waitingFor), question and option texts from the hook
// payload, and the screen is read for layout (numbered rows, checkboxes,
// which row is focused) rather than for fixed English sentences. Wording is
// only a fallback when no structured hint is available.
type profile interface {
	name() string
	args() []string
	analyze(in screenInput) screenInfo
	answerTrust(ctx context.Context, io *screenIO, trust bool) error
	answerPermission(ctx context.Context, io *screenIO, ordinal int) error
	answerQuestion(ctx context.Context, io *screenIO, req *bridge.Request, r bridge.Response) error
	// confirm accepts the dialog a command opened (the /model switch prompt).
	confirm(ctx context.Context, io *screenIO) error
}

// screenInput is what a recognizer reads.
type screenInput struct {
	scr   *terminal.Screen
	title string
	hint  screenHint
}

// screenHint carries what structured channels already know.
type screenHint struct {
	waitingFor    string            // registry; "" when unknown
	questions     []bridge.Question // the pending AskUserQuestion, if any
	expectTrust   bool              // the trust dialog may be showing
	registryKnown bool              // waitingFor is authoritative
}

// screenInfo is everything recognized on one screen.
type screenInfo struct {
	InputBox   bool // the prompt can be typed into
	Busy       bool // a turn is running
	Mode       string
	Effort     string
	RemoteURL  string
	Trust      *trustDialog
	Permission *permissionDialog
	Question   *questionDialog
	Review     *reviewDialog
	TextEntry  int  // screen reader: ordinal whose free text is being typed
	Other      bool // a dialog of no known kind (e.g. the /model confirmation)
}

func (s screenInfo) dialogVisible() bool {
	return s.Trust != nil || s.Permission != nil || s.Question != nil || s.Review != nil || s.TextEntry > 0 || s.Other
}

func (s screenInfo) idle() bool { return s.InputBox && !s.Busy && !s.dialogVisible() }

type trustDialog struct {
	Rows    []string
	Focused int // index into Rows, -1 if unknown
	Yes     int // index of the "trust" row
}

type screenOption struct {
	Ordinal int
	Label   string
	Focused bool
	line    int // screen row of the option
	col     int // rune index of the label on that row
}

type permissionDialog struct {
	Title   string
	Options []screenOption
}

type questionRowKind int

const (
	rowOption questionRowKind = iota
	rowText                   // free-text answer ("Type something" / "Other")
	rowSubmit                 // multi-select "Submit"
	rowChat                   // "Chat about this"
)

type questionRow struct {
	Kind    questionRowKind
	Ordinal int // 0 for an unnumbered row
	Label   string
	Checked bool
	Focused bool
	line    int
	col     int
}

type questionDialog struct {
	Question string
	Multi    bool
	Rows     []questionRow
}

func (d *questionDialog) focused() int {
	for i, r := range d.Rows {
		if r.Focused {
			return i
		}
	}
	return -1
}

func (d *questionDialog) find(kind questionRowKind, ordinal int) int {
	for i, r := range d.Rows {
		if r.Kind == kind && (ordinal == 0 || r.Ordinal == ordinal) {
			return i
		}
	}
	return -1
}

type reviewDialog struct {
	Submit string // keys that submit the answers
}

// permissionKinds classifies on-screen options by position: Claude puts the
// plain "yes" first and the refusal last. Middle options are standing
// approvals, except one that switches mode.
func permissionKinds(opts []screenOption) []bridge.PermissionKind {
	kinds := make([]bridge.PermissionKind, len(opts))
	for i, o := range opts {
		switch {
		case i == 0:
			kinds[i] = bridge.AllowOnce
		case i == len(opts)-1:
			kinds[i] = bridge.Deny
		case modeSwitchRe.MatchString(o.Label):
			kinds[i] = bridge.OtherOption
		default:
			kinds[i] = bridge.AllowAlways
		}
	}
	return kinds
}

var modeSwitchRe = regexp.MustCompile(`(?i)\bmode\b`)

// screenOrdinal maps a request option to the on-screen option to press.
func screenOrdinal(d *permissionDialog, opt bridge.PermissionOption) int {
	kinds := permissionKinds(d.Options)
	if opt.Kind == bridge.OtherOption {
		// "Allow and switch to auto mode" -> the option naming that mode.
		words := strings.Fields(strings.ToLower(opt.Label))
		if len(words) >= 2 {
			mode := words[len(words)-2]
			for i, o := range d.Options {
				if kinds[i] == bridge.OtherOption && strings.Contains(strings.ToLower(o.Label), mode) {
					return o.Ordinal
				}
			}
		}
		for i, o := range d.Options {
			if kinds[i] == bridge.OtherOption {
				return o.Ordinal
			}
		}
		return d.Options[0].Ordinal
	}
	for i, o := range d.Options {
		if kinds[i] == opt.Kind {
			return o.Ordinal
		}
	}
	return 0
}

// --- shared recognizer helpers --------------------------------------------------

var (
	ruleRe      = regexp.MustCompile(`^\s*─{20,}\s*$`)
	numberedRe  = regexp.MustCompile(`^(\s*)([❯›>▸▶]\s*)?(\d+)\.\s+(.*)$`)
	pointerRe   = regexp.MustCompile(`^\s*[❯›>▸▶]`)
	remoteURLRe = regexp.MustCompile(`https://claude\.ai/code/session_[A-Za-z0-9]+`)
)

func normalizeSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// sameQuestion matches an on-screen question (possibly wrapped, so only its
// first row) against the full question text.
func sameQuestion(onScreen, text string) bool {
	a, b := normalizeSpace(onScreen), normalizeSpace(text)
	return a != "" && strings.HasPrefix(b, a)
}

func hintQuestion(h screenHint, onScreen string) (bridge.Question, bool) {
	for _, q := range h.questions {
		if sameQuestion(onScreen, q.Text) {
			return q, true
		}
	}
	return bridge.Question{}, false
}

func titleSpinning(title string) bool {
	return strings.HasPrefix(title, "◐") || strings.HasPrefix(title, "◑")
}

func modeFromText(footer string) string {
	switch {
	case strings.Contains(footer, "manual mode on"):
		return ModeManual
	case strings.Contains(footer, "accept edits on"):
		return ModeAcceptEdits
	case strings.Contains(footer, "plan mode on"):
		return ModePlan
	case strings.Contains(footer, "auto mode on"):
		return ModeAuto
	case strings.Contains(footer, "bypass permissions on"):
		return ModeBypass
	case strings.Contains(footer, "don't ask"), strings.Contains(footer, "dont ask"):
		return ModeDontAsk
	}
	return ""
}

// styleKey is what distinguishes a focused row's label from its siblings.
func styleKey(st terminal.Style) string {
	return fmt.Sprintf("%s/%s/%v/%v/%v", st.Fg, st.Bg, st.Bold, st.Reverse, st.Faint)
}

// isAccent reports a style that highlights: a non-gray color, reverse
// video or bold. Gray marks muted text such as a placeholder.
func isAccent(st terminal.Style) bool {
	if st.Reverse || st.Bold || st.Bg != "" {
		return true
	}
	if len(st.Fg) != 7 {
		return false
	}
	return !(st.Fg[1:3] == st.Fg[3:5] && st.Fg[3:5] == st.Fg[5:7])
}

// styleFocus returns the row drawn unlike all others, or -1. It finds the
// focused row by color when no pointer glyph is drawn. When several rows
// are unique (a dim placeholder is unique too), the accented one wins.
func styleFocus(styles []terminal.Style) int {
	if len(styles) < 3 {
		return -1
	}
	count := map[string]int{}
	for _, st := range styles {
		count[styleKey(st)]++
	}
	var unique []int
	for i, st := range styles {
		if count[styleKey(st)] == 1 {
			unique = append(unique, i)
		}
	}
	if len(unique) == 1 {
		return unique[0]
	}
	accent := -1
	for _, i := range unique {
		if isAccent(styles[i]) {
			if accent >= 0 {
				return -1
			}
			accent = i
		}
	}
	return accent
}

// --- driving the screen ---------------------------------------------------------

// screenIO reads and types into the session's current terminal.
type screenIO struct {
	t    *terminal.Terminal
	prof profile
	hint func() screenHint
}

func (io *screenIO) info() screenInfo {
	return io.prof.analyze(screenInput{scr: io.t.Screen(), title: io.t.Title(), hint: io.hint()})
}

// wait polls until pred holds.
func (io *screenIO) wait(ctx context.Context, timeout time.Duration, what string, pred func(screenInfo) bool) (screenInfo, error) {
	deadline := time.Now().Add(timeout)
	for {
		info := io.info()
		if pred(info) {
			return info, nil
		}
		if time.Now().After(deadline) {
			return info, fmt.Errorf("claude: %s: %w", what, bridge.ErrScreenTimeout)
		}
		select {
		case <-ctx.Done():
			return info, ctx.Err()
		case <-time.After(40 * time.Millisecond):
		}
	}
}

// settle waits until the screen stops changing.
func (io *screenIO) settle(ctx context.Context, quiet, max time.Duration) {
	deadline := time.Now().Add(max)
	last, lastChange := io.t.Version(), time.Now()
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(25 * time.Millisecond):
		}
		if v := io.t.Version(); v != last {
			last, lastChange = v, time.Now()
		} else if time.Since(lastChange) >= quiet {
			return
		}
	}
}

// keys types s one rune at a time: some prompts (the screen reader's text
// entry) drop input that arrives as a single burst.
func (io *screenIO) keys(ctx context.Context, s string) error {
	for _, r := range s {
		if _, err := io.t.WriteString(string(r)); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(40 * time.Millisecond):
		}
	}
	return nil
}

func (io *screenIO) press(key string) error {
	_, err := io.t.WriteString(key)
	return err
}

// focus moves the selection with Up/Down until locate reports the wanted
// row focused, re-reading the screen after every key.
func (io *screenIO) focus(ctx context.Context, locate func(screenInfo) (cur, target int, ok bool)) error {
	for step := 0; step < 40; step++ {
		info, err := io.wait(ctx, 2*time.Second, "locate row", func(s screenInfo) bool { _, _, ok := locate(s); return ok })
		if err != nil {
			return err
		}
		cur, target, _ := locate(info)
		if cur == target {
			return nil
		}
		key := terminal.KeyDown
		if target < cur {
			key = terminal.KeyUp
		}
		v := io.t.Version()
		if err := io.press(key); err != nil {
			return err
		}
		_, _ = io.wait(ctx, 600*time.Millisecond, "redraw", func(screenInfo) bool { return io.t.Version() != v })
		io.settle(ctx, 60*time.Millisecond, 400*time.Millisecond)
	}
	return fmt.Errorf("claude: focus row: %w", bridge.ErrScreenTimeout)
}
