package telegram

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const MaxMessageLen = 4096

// EscapeHTML escapes s for use in Telegram HTML-formatted messages.
// Unlike EscapeHTML, it only uses the four named entities that
// Telegram's HTML parser recognises (&amp; &lt; &gt; &quot;).
// Numeric entities such as &#34; or &#39; are NOT supported by Telegram
// and will cause the API to reject the message's parse mode.
func EscapeHTML(s string) string {
	// & must be replaced first to avoid double-escaping.
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}

// MarkdownToTelegramHTML converts a subset of standard Markdown to
// Telegram-compatible HTML (https://core.telegram.org/bots/api#html-style).
// Handled constructs:
//   - Fenced code blocks (``` … ```) → <pre><code>…</code></pre>
//   - Markdown tables               → <pre><code> Unicode box-drawing </code></pre>
//   - ATX headers (# / ## / ###)   → <b>…</b>
//   - Bold  **text** or __text__   → <b>text</b>
//   - Italic *text*                → <i>text</i>
//   - Inline code `text`           → <code>text</code>
//   - Bullet list items (- / *)    → • …
func MarkdownToTelegramHTML(md string) string {
	if md == "" {
		return ""
	}
	lines := strings.Split(md, "\n")
	var out strings.Builder
	inCode := false
	codeLang := ""
	var codeBuf []string

	i := 0
	for i < len(lines) {
		line := lines[i]

		// ── Inside a fenced code block ──────────────────────────────────
		if inCode {
			if strings.HasPrefix(line, "```") {
				inCode = false
				escaped := EscapeHTML(strings.Join(codeBuf, "\n"))
				if codeLang != "" {
					out.WriteString("<pre><code class=\"language-" +
						EscapeHTML(codeLang) + "\">" + escaped + "</code></pre>\n")
				} else {
					out.WriteString("<pre><code>" + escaped + "</code></pre>\n")
				}
				codeLang = ""
			} else {
				codeBuf = append(codeBuf, line)
			}
			i++
			continue
		}

		// ── Fenced code block start ──────────────────────────────────────
		if strings.HasPrefix(line, "```") {
			inCode = true
			codeLang = strings.TrimSpace(strings.TrimPrefix(line, "```"))
			codeBuf = nil
			i++
			continue
		}

		// ── Markdown table ───────────────────────────────────────────────
		// A table is a contiguous run of lines that start and end with "|".
		if isTableRow(line) {
			j := i
			for j < len(lines) && isTableRow(lines[j]) {
				j++
			}
			if rendered := renderTable(lines[i:j]); rendered != "" {
				out.WriteString("<pre><code>")
				out.WriteString(EscapeHTML(rendered))
				out.WriteString("</code></pre>\n")
			}
			i = j
			continue
		}

		// ── Regular line ─────────────────────────────────────────────────
		out.WriteString(transformLine(line))
		out.WriteByte('\n')
		i++
	}

	// Unclosed fenced block.
	if inCode && len(codeBuf) > 0 {
		out.WriteString("<pre><code>" + EscapeHTML(strings.Join(codeBuf, "\n")) + "</code></pre>\n")
	}
	return strings.TrimRight(out.String(), "\n")
}

// ── Table helpers ─────────────────────────────────────────────────────────────

// isTableRow reports whether a line looks like a Markdown table row:
// trimmed content starts and ends with "|".
func isTableRow(line string) bool {
	t := strings.TrimSpace(line)
	return len(t) >= 2 && t[0] == '|' && t[len(t)-1] == '|'
}

// isSeparatorRow reports whether line is a Markdown table separator
// (e.g. |---|:---:|---:|).
func isSeparatorRow(line string) bool {
	t := strings.TrimSpace(line)
	if len(t) < 2 || t[0] != '|' || t[len(t)-1] != '|' {
		return false
	}
	inner := t[1 : len(t)-1]
	for _, cell := range strings.Split(inner, "|") {
		cell = strings.TrimSpace(cell)
		if cell == "" {
			return false
		}
		if strings.Trim(cell, "-:") != "" {
			return false
		}
	}
	return true
}

// parseTableCells splits a Markdown table row into trimmed cell strings.
// It correctly treats | inside backtick code spans as literal characters,
// not column separators (e.g. `~int | ~float64` stays as one cell).
func parseTableCells(line string) []string {
	t := strings.TrimSpace(line)
	t = t[1 : len(t)-1] // strip leading/trailing "|"

	var cells []string
	inCode := false
	start := 0
	for i := 0; i < len(t); i++ {
		switch t[i] {
		case '`':
			inCode = !inCode
		case '|':
			if !inCode {
				cells = append(cells, strings.TrimSpace(t[start:i]))
				start = i + 1
			}
		}
	}
	cells = append(cells, strings.TrimSpace(t[start:]))
	return cells
}

// cellDisplayWidth estimates the visual column width of s, treating CJK and
// most emoji as double-width characters.
func cellDisplayWidth(s string) int {
	w := 0
	for _, r := range s {
		if isWideRune(r) {
			w += 2
		} else {
			w++
		}
	}
	return w
}

// isWideRune returns true for characters that occupy two terminal columns.
func isWideRune(r rune) bool {
	return (r >= 0x1100 && r <= 0x115F) || // Hangul Jamo
		r == 0x2329 || r == 0x232A ||
		(r >= 0x2E80 && r <= 0x303E) || // CJK Radicals – CJK Symbols
		(r >= 0x3040 && r <= 0x33FF) || // Japanese kana + CJK compat
		(r >= 0x3400 && r <= 0x4DBF) || // CJK Extension A
		(r >= 0x4E00 && r <= 0xA4CF) || // CJK Unified Ideographs
		(r >= 0xA960 && r <= 0xA97F) ||
		(r >= 0xAC00 && r <= 0xD7FF) || // Hangul Syllables
		(r >= 0xF900 && r <= 0xFAFF) || // CJK Compat Ideographs
		(r >= 0xFE10 && r <= 0xFE6F) || // CJK Compatibility Forms
		(r >= 0xFF01 && r <= 0xFF60) || // Fullwidth Forms
		(r >= 0xFFE0 && r <= 0xFFE6) ||
		(r >= 0x1F300 && r <= 0x1F9FF) || // Emoji
		(r >= 0x20000 && r <= 0x3FFFD) // CJK Extension B-F
}

// padRight appends spaces so that the visual width of s reaches width.
func padRight(s string, width int) string {
	pad := width - cellDisplayWidth(s)
	if pad <= 0 {
		return s
	}
	return s + strings.Repeat(" ", pad)
}

// renderTable converts a slice of Markdown table lines (including the
// separator row) into a Unicode box-drawing ASCII table string.
// Returns "" if the input cannot be parsed as a valid table.
func renderTable(tableLines []string) string {
	if len(tableLines) == 0 {
		return ""
	}

	// Parse rows, skipping separator lines.
	var rows [][]string
	for _, line := range tableLines {
		if isSeparatorRow(line) {
			continue
		}
		rows = append(rows, parseTableCells(line))
	}
	if len(rows) == 0 {
		return ""
	}

	// Determine column count.
	numCols := 0
	for _, row := range rows {
		if len(row) > numCols {
			numCols = len(row)
		}
	}
	if numCols == 0 {
		return ""
	}

	// Compute maximum visual width per column.
	colW := make([]int, numCols)
	for _, row := range rows {
		for j := 0; j < numCols; j++ {
			cell := ""
			if j < len(row) {
				cell = row[j]
			}
			if w := cellDisplayWidth(cell); w > colW[j] {
				colW[j] = w
			}
		}
	}
	// Minimum column width of 1.
	for j := range colW {
		if colW[j] < 1 {
			colW[j] = 1
		}
	}

	var sb strings.Builder

	hRule := func(left, mid, right, fill string) {
		sb.WriteString(left)
		for j, w := range colW {
			sb.WriteString(strings.Repeat(fill, w+2))
			if j < len(colW)-1 {
				sb.WriteString(mid)
			}
		}
		sb.WriteString(right)
		sb.WriteByte('\n')
	}

	writeRow := func(row []string) {
		sb.WriteString("│")
		for j := 0; j < numCols; j++ {
			cell := ""
			if j < len(row) {
				cell = row[j]
			}
			sb.WriteByte(' ')
			sb.WriteString(padRight(cell, colW[j]))
			sb.WriteString(" │")
		}
		sb.WriteByte('\n')
	}

	hRule("┌", "┬", "┐", "─")
	writeRow(rows[0]) // header
	if len(rows) > 1 {
		hRule("├", "┼", "┤", "─")
		for _, row := range rows[1:] {
			writeRow(row)
		}
	}
	hRule("└", "┴", "┘", "─")

	return strings.TrimRight(sb.String(), "\n")
}

// ── Inline Markdown helpers ───────────────────────────────────────────────────

var (
	reBold       = regexp.MustCompile(`\*\*([^*\n]+)\*\*|__([^_\n]+)__`)
	reItalic     = regexp.MustCompile(`\*([^*\n]+)\*`)
	reStrike     = regexp.MustCompile(`~~([^~\n]+)~~`)
	reLink       = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	reInlineCode = regexp.MustCompile("`([^`\n]+)`")
)

// transformLine converts one non-code Markdown line to Telegram HTML.
func transformLine(line string) string {
	// ATX headers.
	if strings.HasPrefix(line, "### ") {
		return "<b>" + transformInline(line[4:]) + "</b>"
	}
	if strings.HasPrefix(line, "## ") {
		return "<b>" + transformInline(line[3:]) + "</b>"
	}
	if strings.HasPrefix(line, "# ") {
		return "<b>" + transformInline(line[2:]) + "</b>"
	}
	// Blockquotes.
	trimmed := strings.TrimLeft(line, " \t")
	if strings.HasPrefix(trimmed, "> ") {
		return "<blockquote>" + transformInline(trimmed[2:]) + "</blockquote>"
	}
	// Bullet lists (with optional leading indent).
	if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
		indent := line[:len(line)-len(trimmed)]
		return indent + "• " + transformInline(trimmed[2:])
	}
	return transformInline(line)
}

// transformInline processes bold, italic, and inline-code within a single line.
func transformInline(s string) string {
	// Step 1 – extract inline code spans into placeholders.
	var codes []string
	s = reInlineCode.ReplaceAllStringFunc(s, func(m string) string {
		content := m[1 : len(m)-1]
		codes = append(codes, "<code>"+EscapeHTML(content)+"</code>")
		return fmt.Sprintf("\x01%d\x01", len(codes)-1)
	})

	// Step 2 – HTML-escape (< > & etc.).
	s = EscapeHTML(s)

	// Step 3 – apply bold, italic, strikethrough, links.
	s = reBold.ReplaceAllStringFunc(s, func(m string) string {
		inner := m[2 : len(m)-2]
		return "<b>" + inner + "</b>"
	})
	s = reItalic.ReplaceAllString(s, "<i>$1</i>")
	s = reStrike.ReplaceAllString(s, "<s>$1</s>")
	// Links: [text](url) → <a href="url">text</a>.
	// [ ] ( ) are not HTML-special so the pattern still matches after EscapeHTML.
	s = reLink.ReplaceAllString(s, `<a href="$2">$1</a>`)

	// Step 4 – restore inline code spans.
	for i, c := range codes {
		s = strings.ReplaceAll(s, fmt.Sprintf("\x01%d\x01", i), c)
	}
	return s
}

// ── Message size helpers ──────────────────────────────────────────────────────

// runeStartBoundary returns the largest i <= maxBytes such that text[i] begins
// a UTF-8 rune, preventing slices that cut multi-byte characters in half.
func runeStartBoundary(text string, maxBytes int) int {
	if maxBytes >= len(text) {
		return len(text)
	}
	for maxBytes > 0 && !utf8.RuneStart(text[maxBytes]) {
		maxBytes--
	}
	return maxBytes
}

// Split breaks text into chunks that fit within Telegram's message length limit.
// It prefers splitting at newlines and always respects UTF-8 rune boundaries.
func Split(text string) []string {
	if len(text) == 0 {
		return []string{"<i>(empty response)</i>"}
	}

	var chunks []string
	for len(text) > 0 {
		if len(text) <= MaxMessageLen {
			chunks = append(chunks, text)
			break
		}
		limit := runeStartBoundary(text, MaxMessageLen)
		cut := strings.LastIndex(text[:limit], "\n")
		if cut <= 0 {
			cut = limit
		}
		chunks = append(chunks, text[:cut])
		text = strings.TrimPrefix(text[cut:], "\n")
	}
	return chunks
}

// Truncate shortens text to maxLen bytes, appending "…" when truncation occurs.
// Always cuts at a valid UTF-8 rune boundary.
func Truncate(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	const ellipsis = "…" // 3 bytes in UTF-8
	cut := runeStartBoundary(text, maxLen-len(ellipsis))
	if cut < 0 {
		cut = 0
	}
	if idx := strings.LastIndex(text[:cut], "\n"); idx > 0 {
		cut = idx
	}
	return text[:cut] + ellipsis
}
