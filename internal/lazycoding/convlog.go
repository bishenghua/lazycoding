package lazycoding

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// convlog prints human-readable conversation transcripts to stderr so that
// server operators can follow the interaction in real time.
//
// Output goes to stderr alongside the structured slog output.
// ANSI colors are used when stderr is connected to a terminal.

const (
	ansiReset       = "\033[0m"
	ansiBold        = "\033[1m"
	ansiGray        = "\033[90m"
	ansiCyan        = "\033[36m"
	ansiGreen       = "\033[32m"
	ansiYellow      = "\033[33m"
	ansiRed         = "\033[31m"
	ansiBlue        = "\033[34m"
	ansiMagenta     = "\033[35m"
	ansiBrightCyan  = "\033[96m"
	ansiBrightGreen = "\033[92m"
	ansiWhite       = "\033[37m"
)

// useColor is set once at startup: true if stderr is a terminal.
var useColor = func() bool {
	fi, err := os.Stderr.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}()

func color(code, s string) string {
	if !useColor {
		return s
	}
	return code + s + ansiReset
}

// toolColor returns the ANSI color for a tool name.
// Handles Claude Code built-ins, codex's "Exec", and fuzzy-matches opencode
// tool names (e.g. execute_bash, read_file) based on common naming patterns.
func toolColor(toolName string) string {
	// Exact matches — Claude Code built-ins + sub-agent types
	switch toolName {
	case "Bash", "Exec": // Exec = codex's shell tool
		return ansiBrightCyan
	case "Read", "Write", "Edit", "MultiEdit", "NotebookRead", "NotebookEdit":
		return ansiBlue
	case "Glob", "Grep", "LS":
		return ansiMagenta
	case "WebFetch", "WebSearch":
		return ansiBrightGreen
	case "AskUserQuestion", "TodoWrite", "TodoRead", "ExitPlanMode":
		return ansiYellow
	case "Agent", "Task", "claude":
		return ansiCyan
	case "opencode":
		return ansiBrightGreen
	case "codex":
		return ansiMagenta
	case "general-purpose", "Explore":
		return ansiBrightGreen
	case "Plan":
		return ansiMagenta
	case "statusline-setup":
		return ansiYellow
	}

	// Fuzzy match for opencode / other backends with snake_case tool names
	lower := strings.ToLower(toolName)
	switch {
	case containsAny(lower, "bash", "exec", "run", "command", "shell"):
		return ansiBrightCyan
	case containsAny(lower, "read", "write", "edit", "create", "delete", "file", "patch"):
		return ansiBlue
	case containsAny(lower, "grep", "search", "find", "glob", "list", "ls", "dir"):
		return ansiMagenta
	case containsAny(lower, "fetch", "web", "url", "http", "browse"):
		return ansiBrightGreen
	case containsAny(lower, "todo", "ask", "question", "plan", "task"):
		return ansiYellow
	default:
		return ansiYellow
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func ts() string {
	return color(ansiGray, time.Now().Format("15:04:05"))
}

// indent adds "  " before each line of s.
func indent(s string) string {
	s = strings.TrimRight(s, "\n")
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}

// extractSubagentType tries to parse the subagent_type from a JSON tool input.
func extractSubagentType(input string) string {
	if input == "" {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(input), &m); err != nil {
		return ""
	}
	raw, ok := m["subagent_type"]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return strings.Trim(string(raw), `"`)
	}
	return s
}

// convLogRecv logs an incoming user message.
func convLogRecv(convID, userKey, text string) {
	arrow := color(ansiBold+ansiCyan, "▶")
	meta := color(ansiGray, fmt.Sprintf("conv=%s  %s", convID, userKey))
	fmt.Fprintf(os.Stderr, "\n%s %s %s\n%s\n",
		ts(), arrow, meta, indent(text))
}

// convLogTool logs a tool invocation in Claude Code CLI style:
//
//	HH:MM:SS  ⏺ ToolName(summary)
func convLogTool(name, input, workDir string) {
	displayName := name
	colorName := name
	if name == "Agent" {
		subagentType := extractSubagentType(input)
		if subagentType != "" {
			displayName = "Agent (" + subagentType + ")"
			colorName = subagentType
		}
	}

	summary := formatToolInput(name, input, workDir)
	bullet := color(toolColor(colorName), "⏺")
	label := color(toolColor(colorName)+ansiBold, displayName)

	if summary == "" {
		fmt.Fprintf(os.Stderr, "%s  %s %s\n", ts(), bullet, label)
		return
	}

	lines := strings.Split(strings.TrimRight(summary, "\n"), "\n")
	if len(lines) == 1 {
		// Single-line: ⏺ ToolName(args)
		fmt.Fprintf(os.Stderr, "%s  %s %s(%s)\n",
			ts(), bullet, label, color(ansiGray, lines[0]))
	} else {
		// Multi-line (e.g. heredoc): show first line inline, rest indented
		// "15:04:07  ⏺ Bash(first line" — 8+2+2+1+len(name)+1 = varies
		// Use a fixed continuation indent of 13 spaces to keep it readable.
		const contIndent = "             "
		fmt.Fprintf(os.Stderr, "%s  %s %s(%s\n",
			ts(), bullet, label, color(ansiGray, lines[0]))
		for i, line := range lines[1:] {
			if i == len(lines)-2 {
				fmt.Fprintf(os.Stderr, "%s%s)\n", contIndent, color(ansiGray, line))
			} else {
				fmt.Fprintf(os.Stderr, "%s%s\n", contIndent, color(ansiGray, line))
			}
		}
	}
}

// convLogToolResult logs tool output in Claude Code CLI style:
//
//	          ⎿  output line 1
//	             output line 2
func convLogToolResult(result string) {
	if result == "" {
		return
	}
	trimmed := strings.TrimSpace(result)
	const maxLines = 20
	const maxChars = 1000

	lines := strings.Split(trimmed, "\n")
	truncated := false
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		truncated = true
	}
	out := strings.Join(lines, "\n")
	if len(out) > maxChars {
		out = safeSlice(out, maxChars)
		if idx := strings.LastIndex(out, "\n"); idx > 0 {
			out = out[:idx]
		}
		truncated = true
	}
	if truncated {
		out += "\n…"
	}

	// Align ⎿ at the same column as ⏺ (timestamp=8 + 2 spaces = 10).
	const prefix = "          " // 10 spaces
	outLines := strings.Split(out, "\n")
	fmt.Fprintf(os.Stderr, "%s%s  %s\n",
		prefix, color(ansiGray, "⎿"), color(ansiGray, outLines[0]))
	for _, line := range outLines[1:] {
		fmt.Fprintf(os.Stderr, "%s   %s\n", prefix, color(ansiGray, line))
	}
}

// shortenPath strips the workDir prefix from p; if still long, keeps last 3 segments.
func shortenPath(p, workDir string) string {
	if workDir != "" && strings.HasPrefix(p, workDir) {
		rel := strings.TrimPrefix(p, workDir)
		rel = strings.TrimPrefix(rel, "/")
		if rel != "" {
			p = rel
		}
	}
	if len(p) > 80 {
		parts := strings.Split(p, "/")
		if len(parts) > 3 {
			p = "…/" + strings.Join(parts[len(parts)-3:], "/")
		}
	}
	return p
}

// formatToolInput extracts a readable summary from a tool's input.
//
// Claude Code sends JSON; codex sends plain command strings; opencode sends
// the first meaningful field value (already extracted by its parser).
func formatToolInput(toolName, input, workDir string) string {
	if input == "" {
		return ""
	}

	// Codex: "Exec" carries a plain command string, not JSON.
	if toolName == "Exec" {
		return truncStr(input, 1000)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(input), &m); err != nil {
		// Non-JSON input (opencode returns plain strings from its formatArgs).
		// If it looks like a filesystem path, shorten it.
		if strings.HasPrefix(input, "/") || strings.HasPrefix(input, "~") {
			return shortenPath(truncStr(input, 1000), workDir)
		}
		return truncStr(input, 1000)
	}

	getString := func(keys ...string) string {
		for _, key := range keys {
			raw, ok := m[key]
			if !ok {
				continue
			}
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return strings.Trim(string(raw), `"`)
			}
			if s != "" {
				return s
			}
		}
		return ""
	}

	// Claude Code built-in tools
	switch toolName {
	case "Read", "Write", "Edit", "MultiEdit", "NotebookRead", "NotebookEdit":
		p := getString("file_path", "notebook_path", "path")
		return shortenPath(p, workDir)

	case "LS":
		p := getString("path")
		if p == "" {
			return "."
		}
		return shortenPath(p, workDir)

	case "Bash":
		cmd := getString("command")
		return truncStr(cmd, 1000)

	case "Glob":
		pat := getString("pattern")
		dir := getString("path")
		if dir != "" {
			return pat + "  in " + shortenPath(dir, workDir)
		}
		return pat

	case "Grep":
		pat := getString("pattern")
		path := getString("path")
		glob := getString("glob")
		s := pat
		if glob != "" {
			s += "  [" + glob + "]"
		}
		if path != "" {
			s += "  in " + shortenPath(path, workDir)
		}
		return s

	case "WebFetch":
		return truncStr(getString("url"), 120)

	case "WebSearch":
		return getString("query")

	case "TodoWrite":
		raw, ok := m["todos"]
		if ok {
			var todos []any
			if err := json.Unmarshal(raw, &todos); err == nil {
				return fmt.Sprintf("%d todos", len(todos))
			}
		}

	case "TodoRead":
		return ""

	case "ExitPlanMode":
		return ""

	case "AskUserQuestion":
		raw, ok := m["questions"]
		if ok {
			var questions []struct {
				Question string `json:"question"`
			}
			if err := json.Unmarshal(raw, &questions); err == nil && len(questions) > 0 {
				return truncStr(questions[0].Question, 120)
			}
		}

	case "Task", "Agent":
		subagentType := getString("subagent_type")
		desc := getString("description", "prompt")
		result := subagentType
		if desc != "" {
			desc = truncStr(desc, 80)
			if result != "" {
				result += ": " + desc
			} else {
				result = desc
			}
		}
		if result == "" {
			result = "agent"
		}
		return result
	}

	// Fuzzy match for opencode / other backends (snake_case tool names).
	// Try common field names for the most frequent tool categories.
	lower := strings.ToLower(toolName)
	switch {
	case containsAny(lower, "bash", "exec", "run", "command", "shell"):
		cmd := getString("command", "cmd", "script")
		return truncStr(cmd, 1000)

	case containsAny(lower, "read", "write", "edit", "create", "delete", "file", "patch", "view"):
		p := getString("path", "file_path", "filename", "target")
		return shortenPath(p, workDir)

	case containsAny(lower, "list", "ls", "dir", "directory"):
		p := getString("path", "directory", "dir")
		if p == "" {
			return "."
		}
		return shortenPath(p, workDir)

	case containsAny(lower, "grep", "search", "find", "glob"):
		q := getString("pattern", "query", "glob", "keyword")
		return truncStr(q, 120)

	case containsAny(lower, "fetch", "web", "url", "browse", "http"):
		return truncStr(getString("url", "uri", "link"), 120)

	case containsAny(lower, "todo"):
		raw, ok := m["todos"]
		if ok {
			var todos []any
			if err := json.Unmarshal(raw, &todos); err == nil {
				return fmt.Sprintf("%d todos", len(todos))
			}
		}
		return getString("content", "text")
	}

	// Final fallback: truncated raw JSON
	return truncStr(input, 160)
}

// truncStr truncates s to at most max bytes, appending "…" if cut.
func truncStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// convLogSend logs the final Claude response.
func convLogSend(text string) {
	arrow := color(ansiBold+ansiGreen, "◀")
	label := color(ansiBold, "CLAUDE")
	trimmed := strings.TrimSpace(text)
	if len(trimmed) > 1000 {
		trimmed = trimmed[:997] + "…"
	}
	fmt.Fprintf(os.Stderr, "%s %s %s\n%s\n",
		ts(), arrow, label, indent(trimmed))
}

// convLogError logs a terminal agent error.
func convLogError(convID string, err error) {
	icon := color(ansiRed, "✗")
	fmt.Fprintf(os.Stderr, "%s %s conv=%s  %v\n", ts(), icon, convID, err)
}
