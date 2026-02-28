package claude

import (
	"encoding/json"
	"fmt"
	"strings"

	"lazycoding/internal/agent"
)

// rawLine is the top-level structure of a stream-json line.
type rawLine struct {
	Type      string      `json:"type"`
	Subtype   string      `json:"subtype"`
	SessionID string      `json:"session_id"`
	Message   *rawMessage `json:"message"`
	Result    string      `json:"result"`
	IsError   bool        `json:"is_error"`
}

type rawMessage struct {
	Role    string       `json:"role"`
	Content []rawContent `json:"content"`
}

type rawContent struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`          // tool_use: unique id
	Text      string          `json:"text"`
	Name      string          `json:"name"`        // tool_use: tool name
	Input     json.RawMessage `json:"input"`       // tool_use: input params
	ToolUseID string          `json:"tool_use_id"` // tool_result: correlates to tool_use.ID
	Content   json.RawMessage `json:"content"`     // tool_result: output (string or []block)
}

// ParseLineMulti converts one JSONL line from claude's stream-json output into
// zero or more agent.Event values, handling multi-block assistant messages.
func ParseLineMulti(line string) []agent.Event {
	var raw rawLine
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return nil
	}

	switch raw.Type {
	case "system":
		return []agent.Event{{Kind: agent.EventKindInit, SessionID: raw.SessionID}}

	case "assistant":
		if raw.Message == nil {
			return nil
		}
		var events []agent.Event
		for _, c := range raw.Message.Content {
			switch c.Type {
			case "text":
				if c.Text != "" {
					events = append(events, agent.Event{Kind: agent.EventKindText, Text: c.Text})
				}
			case "tool_use":
				events = append(events, agent.Event{
					Kind:      agent.EventKindToolUse,
					ToolName:  c.Name,
					ToolInput: formatInput(c.Input),
					ToolUseID: c.ID,
				})
			}
		}
		return events

	case "user":
		// User messages carry tool_result blocks – the actual output of tool calls.
		if raw.Message == nil {
			return nil
		}
		var events []agent.Event
		for _, c := range raw.Message.Content {
			if c.Type != "tool_result" {
				continue
			}
			text := extractResultText(c.Content)
			if text == "" {
				continue
			}
			events = append(events, agent.Event{
				Kind:       agent.EventKindToolResult,
				ToolUseID:  c.ToolUseID,
				ToolResult: text,
			})
		}
		return events

	case "result":
		if raw.IsError {
			return []agent.Event{{Kind: agent.EventKindError, Err: fmt.Errorf("claude error: %s", raw.Result)}}
		}
		return []agent.Event{{Kind: agent.EventKindResult, Text: raw.Result, SessionID: raw.SessionID}}
	}

	return nil
}

// extractResultText pulls plain text out of a tool_result content field.
// The field may be a JSON string, or an array of typed content blocks.
func extractResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try plain string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Try array of content blocks: [{"type":"text","text":"..."}]
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// formatInput extracts a human-readable summary from a tool_use input object.
func formatInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err == nil {
		for _, key := range []string{"command", "description", "path", "query"} {
			if v, ok := m[key]; ok {
				if s, ok := v.(string); ok {
					return s
				}
			}
		}
	}
	return string(raw)
}
