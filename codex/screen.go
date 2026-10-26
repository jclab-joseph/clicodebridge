package codex

import (
	"regexp"
	"strconv"
	"strings"

	bridge "github.com/jclab-joseph/clicodebridge"
)

// Recognizers for the Codex TUI (verified against codex-cli 0.164.0).

const focusMarker = "›"

type screenInfo struct {
	InputBox bool
	Busy     bool
	Plan     bool // "Plan mode" collaboration mode is on
	Approval *approvalDialog
	Question *questionDialog
	Trust    *approvalDialog
}

func (s screenInfo) dialogVisible() bool {
	return s.Approval != nil || s.Question != nil || s.Trust != nil
}

func (s screenInfo) idle() bool { return s.InputBox && !s.Busy && !s.dialogVisible() }

var (
	optionRe   = regexp.MustCompile(`^(\s*)(` + focusMarker + `\s*)?(\d+)\.\s+(.*)$`)
	questionRe = regexp.MustCompile(`^\s*Question (\d+)/(\d+)`)
	shortcutRe = regexp.MustCompile(`\s*\((?:[a-z]|esc)\)\s*$`)
)

func analyze(lines []string) screenInfo {
	var s screenInfo
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	for _, l := range lines {
		t := strings.TrimSpace(l)
		// "• Working (3s • esc to interrupt)". The question dialog footer
		// also says "esc to interrupt", but without the parenthesis.
		if strings.Contains(l, "esc to interrupt)") {
			s.Busy = true
		}
		if strings.HasPrefix(t, focusMarker) && !optionRe.MatchString(l) {
			s.InputBox = true
		}
		if strings.HasSuffix(t, "Plan mode") {
			s.Plan = true
		}
	}
	s.Question = parseQuestion(lines)
	if s.Question == nil {
		if d := parseApproval(lines); d != nil {
			if strings.Contains(strings.ToLower(d.Title), "trust") {
				s.Trust = d
			} else {
				s.Approval = d
			}
		}
	}
	if s.dialogVisible() {
		s.InputBox = false
	}
	return s
}

type screenOption struct {
	Ordinal int
	Label   string
	Focused bool
}

// parseOptions reads consecutive numbered rows starting at from.
func parseOptions(lines []string, from int) ([]screenOption, int) {
	var out []screenOption
	i := from
	for ; i < len(lines); i++ {
		m := optionRe.FindStringSubmatch(lines[i])
		if m == nil {
			if len(out) > 0 {
				break
			}
			continue
		}
		n, _ := strconv.Atoi(m[3])
		out = append(out, screenOption{Ordinal: n, Label: strings.TrimSpace(m[4]), Focused: m[2] != ""})
	}
	return out, i
}

// --- approval ------------------------------------------------------------------

type approvalDialog struct {
	Title   string
	Detail  string
	Options []screenOption
}

func parseApproval(lines []string) *approvalDialog {
	start := -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "Would you like to") || strings.HasPrefix(t, "Do you trust") || (strings.HasPrefix(t, "Allow ") && strings.HasSuffix(t, "?")) {
			start = i
		}
	}
	if start < 0 {
		return nil
	}
	opts, _ := parseOptions(lines, start+1)
	if len(opts) < 2 {
		return nil
	}
	d := &approvalDialog{Title: strings.TrimSpace(lines[start]), Options: opts}
	var detail []string
	for i := start + 1; i < len(lines); i++ {
		if optionRe.MatchString(lines[i]) {
			break
		}
		if t := strings.TrimSpace(lines[i]); t != "" {
			detail = append(detail, t)
		}
	}
	d.Detail = strings.Join(detail, "\n")
	return d
}

// cleanLabel drops the keyboard shortcut hint: "Yes, proceed (y)".
func cleanLabel(l string) string { return shortcutRe.ReplaceAllString(l, "") }

func classifyLabel(label string) bridge.PermissionKind {
	l := strings.ToLower(label)
	switch {
	case strings.HasPrefix(l, "no"):
		return bridge.Deny
	case strings.Contains(l, "don't ask again") || strings.Contains(l, "always") || strings.Contains(l, "this session"):
		return bridge.AllowAlways
	case strings.HasPrefix(l, "yes"):
		return bridge.AllowOnce
	}
	return bridge.OtherOption
}

// --- request_user_input ----------------------------------------------------------

type questionDialog struct {
	Index, Total int // 1-based "Question i/N"
	Question     string
	Options      []screenOption
}

func parseQuestion(lines []string) *questionDialog {
	for i, l := range lines {
		m := questionRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		d := &questionDialog{}
		d.Index, _ = strconv.Atoi(m[1])
		d.Total, _ = strconv.Atoi(m[2])
		j := i + 1
		for ; j < len(lines); j++ {
			if t := strings.TrimSpace(lines[j]); t != "" {
				d.Question = t
				break
			}
		}
		d.Options, _ = parseOptions(lines, j+1)
		for k := range d.Options {
			d.Options[k].Label = optionLabel(d.Options[k].Label)
		}
		if len(d.Options) == 0 {
			return nil
		}
		return d
	}
	return nil
}

// optionLabel strips the description column: "Go (Recommended)   Use Go…".
func optionLabel(l string) string {
	if i := strings.Index(l, "  "); i > 0 {
		return strings.TrimSpace(l[:i])
	}
	return l
}

func (d *questionDialog) focused() int {
	for i, o := range d.Options {
		if o.Focused {
			return i
		}
	}
	return -1
}

// noneIndex is the "None of the above" row, which takes free-text notes.
func (d *questionDialog) noneIndex() int {
	for i, o := range d.Options {
		if strings.HasPrefix(o.Label, "None of the above") {
			return i
		}
	}
	return -1
}
