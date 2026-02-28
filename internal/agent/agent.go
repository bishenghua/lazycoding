package agent

import "context"

// EventKind classifies a streaming event from the AI backend.
type EventKind int

const (
	EventKindInit       EventKind = iota // session initialised
	EventKindText                        // incremental text from the model
	EventKindToolUse                     // tool invocation
	EventKindToolResult                  // output returned by a tool
	EventKindResult                      // final result with session ID
	EventKindError                       // terminal error
)

// Event is one item emitted on the stream returned by Agent.Stream.
type Event struct {
	Kind       EventKind
	Text       string // EventKindText / EventKindResult
	ToolName   string // EventKindToolUse
	ToolInput  string // EventKindToolUse – human-readable summary of the input
	ToolUseID  string // EventKindToolUse / EventKindToolResult – correlation id
	ToolResult string // EventKindToolResult – raw output from the tool
	SessionID  string // EventKindInit / EventKindResult
	Err        error  // EventKindError
}

// StreamRequest carries everything needed to start an AI inference.
type StreamRequest struct {
	Prompt    string
	SessionID string // empty = new session

	// Per-request overrides. Zero values mean "use the runner's configured default".
	WorkDir    string   // working directory for the claude subprocess
	ExtraFlags []string // additional CLI flags; nil = use runner default
}

// Agent is the interface for any AI backend.
type Agent interface {
	Stream(ctx context.Context, req StreamRequest) (<-chan Event, error)
}
