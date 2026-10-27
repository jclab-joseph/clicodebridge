package claude

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	bridge "github.com/jclab-joseph/clicodebridge"
	"github.com/jclab-joseph/clicodebridge/terminal"
)

// classicProfile reads the default TUI (verified against 2.1.296-2.1.310).
type classicProfile struct{}

func (classicProfile) name() string   { return "classic" }
func (classicProfile) args() []string { return nil }

var (
	effortRe   = regexp.MustCompile(`(\S+)\s+·\s+/effort\s*$`)
	checkboxRe = regexp.MustCompile(`^\[(.)\]\s*(.*)$`)
)

func (p classicProfile) analyze(in screenInput) screenInfo {
	lines := in.scr.Lines
	var s screenInfo
	if m := remoteURLRe.FindString(in.scr.Text()); m != "" {
		s.RemoteURL = m
	}
	s.InputBox = classicInputBox(lines)
	footer := footerBelowLastRule(lines)
	s.Busy = strings.Contains(footer, "esc to interrupt") || titleSpinning(in.title)
	s.Mode = modeFromText(footer)
	for _, l := range lines {
		if m := effortRe.FindStringSubmatch(l); m != nil {
			s.Effort = normalizeEffort(m[1])
		}
	}
	if s.InputBox {
		return s
	}
	// The registry says what kind of dialog is up; without it, structure
	// (a tab bar of checkbox glyphs means a question) decides.
	h := in.hint
	switch {
	case h.registryKnown && h.waitingFor == waitingPermission:
		s.Permission = classicPermission(in)
	case h.registryKnown && h.waitingFor == waitingInput:
		if s.Review = classicReview(in); s.Review == nil {
			s.Question = classicQuestion(in)
		}
	default:
		if s.Review = classicReview(in); s.Review == nil {
			if s.Question = classicQuestion(in); s.Question == nil {
				if s.Permission = classicPermission(in); s.Permission == nil {
					s.Trust = classicTrust(in)
				}
			}
		}
	}
	if !s.dialogVisible() && lastOptionBlock(lines, 0) != nil {
		s.Other = true
	}
	return s
}

// classicInputBox finds the prompt editor: a rule, a row starting with the
// pointer, and a closing rule below it.
func classicInputBox(lines []string) bool {
	for i := 0; i+2 < len(lines); i++ {
		if !ruleRe.MatchString(lines[i]) || !pointerRe.MatchString(lines[i+1]) || numberedRe.MatchString(lines[i+1]) {
			continue
		}
		for j := i + 2; j < len(lines); j++ {
			if ruleRe.MatchString(lines[j]) {
				return true
			}
		}
	}
	return false
}

func lastRule(lines []string) int {
	last := -1
	for i, l := range lines {
		if ruleRe.MatchString(l) {
			last = i
		}
	}
	return last
}

func footerBelowLastRule(lines []string) string {
	r := lastRule(lines)
	if r < 0 {
		return ""
	}
	return strings.Join(lines[r+1:], "\n")
}

// optionBlock reads consecutive "N. label" rows starting at from, folding
// deeper-indented wrapped rows into the label.
func optionBlock(lines []string, from int) []screenOption {
	var out []screenOption
	for i := from; i < len(lines); i++ {
		l := lines[i]
		if ruleRe.MatchString(l) {
			break
		}
		if m := numberedRe.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[3])
			out = append(out, screenOption{Ordinal: n, Label: strings.TrimSpace(m[4]), Focused: m[2] != "",
				line: i, col: len([]rune(m[1] + m[2] + m[3] + ". "))})
			continue
		}
		t := strings.TrimSpace(l)
		if t == "" || len(out) == 0 {
			continue
		}
		if indent(l) > indent(lines[out[len(out)-1].line]) {
			out[len(out)-1].Label += " " + t
			continue
		}
		break
	}
	return out
}

func indent(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }

// lastOptionBlock returns the numbered options of the dialog at the bottom
// of the screen (below the last rule, or the whole screen), 1..n in order.
func lastOptionBlock(lines []string, from int) []screenOption {
	start := -1
	for i := len(lines) - 1; i >= from; i-- {
		if m := numberedRe.FindStringSubmatch(lines[i]); m != nil && m[3] == "1" {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	opts := optionBlock(lines, start)
	if len(opts) < 2 {
		return nil
	}
	for i, o := range opts {
		if o.Ordinal != i+1 {
			return nil
		}
	}
	return opts
}

// applyStyleFocus marks the focused option by color when no pointer is drawn.
func applyStyleFocus(scr *terminal.Screen, focused func(i int) bool, set func(i int), n int, pos func(i int) (int, int)) {
	for i := 0; i < n; i++ {
		if focused(i) {
			return
		}
	}
	styles := make([]terminal.Style, n)
	for i := 0; i < n; i++ {
		y, x := pos(i)
		styles[i] = scr.StyleAt(y, x)
	}
	if i := styleFocus(styles); i >= 0 {
		set(i)
	}
}

func classicPermission(in screenInput) *permissionDialog {
	lines := in.scr.Lines
	top := lastRule(lines)
	if top < 0 {
		top = 0
	}
	opts := lastOptionBlock(lines, top)
	if opts == nil {
		return nil
	}
	if !in.hint.registryKnown {
		// Without the registry, require the dialog's question line.
		ok := false
		for i := top; i < opts[0].line; i++ {
			if strings.HasPrefix(strings.TrimSpace(lines[i]), "Do you want to ") {
				ok = true
			}
		}
		if !ok {
			return nil
		}
	}
	applyStyleFocus(in.scr, func(i int) bool { return opts[i].Focused }, func(i int) { opts[i].Focused = true }, len(opts),
		func(i int) (int, int) { return opts[i].line, opts[i].col })
	d := &permissionDialog{Options: opts}
	for j := top + 1; j < opts[0].line; j++ {
		if t := strings.TrimSpace(lines[j]); t != "" {
			d.Title = t
			break
		}
	}
	return d
}

func isTabBar(l string) bool { return strings.ContainsAny(l, "☐☒") }

// classicQuestion parses the AskUserQuestion dialog, anchored on the tab bar
// of checkbox glyphs or, failing that, on the question text from the hook.
func classicQuestion(in screenInput) *questionDialog {
	lines := in.scr.Lines
	bar := -1
	for i, l := range lines {
		if isTabBar(l) {
			bar = i
		}
	}
	qline := -1
	if bar >= 0 {
		for i := bar + 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) != "" {
				qline = i
				break
			}
		}
	} else {
		for i := len(lines) - 1; i >= 0 && qline < 0; i-- {
			if _, ok := hintQuestion(in.hint, strings.TrimSpace(lines[i])); ok {
				qline = i
			}
		}
	}
	if qline < 0 {
		return nil
	}
	d := &questionDialog{Question: strings.TrimSpace(lines[qline])}
	hq, hinted := hintQuestion(in.hint, d.Question)
	type plainRow struct {
		line int
		text string
	}
	var plain []plainRow
	var chat []questionRow
	extra := false
	for i := qline + 1; i < len(lines); i++ {
		l := lines[i]
		if ruleRe.MatchString(l) {
			extra = true
			continue
		}
		m := numberedRe.FindStringSubmatch(l)
		if m == nil {
			if t := strings.TrimSpace(strings.TrimLeft(l, " ❯›>▸▶")); t != "" && !extra {
				plain = append(plain, plainRow{i, t})
			}
			continue
		}
		n, _ := strconv.Atoi(m[3])
		row := questionRow{Kind: rowOption, Ordinal: n, Label: strings.TrimSpace(m[4]), Focused: m[2] != "",
			line: i, col: len([]rune(m[1] + m[2] + m[3] + ". "))}
		if cm := checkboxRe.FindStringSubmatch(row.Label); cm != nil {
			d.Multi = true
			row.Checked = strings.TrimSpace(cm[1]) != ""
			row.Label = cm[2]
		}
		if extra {
			row.Kind = rowChat
			chat = append(chat, row)
		} else {
			d.Rows = append(d.Rows, row)
		}
	}
	if len(d.Rows) < 2 {
		return nil
	}
	if hinted {
		d.Multi = d.Multi || hq.MultiSelect
	}
	// The free-text row is the last numbered row above the separator; with
	// the hook's options it is exactly option count + 1.
	last := d.Rows[len(d.Rows)-1]
	textOrd := last.Ordinal
	if hinted {
		textOrd = len(hq.Options) + 1
	}
	if i := d.find(rowOption, textOrd); i >= 0 {
		d.Rows[i].Kind = rowText
	}
	// A multi-select's Submit is the first unnumbered row below the last
	// numbered one (descriptions sit between options, never after it).
	if d.Multi {
		for _, pr := range plain {
			if pr.line > last.line && !(hinted && isDescription(hq, pr.text)) {
				l := lines[pr.line]
				d.Rows = append(d.Rows, questionRow{Kind: rowSubmit, Label: pr.text, Focused: pointerRe.MatchString(l),
					line: pr.line, col: len([]rune(l)) - len([]rune(strings.TrimLeft(l, " ❯›>▸▶")))})
				break
			}
		}
	}
	n := len(d.Rows)
	d.Rows = append(d.Rows, chat...)
	applyStyleFocus(in.scr, func(i int) bool { return d.Rows[i].Focused }, func(i int) { d.Rows[i].Focused = true },
		n, func(i int) (int, int) { return d.Rows[i].line, d.Rows[i].col })
	return d
}

func isDescription(q bridge.Question, t string) bool {
	for _, o := range q.Options {
		if o.Description != "" && normalizeSpace(o.Description) == normalizeSpace(t) {
			return true
		}
	}
	return false
}

// classicReview recognizes the final review tab of a multi-step question:
// the tab bar is up, the first row under it is no longer a question, the
// questions appear in a summary, and a short numbered list follows.
func classicReview(in screenInput) *reviewDialog {
	lines := in.scr.Lines
	bar := -1
	for i, l := range lines {
		if isTabBar(l) {
			bar = i
		}
	}
	if bar < 0 {
		return nil
	}
	first := ""
	for i := bar + 1; i < len(lines); i++ {
		if t := strings.TrimSpace(lines[i]); t != "" {
			first = t
			break
		}
	}
	if len(in.hint.questions) > 0 {
		if _, ok := hintQuestion(in.hint, first); ok {
			return nil
		}
		summarized := false
		for _, l := range lines[bar+1:] {
			for _, q := range in.hint.questions {
				if strings.Contains(normalizeSpace(l), normalizeSpace(q.Text)) {
					summarized = true
				}
			}
		}
		if !summarized {
			return nil
		}
	} else if !strings.Contains(in.scr.Text(), "Ready to submit") {
		return nil
	}
	opts := lastOptionBlock(lines, bar+1)
	if opts == nil {
		return nil
	}
	return &reviewDialog{Submit: strconv.Itoa(opts[0].Ordinal)}
}

// classicTrust recognizes the workspace trust selector: unnumbered rows,
// one carrying the pointer, before the prompt has ever been shown.
func classicTrust(in screenInput) *trustDialog {
	lines := in.scr.Lines
	focus := -1
	for i, l := range lines {
		if pointerRe.MatchString(l) && !numberedRe.MatchString(l) {
			focus = i
		}
	}
	if focus < 0 {
		return nil
	}
	ind := indent(strings.Replace(lines[focus], "❯", " ", 1))
	start, end := focus, focus
	for start > 0 && rowAt(lines[start-1], ind) {
		start--
	}
	for end+1 < len(lines) && rowAt(lines[end+1], ind) {
		end++
	}
	d := &trustDialog{Focused: -1, Yes: -1}
	for i := start; i <= end; i++ {
		row := strings.TrimSpace(strings.Replace(lines[i], "❯", "", 1))
		if i == focus {
			d.Focused = len(d.Rows)
		}
		if strings.Contains(strings.ToLower(row), "trust") {
			d.Yes = len(d.Rows)
		}
		d.Rows = append(d.Rows, row)
	}
	if len(d.Rows) != 2 || d.Yes < 0 {
		return nil
	}
	if !in.hint.expectTrust && in.hint.registryKnown {
		return nil
	}
	return d
}

func rowAt(l string, ind int) bool {
	t := strings.TrimSpace(l)
	return t != "" && indent(l) == ind && !numberedRe.MatchString(l)
}

// --- answering --------------------------------------------------------------------

func (classicProfile) answerTrust(ctx context.Context, io *screenIO, trust bool) error {
	err := io.focus(ctx, func(s screenInfo) (int, int, bool) {
		d := s.Trust
		if d == nil || d.Focused < 0 {
			return 0, 0, false
		}
		want := d.Yes
		if !trust {
			want = 1 - d.Yes
		}
		return d.Focused, want, true
	})
	if err != nil {
		return err
	}
	return io.press(terminal.KeyEnter)
}

func (classicProfile) answerPermission(ctx context.Context, io *screenIO, ordinal int) error {
	return io.press(strconv.Itoa(ordinal))
}

func (classicProfile) confirm(ctx context.Context, io *screenIO) error { return io.press("1") }

func (classicProfile) answerQuestion(ctx context.Context, io *screenIO, req *bridge.Request, r bridge.Response) error {
	focus := func(kind questionRowKind) error {
		return io.focus(ctx, func(s screenInfo) (int, int, bool) {
			d := s.Question
			if d == nil {
				return 0, 0, false
			}
			return d.focused(), d.find(kind, 0), d.focused() >= 0 && d.find(kind, 0) >= 0
		})
	}
	typeText := func(text string) error {
		if err := io.press(strings.Join(strings.Fields(text), " ")); err != nil {
			return err
		}
		io.settle(ctx, 150*time.Millisecond, 1500*time.Millisecond)
		return nil
	}
	for i, q := range req.Questions {
		info, err := io.wait(ctx, 10*time.Second, "question "+strconv.Itoa(i+1)+" on screen", func(s screenInfo) bool {
			return s.Question != nil && sameQuestion(s.Question.Question, q.Text)
		})
		if err != nil {
			return err
		}
		if r.Cancel {
			return io.press(terminal.KeyEscape)
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
				if err := io.press(terminal.KeyEnter); err != nil {
					return err
				}
			} else if err := io.press(strconv.Itoa(q.OptionIndex(a.Selected[0]) + 1)); err != nil {
				return err
			}
		} else {
			d := info.Question
			for _, label := range a.Selected {
				idx := q.OptionIndex(label)
				if row := d.find(rowOption, idx+1); row >= 0 && d.Rows[row].Checked {
					continue // already checked; pressing would uncheck it
				}
				if err := io.press(strconv.Itoa(idx + 1)); err != nil {
					return err
				}
				io.settle(ctx, 100*time.Millisecond, time.Second)
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
			if err := io.press(terminal.KeyEnter); err != nil {
				return err
			}
		}
		io.settle(ctx, 150*time.Millisecond, 1500*time.Millisecond)
	}
	// Multi-question and multi-select dialogs end on a review tab.
	info, _ := io.wait(ctx, 2*time.Second, "review", func(s screenInfo) bool {
		return s.Review != nil || (s.Question == nil && !s.dialogVisible())
	})
	if info.Review != nil {
		return io.press(info.Review.Submit)
	}
	return nil
}
