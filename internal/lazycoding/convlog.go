package lazycoding

import (
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

// convLogTool logs a tool invocation.
func convLogTool(name, input string) {
	label := color(ansiYellow, "🔧 "+name)
	if input != "" {
		if len(input) > 120 {
			input = input[:117] + "…"
		}
		fmt.Fprintf(os.Stderr, "%s   %s  %s\n", ts(), label, input)
	} else {
		fmt.Fprintf(os.Stderr, "%s   %s\n", ts(), label)
	}
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
