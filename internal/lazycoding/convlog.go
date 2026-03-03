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
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiGray   = "\033[90m"
	ansiCyan   = "\033[36m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiRed    = "\033[31m"
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

func ts() string {
	return color(ansiGray, time.Now().Format("15:04:05"))
}

// indent adds "  " before each line of s.
func indent(s string) string {
	s = strings.TrimRight(s, "\n")
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}

// convLogRecv logs an incoming user message.
func convLogRecv(convID, userKey, text string) {
	arrow := color(ansiBold+ansiCyan, "▶")
	meta := color(ansiGray, fmt.Sprintf("conv=%s  %s", convID, userKey))
	fmt.Fprintf(os.Stderr, "\n%s %s %s\n%s\n",
		ts(), arrow, meta, indent(text))
}

// convLogTool logs a tool invocation with a human-readable summary.
func convLogTool(name, input, workDir string) {
	label := color(ansiYellow, "🔧 "+name)
	summary := formatToolInput(name, input, workDir)
	if summary != "" {
		fmt.Fprintf(os.Stderr, "%s   %s  %s\n", ts(), label, color(ansiGray, summary))
	} else {
		fmt.Fprintf(os.Stderr, "%s   %s\n", ts(), label)
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

// formatToolInput extracts a readable summary from a tool's JSON input.
func formatToolInput(toolName, input, workDir string) string {
	if input == "" {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(input), &m); err != nil {
		if len(input) > 100 {
			return input[:97] + "…"
		}
		return input
	}

	getString := func(key string) string {
		raw, ok := m[key]
		if !ok {
			return ""
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return strings.Trim(string(raw), `"`)
		}
		return s
	}

	switch toolName {
	case "Read", "Write", "Edit", "NotebookEdit":
		p := getString("file_path")
		if p == "" {
			p = getString("notebook_path")
		}
		return shortenPath(p, workDir)

	case "Bash":
		cmd := getString("command")
		if len(cmd) > 200 {
			cmd = cmd[:197] + "…"
		}
		return cmd

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
		url := getString("url")
		if len(url) > 120 {
			url = url[:117] + "…"
		}
		return url

	case "WebSearch":
		return getString("query")

	case "Task":
		desc := getString("description")
		if len(desc) > 120 {
			desc = desc[:117] + "…"
		}
		return desc

	case "AskUserQuestion":
		raw, ok := m["questions"]
		if ok {
			var questions []struct {
				Question string `json:"question"`
			}
			if err := json.Unmarshal(raw, &questions); err == nil && len(questions) > 0 {
				q := questions[0].Question
				if len(q) > 120 {
					q = q[:117] + "…"
				}
				return q
			}
		}

	case "TodoWrite":
		raw, ok := m["todos"]
		if ok {
			var todos []any
			if err := json.Unmarshal(raw, &todos); err == nil {
				return fmt.Sprintf("(%d todos)", len(todos))
			}
		}
	}

	// fallback: truncated raw JSON
	if len(input) > 160 {
		return input[:157] + "…"
	}
	return input
}

// convLogSend logs the final Claude response.
func convLogSend(text string) {
	arrow := color(ansiBold+ansiGreen, "◀")
	label := color(ansiBold, "CLAUDE")
	trimmed := strings.TrimSpace(text)
	if len(trimmed) > 2000 {
		trimmed = trimmed[:1997] + "…"
	}
	fmt.Fprintf(os.Stderr, "%s %s %s\n%s\n",
		ts(), arrow, label, indent(trimmed))
}

// convLogError logs a terminal agent error.
func convLogError(convID string, err error) {
	icon := color(ansiRed, "✗")
	fmt.Fprintf(os.Stderr, "%s %s conv=%s  %v\n", ts(), icon, convID, err)
}
