package codex

import (
	"encoding/json"
	"strings"

	"github.com/bishenghua/lazycoding/internal/agent"
)

// rawEvent is the top-level envelope for codex's --json JSONL output.
// Codex uses Rust-style event type names (PascalCase).
type rawEvent struct {
	Type string `json:"type"`

	// AgentMessageDelta fields
	Delta *rawDelta `json:"delta"`

	// ExecCommandBegin fields
	Command []string `json:"command"`
	CallID  string   `json:"callId"`

	// ExecCommandEnd fields
	ExitCode *int   `json:"exitCode"`
	Output   string `json:"output"`

	// SessionCreated / SessionResumed
	SessionID string `json:"sessionId"`
}

type rawDelta struct {
	Type  string `json:"type"`  // "output_text"
	Text  string `json:"text"`  // incremental text chunk
	Index int    `json:"index"` // output index (usually 0)
}

// ParseLine converts one JSONL line from codex's --json output into zero or
// more agent.Event values.  sessionID is updated in place when a
// SessionCreated or SessionResumed event carries a session ID.
func ParseLine(line string, sessionID *string) []agent.Event {
	var raw rawEvent
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return nil
	}

	switch raw.Type {
	case "SessionCreated", "SessionResumed":
		if raw.SessionID != "" {
			*sessionID = raw.SessionID
			return []agent.Event{{Kind: agent.EventKindInit, SessionID: *sessionID}}
		}

	case "AgentMessageDelta":
		if raw.Delta != nil && raw.Delta.Type == "output_text" && raw.Delta.Text != "" {
			return []agent.Event{{Kind: agent.EventKindText, Text: raw.Delta.Text}}
		}

	case "ExecCommandBegin":
		if len(raw.Command) > 0 {
			name := "Exec"
			input := strings.Join(raw.Command, " ")
			// Unwrap common shell wrappers: ["bash", "-c", "actual command"]
			if len(raw.Command) >= 3 &&
				(raw.Command[0] == "bash" || raw.Command[0] == "sh") &&
				raw.Command[1] == "-c" {
				input = raw.Command[2]
			}
			return []agent.Event{{
				Kind:      agent.EventKindToolUse,
				ToolName:  name,
				ToolInput: input,
				ToolUseID: raw.CallID,
			}}
		}

	case "ExecCommandEnd":
		if raw.Output != "" {
			return []agent.Event{{
				Kind:       agent.EventKindToolResult,
				ToolUseID:  raw.CallID,
				ToolResult: raw.Output,
			}}
		}
	}

	return nil
}
