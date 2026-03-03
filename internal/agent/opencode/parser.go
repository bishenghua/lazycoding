package opencode

import (
	"encoding/json"
	"strings"

	"github.com/bishenghua/lazycoding/internal/agent"
)

// rawEvent is the top-level envelope for opencode's --format json JSONL output.
type rawEvent struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

type rawMsgPartProps struct {
	Part rawPart `json:"part"`
}

type rawPart struct {
	Type           string             `json:"type"`
	Delta          string             `json:"delta"`          // incremental text chunk (type=text)
	ToolInvocation *rawToolInvocation `json:"toolInvocation"` // non-nil when type=tool-invocation
}

type rawToolInvocation struct {
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	State      string          `json:"state"` // "partial-call" | "call" | "result"
	Args       json.RawMessage `json:"args"`
	Result     string          `json:"result"`
}

type rawSessionProps struct {
	Info struct {
		ID string `json:"id"`
	} `json:"info"`
}

// ParseLine converts one JSONL line from opencode's --format json output into
// zero or more agent.Event values.  sessionID is updated in place when a
// session.updated event carries a session ID.
func ParseLine(line string, sessionID *string) []agent.Event {
	var raw rawEvent
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return nil
	}

	switch raw.Type {
	case "session.updated":
		var props rawSessionProps
		if err := json.Unmarshal(raw.Properties, &props); err == nil && props.Info.ID != "" {
			*sessionID = props.Info.ID
			return []agent.Event{{Kind: agent.EventKindInit, SessionID: *sessionID}}
		}

	case "message.part.updated":
		var props rawMsgPartProps
		if err := json.Unmarshal(raw.Properties, &props); err != nil {
			return nil
		}
		switch props.Part.Type {
		case "text":
			if props.Part.Delta != "" {
				return []agent.Event{{Kind: agent.EventKindText, Text: props.Part.Delta}}
			}
		case "tool-invocation":
			inv := props.Part.ToolInvocation
			if inv == nil {
				return nil
			}
			switch inv.State {
			case "call":
				return []agent.Event{{
					Kind:      agent.EventKindToolUse,
					ToolName:  inv.ToolName,
					ToolInput: formatArgs(inv.Args),
					ToolUseID: inv.ToolCallID,
				}}
			case "result":
				if inv.Result != "" {
					return []agent.Event{{
						Kind:       agent.EventKindToolResult,
						ToolUseID:  inv.ToolCallID,
						ToolResult: inv.Result,
					}}
				}
			}
		}
	}

	return nil
}

// formatArgs extracts a human-readable summary from a tool invocation args object.
func formatArgs(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil {
		for _, key := range []string{"command", "description", "path", "query", "content"} {
			if v, ok := m[key]; ok {
				if s, ok := v.(string); ok {
					return strings.TrimSpace(s)
				}
			}
		}
	}
	return string(raw)
}
