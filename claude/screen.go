package claude

import (
	"regexp"
	"strconv"
	"strings"

	bridge "github.com/jclab-joseph/clicodebridge"
)

// Recognizers for the Claude Code TUI (verified against 2.1.296). They work
// on the emulator's plain-text screen, one string per row.

const focusMarker = "❯"

// screenInfo is everything recognized on one screen.
type screenInfo struct {
	InputBox   bool // the prompt input box is visible
	Busy       bool // a turn is running ("esc to interrupt")
	Mode       string
	Effort     string
	RemoteURL  string
	Trust      *trustDialog
	Permission *permissionDialog
	Question   *questionDialog
	Review     *reviewDialog
	ModelSwap  bool // "Switch model?" confirmation after /model
}

// dialogVisible reports whether any modal dialog is on screen.
func (s screenInfo) dialogVisible() bool {
	return s.Trust != nil || s.Permission != nil || s.Question != nil || s.Review != nil || s.ModelSwap
}

// idle reports whether a prompt can be typed now.
func (s screenInfo) idle() bool { return s.InputBox && !s.Busy && !s.dialogVisible() }

var (
	ruleRe      = regexp.MustCompile(`^\s*─{20,}\s*$`)
	optionRe    = regexp.MustCompile(`^(\s*)(` + focusMarker + `\s*)?(\d+)\.\s+(.*)$`)
	remoteURLRe = regexp.MustCompile(`https://claude\.ai/code/session_[A-Za-z0-9]+`)
	effortRe    = regexp.MustCompile(`(\S+)\s+·\s+/effort\s*$`)
)

func analyze(lines []string) screenInfo {
	var s screenInfo
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	full := strings.Join(lines, "\n")
	if m := remoteURLRe.FindString(full); m != "" {
		s.RemoteURL = m
	}
	s.InputBox = hasInputBox(lines)
	footer := footerText(lines)
	s.Busy = strings.Contains(footer, "esc to interrupt")
	s.Mode = modeFromFooter(footer)
	for _, l := range lines {
		if m := effortRe.FindStringSubmatch(l); m != nil {
			s.Effort = normalizeEffort(m[1])
		}
	}
	s.Trust = parseTrust(lines)
	s.Review = parseReview(lines)
	if s.Review == nil {
		s.Question = parseQuestion(lines)
	}
	if s.Question == nil && s.Review == nil {
		s.Permission = parsePermission(lines)
	}
	s.ModelSwap = strings.Contains(full, "Switch model?") && strings.Contains(full, "Yes, switch to")
	return s
}

// hasInputBox finds "────" / "❯ ..." / "────": the prompt editor.
func hasInputBox(lines []string) bool {
	for i := 0; i+2 < len(lines); i++ {
		if !ruleRe.MatchString(lines[i]) || !strings.HasPrefix(lines[i+1], focusMarker) {
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

// footerText returns the non-empty rows below the last rule line.
func footerText(lines []string) string {
	last := -1
	for i, l := range lines {
		if ruleRe.MatchString(l) {
			last = i
		}
	}
	if last < 0 {
		return ""
	}
	return strings.Join(lines[last+1:], "\n")
}

func modeFromFooter(footer string) string {
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

// --- workspace trust -------------------------------------------------------

type trustDialog struct {
	Focused int // index into trustRows
	Rows    []string
}

var trustRows = []string{"Yes, I trust this folder", "No, exit"}

func parseTrust(lines []string) *trustDialog {
	d := &trustDialog{Focused: -1}
	for _, l := range lines {
		t := strings.TrimSpace(l)
		focused := strings.HasPrefix(t, focusMarker)
		t = strings.TrimSpace(strings.TrimPrefix(t, focusMarker))
		for _, r := range trustRows {
			if t == r {
				if focused {
					d.Focused = len(d.Rows)
				}
				d.Rows = append(d.Rows, r)
			}
		}
	}
	if len(d.Rows) < 2 {
		return nil
	}
	return d
}

func (d *trustDialog) index(row string) int {
	for i, r := range d.Rows {
		if r == row {
			return i
		}
	}
	return -1
}

// --- numbered option lists -------------------------------------------------

type screenOption struct {
	Ordinal int
	Label   string
	Focused bool
	Line    int
}

// parseOptions reads consecutive "N. label" rows starting at line from,
// folding indented continuation rows into the label.
func parseOptions(lines []string, from int) []screenOption {
	var out []screenOption
	for i := from; i < len(lines); i++ {
		l := lines[i]
		if ruleRe.MatchString(l) {
			break
		}
		if m := optionRe.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[3])
			out = append(out, screenOption{Ordinal: n, Label: strings.TrimSpace(m[4]), Focused: m[2] != "", Line: i})
			continue
		}
		t := strings.TrimSpace(l)
		if t == "" || len(out) == 0 {
			continue
		}
		// Wrapped label rows are indented deeper than the option number;
		// anything else (the dialog footer) ends the list.
		if indent(l) > indent(lines[out[len(out)-1].Line]) {
			out[len(out)-1].Label += " " + t
			continue
		}
		break
	}
	return out
}

func indent(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }

// --- permission dialog -----------------------------------------------------

type permissionDialog struct {
	Title   string
	Prompt  string
	Options []screenOption
}

func parsePermission(lines []string) *permissionDialog {
	q := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "Do you want to ") {
			q = i
		}
	}
	if q < 0 {
		return nil
	}
	opts := parseOptions(lines, q+1)
	if len(opts) < 2 {
		return nil
	}
	d := &permissionDialog{Prompt: strings.TrimSpace(lines[q]), Options: opts}
	for i := q - 1; i >= 0; i-- {
		if ruleRe.MatchString(lines[i]) {
			for j := i + 1; j < q; j++ {
				if t := strings.TrimSpace(lines[j]); t != "" {
					d.Title = t
					break
				}
			}
			break
		}
	}
	return d
}

// classifyPermissionLabel maps a TUI option label to a decision kind.
func classifyPermissionLabel(label string) bridge.PermissionKind {
	l := strings.ToLower(label)
	switch {
	case strings.HasPrefix(l, "no"):
		return bridge.Deny
	case strings.Contains(l, "always") || strings.Contains(l, "don't ask") ||
		strings.Contains(l, "during this session") || strings.Contains(l, "allow all"):
		return bridge.AllowAlways
	case l == "yes" || (strings.HasPrefix(l, "yes") && !strings.Contains(l, " and ")):
		return bridge.AllowOnce
	}
	return bridge.OtherOption
}

// --- AskUserQuestion dialog ------------------------------------------------

type questionRowKind int

const (
	rowOption questionRowKind = iota
	rowText                   // "Type something" (free text)
	rowSubmit                 // multi-select "Submit"
	rowChat                   // "Chat about this"
)

type questionRow struct {
	Kind    questionRowKind
	Ordinal int // 0 for the unnumbered Submit row
	Label   string
	Checked bool
	Focused bool
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

func isTabBar(l string) bool {
	return strings.Contains(l, "☐") || strings.Contains(l, "☒") || strings.Contains(l, "✔ Submit")
}

func parseQuestion(lines []string) *questionDialog {
	footer := false
	for _, l := range lines {
		if strings.Contains(l, "Enter to select") {
			footer = true
		}
	}
	if !footer {
		return nil
	}
	bar := -1
	for i, l := range lines {
		if isTabBar(l) {
			bar = i
		}
	}
	if bar < 0 {
		return nil
	}
	d := &questionDialog{}
	i := bar + 1
	for ; i < len(lines); i++ {
		if t := strings.TrimSpace(lines[i]); t != "" {
			d.Question = t
			i++
			break
		}
	}
	for ; i < len(lines); i++ {
		l := lines[i]
		t := strings.TrimSpace(l)
		if strings.Contains(l, "Enter to select") {
			break
		}
		if m := optionRe.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[3])
			label := strings.TrimSpace(m[4])
			row := questionRow{Kind: rowOption, Ordinal: n, Focused: m[2] != ""}
			if strings.HasPrefix(label, "[") && len(label) > 3 {
				if end := strings.Index(label, "]"); end > 0 {
					d.Multi = true
					row.Checked = strings.TrimSpace(label[1:end]) != ""
					label = strings.TrimSpace(label[end+1:])
				}
			}
			row.Label = label
			if strings.HasPrefix(label, "Chat about this") {
				row.Kind = rowChat
			}
			d.Rows = append(d.Rows, row)
			continue
		}
		if strings.TrimSpace(strings.TrimPrefix(t, focusMarker)) == "Submit" {
			d.Rows = append(d.Rows, questionRow{Kind: rowSubmit, Label: "Submit", Focused: strings.HasPrefix(t, focusMarker)})
		}
	}
	// The free-text row is the last numbered row before Submit/Chat. Its
	// label is "Type something" until the user types, then the typed text.
	last := -1
	for k, r := range d.Rows {
		if r.Kind == rowOption {
			last = k
		}
	}
	if last >= 0 {
		d.Rows[last].Kind = rowText
	}
	if len(d.Rows) == 0 || d.Question == "" {
		return nil
	}
	return d
}

type reviewDialog struct {
	Options []screenOption
}

func parseReview(lines []string) *reviewDialog {
	start := -1
	for i, l := range lines {
		if strings.Contains(l, "Ready to submit your answers?") {
			start = i
		}
	}
	if start < 0 {
		return nil
	}
	opts := parseOptions(lines, start+1)
	if len(opts) == 0 {
		return nil
	}
	return &reviewDialog{Options: opts}
}

func (d *reviewDialog) submitOrdinal() int {
	for _, o := range d.Options {
		if strings.HasPrefix(o.Label, "Submit") {
			return o.Ordinal
		}
	}
	return 0
}
