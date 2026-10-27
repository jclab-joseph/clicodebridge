package clicodebridge

import (
	"encoding/json"
	"time"
)

// EventType identifies an Event.
type EventType string

const (
	EventSessionID       EventType = "session_id"       // SessionID became known
	EventState           EventType = "state"            // State changed
	EventUserMessage     EventType = "user_message"     // Text: a user prompt (from any client, including the TUI or remote control)
	EventAssistantText   EventType = "assistant_text"   // Text: assistant output
	EventThinking        EventType = "thinking"         // Text: visible reasoning
	EventToolCall        EventType = "tool_call"        // Tool
	EventToolResult      EventType = "tool_result"      // Tool
	EventTurnStarted     EventType = "turn_started"     //
	EventTurnCompleted   EventType = "turn_completed"   // Text: final assistant message, Interrupted
	EventRequest         EventType = "request"          // Request: answer with Session.Respond
	EventRequestResolved EventType = "request_resolved" // RequestID, Resolution
	EventSettings        EventType = "settings"         // Settings changed
	EventNotice          EventType = "notice"           // Text, Kind: informational CLI notification
	EventRestarted       EventType = "restarted"        // the CLI process was relaunched; re-read Terminal()
	EventDiagnostic      EventType = "diagnostic"       // Kind ("drift"), Text, Screen: a recognizer disagrees with the CLI's own state
	EventError           EventType = "error"            // Text
	EventExited          EventType = "exited"           // Text: exit reason
)

// Event is one thing that happened in a session. Only the fields relevant to
// Type are set.
type Event struct {
	Type      EventType `json:"type"`
	SessionID string    `json:"sessionId,omitempty"`
	Time      time.Time `json:"time"`

	Text        string     `json:"text,omitempty"`
	Kind        string     `json:"kind,omitempty"`
	State       State      `json:"state,omitempty"`
	Interrupted bool       `json:"interrupted,omitempty"`
	Tool        *ToolEvent `json:"tool,omitempty"`
	Request     *Request   `json:"request,omitempty"`
	RequestID   string     `json:"requestId,omitempty"`
	Resolution  Resolution `json:"resolution,omitempty"`
	Settings    *Settings  `json:"settings,omitempty"`
	// Screen is the terminal text, for diagnostics.
	Screen string `json:"screen,omitempty"`

	// Raw is the provider record the event came from, when there is one.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// ToolEvent describes a tool call or its result.
type ToolEvent struct {
	ID      string          `json:"id,omitempty"`
	Name    string          `json:"name,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Output  string          `json:"output,omitempty"`
	IsError bool            `json:"isError,omitempty"`
}

// Resolution says how a Request stopped being pending.
type Resolution string

const (
	ResolvedByClient   Resolution = "client"    // answered through Respond
	ResolvedExternally Resolution = "external"  // answered in the TUI or via remote control
	ResolvedCancelled  Resolution = "cancelled" // the CLI withdrew it (interrupt, turn end)
)
