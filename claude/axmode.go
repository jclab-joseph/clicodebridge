package claude

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	bridge "github.com/jclab-joseph/clicodebridge"
	"github.com/jclab-joseph/clicodebridge/terminal"
)

// screenReaderProfile reads the flat output of --ax-screen-reader (verified
// against 2.1.310). That mode is built for assistive technology: no borders
// or animation, labelled lines, and every dialog ends with an instruction
// line that states how to answer it:
//
//	Select with numbers [1-4]. Then Enter to submit or Escape to cancel:
//	Select with numbers [1-4] (comma- or space-separated for several). …
//	Enter text for option 3 (Other), or Escape for the list:
//	Enter y/n:
//
// Recognition keys on that grammar ([1-N], "several", "option K", y/n),
// so it does not depend on the layout of the classic TUI. Answers are typed
// and confirmed with Enter, one character at a time (bursts are dropped).
type screenReaderProfile struct{}

func (screenReaderProfile) name() string   { return "screen-reader" }
func (screenReaderProfile) args() []string { return []string{"--ax-screen-reader"} }

var (
	axRangeRe   = regexp.MustCompile(`\[1-(\d+)\]`)
	axTextRe    = regexp.MustCompile(`(?i)option (\d+)\b.*:\s*$`)
	axYesNoRe   = regexp.MustCompile(`(?i)\by/n\b`)
	axEffortRe  = regexp.MustCompile(`effort:\s*(\S+)`)
	axYesNoRow  = regexp.MustCompile(`^([yn])\.\s+(.*)$`)
	axDescSplit = " — "
)

// axPromptLine recognizes the prompt row: "$", then any space (after an
// interrupt the restored text follows a no-break space).
func axPromptLine(l string) bool {
	r := []rune(strings.TrimSpace(l))
	return len(r) > 0 && r[0] == '$' && (len(r) == 1 || unicode.IsSpace(r[1]))
}

type axInstruction struct {
	line  int
	max   int  // [1-max]
	multi bool // several answers accepted
	text  int  // free-text entry for this ordinal
	yesNo bool
}

func axInstructionOf(lines []string) (axInstruction, bool) {
	// The instruction is among the last few rows (a hint row may follow).
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-4; i-- {
		l := strings.TrimSpace(lines[i])
		switch {
		case axTextRe.MatchString(l) && strings.Contains(strings.ToLower(l), "text"):
			n, _ := strconv.Atoi(axTextRe.FindStringSubmatch(l)[1])
			return axInstruction{line: i, text: n}, true
		case axRangeRe.MatchString(l):
			n, _ := strconv.Atoi(axRangeRe.FindStringSubmatch(l)[1])
			return axInstruction{line: i, max: n, multi: strings.Contains(l, "several")}, true
		case axYesNoRe.MatchString(l):
			return axInstruction{line: i, yesNo: true}, true
		}
	}
	return axInstruction{}, false
}

func (screenReaderProfile) analyze(in screenInput) screenInfo {
	lines := in.scr.Lines
	var s screenInfo
	if m := remoteURLRe.FindString(in.scr.Text()); m != "" {
		s.RemoteURL = m
	}
	tail := lines
	if len(tail) > 8 {
		tail = tail[len(tail)-8:]
	}
	tailText := strings.Join(tail, "\n")
	s.Busy = strings.Contains(tailText, "esc to interrupt")
	s.Mode = modeFromText(tailText)
	if m := axEffortRe.FindStringSubmatch(tailText); m != nil {
		s.Effort = normalizeEffort(m[1])
	}
	if n := len(lines); n > 0 {
		s.InputBox = axPromptLine(lines[n-1])
	}
	ins, ok := axInstructionOf(lines)
	if !ok || s.InputBox {
		return s
	}
	bar := -1
	for i := 0; i < ins.line; i++ {
		if isTabBar(lines[i]) {
			bar = i
		}
	}
	h := in.hint
	switch {
	case ins.text > 0:
		s.TextEntry = ins.text
		s.Question = axQuestion(lines, bar, ins, h)
	case ins.yesNo:
		rows := axYesNoRows(lines, ins.line)
		switch {
		case bar >= 0:
			s.Review = &reviewDialog{Submit: "y"}
		case trustRow(rows) >= 0 && (h.expectTrust || !h.registryKnown):
			s.Trust = &trustDialog{Rows: rows, Focused: -1, Yes: trustRow(rows)}
		default:
			s.Other = true
		}
	default:
		question := bar >= 0 || (h.registryKnown && h.waitingFor == waitingInput)
		if question {
			s.Question = axQuestion(lines, bar, ins, h)
		}
		if s.Question == nil {
			opts := axOptions(lines, ins)
			switch {
			case len(opts) < 2:
				s.Other = true
			case h.registryKnown && h.waitingFor != waitingPermission:
				s.Other = true
			default:
				s.Permission = &permissionDialog{Title: axTitle(lines, opts[0].line), Options: opts}
			}
		}
	}
	return s
}

// axOptions reads the numbered rows above the instruction, folding wrapped
// rows into the option above them.
func axOptions(lines []string, ins axInstruction) []screenOption {
	start := -1
	for i := ins.line - 1; i >= 0; i-- {
		if m := numberedRe.FindStringSubmatch(lines[i]); m != nil && m[3] == "1" {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	var out []screenOption
	for i := start; i < ins.line; i++ {
		l := lines[i]
		if m := numberedRe.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[3])
			out = append(out, screenOption{Ordinal: n, Label: strings.TrimSpace(m[4]), line: i})
		} else if t := strings.TrimSpace(l); t != "" && len(out) > 0 {
			out[len(out)-1].Label += " " + t
		}
	}
	for i := range out {
		if j := strings.Index(out[i].Label, axDescSplit); j > 0 {
			out[i].Label = strings.TrimSpace(out[i].Label[:j])
		}
	}
	return out
}

// axTitle is the dialog's heading: the "Permission Required: …" label, else
// the first row of the block above the options.
func axTitle(lines []string, firstOption int) string {
	for i := firstOption - 1; i >= 0 && i >= firstOption-8; i-- {
		t := strings.TrimSpace(lines[i])
		if k := strings.Index(t, ":"); k > 0 && strings.HasPrefix(strings.ToLower(t), "permission required") {
			return strings.TrimSpace(t[k+1:])
		}
	}
	for i := firstOption - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

func axQuestion(lines []string, bar int, ins axInstruction, h screenHint) *questionDialog {
	qline := -1
	start := bar + 1
	if bar < 0 {
		start = 0
	}
	for i := start; i < ins.line; i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, ">") {
			qline = i
			break
		}
		if _, ok := hintQuestion(h, t); ok {
			qline = i
			break
		}
	}
	if qline < 0 {
		return nil
	}
	d := &questionDialog{Question: strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[qline]), ">")), Multi: ins.multi}
	opts := axOptions(lines, axInstruction{line: ins.line})
	hq, hinted := hintQuestion(h, d.Question)
	textOrd, chatOrd := 0, 0
	if hinted {
		textOrd = len(hq.Options) + 1
		if len(opts) > textOrd {
			chatOrd = textOrd + 1
		}
		d.Multi = d.Multi || hq.MultiSelect
	} else if len(opts) >= 2 {
		// Without the hook: in a single select the list ends with the free-
		// text row then the chat row; a multi-select has no chat row.
		textOrd = len(opts)
		if !d.Multi {
			textOrd, chatOrd = len(opts)-1, len(opts)
		}
	}
	for _, o := range opts {
		kind := rowOption
		switch o.Ordinal {
		case textOrd:
			kind = rowText
		case chatOrd:
			kind = rowChat
		}
		d.Rows = append(d.Rows, questionRow{Kind: kind, Ordinal: o.Ordinal, Label: o.Label, line: o.line})
	}
	if len(d.Rows) < 2 {
		return nil
	}
	return d
}

func axYesNoRows(lines []string, instr int) []string {
	var rows []string
	for i := 0; i < instr; i++ {
		if m := axYesNoRow.FindStringSubmatch(strings.TrimSpace(lines[i])); m != nil {
			rows = append(rows, m[1]+". "+m[2])
		}
	}
	return rows
}

func trustRow(rows []string) int {
	for i, r := range rows {
		if strings.HasPrefix(r, "y.") && strings.Contains(strings.ToLower(r), "trust") {
			return i
		}
	}
	return -1
}

// --- answering --------------------------------------------------------------------

func (screenReaderProfile) answerTrust(ctx context.Context, io *screenIO, trust bool) error {
	k := "n"
	if trust {
		k = "y"
	}
	return io.keys(ctx, k+terminal.KeyEnter)
}

func (screenReaderProfile) answerPermission(ctx context.Context, io *screenIO, ordinal int) error {
	return io.keys(ctx, strconv.Itoa(ordinal)+terminal.KeyEnter)
}

func (screenReaderProfile) confirm(ctx context.Context, io *screenIO) error {
	if ins, ok := axInstructionOf(io.t.Screen().Lines); ok && ins.yesNo {
		return io.keys(ctx, "y"+terminal.KeyEnter)
	}
	return io.keys(ctx, "1"+terminal.KeyEnter)
}

func (screenReaderProfile) answerQuestion(ctx context.Context, io *screenIO, req *bridge.Request, r bridge.Response) error {
	for i, q := range req.Questions {
		info, err := io.wait(ctx, 10*time.Second, "question "+strconv.Itoa(i+1)+" on screen", func(s screenInfo) bool {
			return s.Question != nil && s.TextEntry == 0 && sameQuestion(s.Question.Question, q.Text)
		})
		if err != nil {
			return err
		}
		if r.Cancel {
			return io.press(terminal.KeyEscape)
		}
		a := r.Answers[i]
		var picks []string
		for _, label := range a.Selected {
			picks = append(picks, strconv.Itoa(q.OptionIndex(label)+1))
		}
		textOrd := len(q.Options) + 1
		if d := info.Question; d != nil {
			if j := d.find(rowText, 0); j >= 0 {
				textOrd = d.Rows[j].Ordinal
			}
		}
		if a.Text != "" {
			picks = append(picks, strconv.Itoa(textOrd))
		}
		if err := io.keys(ctx, strings.Join(picks, ",")+terminal.KeyEnter); err != nil {
			return err
		}
		if a.Text != "" {
			if _, err := io.wait(ctx, 5*time.Second, "free-text entry", func(s screenInfo) bool { return s.TextEntry > 0 }); err != nil {
				return err
			}
			if err := io.keys(ctx, strings.Join(strings.Fields(a.Text), " ")+terminal.KeyEnter); err != nil {
				return err
			}
		}
		io.settle(ctx, 150*time.Millisecond, 1500*time.Millisecond)
	}
	info, _ := io.wait(ctx, 2*time.Second, "review", func(s screenInfo) bool {
		return s.Review != nil || (s.Question == nil && !s.dialogVisible())
	})
	if info.Review != nil {
		return io.keys(ctx, info.Review.Submit+terminal.KeyEnter)
	}
	return nil
}
