package clicodebridge

import (
	"encoding/json"
	"fmt"
	"time"
)

// RequestKind distinguishes permission prompts from questions.
type RequestKind string

const (
	// RequestPermission asks whether the agent may do something; answer with
	// one of Permission.Options.
	RequestPermission RequestKind = "permission"
	// RequestQuestion asks the user one or more questions; answer with
	// Response.Answers.
	RequestQuestion RequestKind = "question"
)

// Request is something the CLI needs the user to decide.
type Request struct {
	ID    string      `json:"id"`
	Kind  RequestKind `json:"kind"`
	Time  time.Time   `json:"time"`
	Title string      `json:"title"`

	Permission *Permission `json:"permission,omitempty"`
	Questions  []Question  `json:"questions,omitempty"`
}

// Permission describes a permission prompt.
type Permission struct {
	// ToolName is the tool asking, e.g. "Bash". Empty for non-tool prompts
	// such as workspace trust.
	ToolName string          `json:"toolName,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	// Detail is a human-readable summary such as the command line.
	Detail  string             `json:"detail,omitempty"`
	Options []PermissionOption `json:"options"`
}

// PermissionOption is one way to answer a Permission.
type PermissionOption struct {
	ID    string         `json:"id"`
	Label string         `json:"label"`
	Kind  PermissionKind `json:"kind"`
}

// PermissionKind classifies a PermissionOption.
type PermissionKind string

const (
	AllowOnce   PermissionKind = "allow_once"
	AllowAlways PermissionKind = "allow_always"
	Deny        PermissionKind = "deny"
	OtherOption PermissionKind = "other"
)

// Question is one question with selectable options.
type Question struct {
	ID          string           `json:"id,omitempty"`
	Header      string           `json:"header,omitempty"`
	Text        string           `json:"text"`
	MultiSelect bool             `json:"multiSelect,omitempty"`
	Options     []QuestionOption `json:"options"`
	// AllowText is true when a free-text answer is accepted.
	AllowText bool `json:"allowText,omitempty"`
}

// QuestionOption is one choice of a Question.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Response answers a Request.
type Response struct {
	// OptionID picks a Permission option. If empty, the first option of
	// Decision's kind is used.
	OptionID string         `json:"optionId,omitempty"`
	Decision PermissionKind `json:"decision,omitempty"`
	// Message is feedback for a denial, where the CLI supports it.
	Message string `json:"message,omitempty"`

	// Answers answer a question request, index-aligned with Questions.
	Answers []Answer `json:"answers,omitempty"`

	// Cancel dismisses the request (Esc in the TUI).
	Cancel bool `json:"cancel,omitempty"`
}

// Answer answers one Question. Selected holds option labels; Text is a
// free-text answer. Both may be set for multi-select questions.
type Answer struct {
	Selected []string `json:"selected,omitempty"`
	Text     string   `json:"text,omitempty"`
}

// Allow returns a Response choosing the first allow-once option.
func Allow() Response { return Response{Decision: AllowOnce} }

// AllowForever returns a Response choosing the first allow-always option.
func AllowForever() Response { return Response{Decision: AllowAlways} }

// Reject returns a Response choosing the first deny option.
func Reject(message string) Response { return Response{Decision: Deny, Message: message} }

// Choose returns a Response selecting labels, one Answer per question.
func Choose(labels ...string) Response {
	r := Response{}
	for _, l := range labels {
		r.Answers = append(r.Answers, Answer{Selected: []string{l}})
	}
	return r
}

// ResolveOption finds the option a Response picks for this permission.
func (p *Permission) ResolveOption(r Response) (PermissionOption, error) {
	if r.OptionID != "" {
		for _, o := range p.Options {
			if o.ID == r.OptionID {
				return o, nil
			}
		}
		return PermissionOption{}, fmt.Errorf("clicodebridge: unknown permission option %q", r.OptionID)
	}
	kind := r.Decision
	if kind == "" {
		if r.Cancel {
			kind = Deny
		} else {
			kind = AllowOnce
		}
	}
	for _, o := range p.Options {
		if o.Kind == kind {
			return o, nil
		}
	}
	return PermissionOption{}, fmt.Errorf("clicodebridge: no %s option available", kind)
}

// OptionIndex returns the index of the option with label, or -1.
func (q *Question) OptionIndex(label string) int {
	for i, o := range q.Options {
		if o.Label == label {
			return i
		}
	}
	return -1
}

// ValidateAnswers checks that answers fit the questions.
func ValidateAnswers(qs []Question, answers []Answer) error {
	if len(answers) != len(qs) {
		return fmt.Errorf("clicodebridge: got %d answers for %d questions", len(answers), len(qs))
	}
	for i, q := range qs {
		a := answers[i]
		if len(a.Selected) == 0 && a.Text == "" {
			return fmt.Errorf("clicodebridge: question %d (%q) has no answer", i+1, q.Text)
		}
		if !q.MultiSelect && len(a.Selected)+boolInt(a.Text != "") > 1 {
			return fmt.Errorf("clicodebridge: question %d (%q) takes a single answer", i+1, q.Text)
		}
		if a.Text != "" && !q.AllowText {
			return fmt.Errorf("clicodebridge: question %d (%q) does not accept free text", i+1, q.Text)
		}
		for _, s := range a.Selected {
			if q.OptionIndex(s) < 0 {
				return fmt.Errorf("clicodebridge: question %d (%q) has no option %q", i+1, q.Text, s)
			}
		}
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
