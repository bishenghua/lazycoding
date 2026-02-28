package telegram

import (
	"testing"
)

func TestEscapeHTML(t *testing.T) {
	cases := []struct{ in, want string }{
		{`pwd && ls`, `pwd &amp;&amp; ls`},
		{`{"key":"value"}`, `{&quot;key&quot;:&quot;value&quot;}`},
		{`a < b > c`, `a &lt; b &gt; c`},
		{`it's fine`, `it's fine`}, // single quote: no escaping needed
		{`a & b < c > "d"`, `a &amp; b &lt; c &gt; &quot;d&quot;`},
	}
	for _, c := range cases {
		got := EscapeHTML(c.in)
		if got != c.want {
			t.Errorf("EscapeHTML(%q)\n got:  %q\n want: %q", c.in, got, c.want)
		}
	}
}

func TestMarkdownToTelegramHTML(t *testing.T) {
	cases := []struct {
		name string
		md   string
		want string
	}{
		// Headers
		{"h1", "# Title", "<b>Title</b>"},
		{"h2", "## Section", "<b>Section</b>"},
		{"h3", "### Sub", "<b>Sub</b>"},

		// Bold / italic
		{"bold star", "**bold**", "<b>bold</b>"},
		{"bold under", "__bold__", "<b>bold</b>"},
		{"italic", "*em*", "<i>em</i>"},

		// Strikethrough
		{"strike", "~~deleted~~", "<s>deleted</s>"},

		// Links
		{"link", "[Go](https://golang.org)", `<a href="https://golang.org">Go</a>`},
		{"link bold", "[**Go**](https://go.dev)", `<a href="https://go.dev"><b>Go</b></a>`},

		// Inline code
		{"inline code", "`fmt.Println`", "<code>fmt.Println</code>"},
		{"inline code html", "`a < b`", "<code>a &lt; b</code>"},

		// Blockquote
		{"blockquote", "> wise words", "<blockquote>wise words</blockquote>"},

		// Bullet lists
		{"bullet dash", "- item", "• item"},
		{"bullet star", "* item", "• item"},
		{"bullet indent", "  - nested", "  • nested"},

		// HTML escaping in plain text
		{"escape amp", "a & b", "a &amp; b"},
		{"escape lt gt", "x < y > z", "x &lt; y &gt; z"},
		{"escape quote", `say "hi"`, `say &quot;hi&quot;`},

		// Fenced code block
		{
			"fenced code",
			"```\nfmt.Println(\"hi\")\n```",
			"<pre><code>fmt.Println(&quot;hi&quot;)</code></pre>",
		},
		{
			"fenced code lang",
			"```go\nvar x = 1\n```",
			`<pre><code class="language-go">var x = 1</code></pre>`,
		},

		// Mixed
		{
			"mixed",
			"## Header\n\n**bold** and *italic* and ~~strike~~",
			"<b>Header</b>\n\n<b>bold</b> and <i>italic</i> and <s>strike</s>",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MarkdownToTelegramHTML(c.md)
			if got != c.want {
				t.Errorf("\n got:  %q\n want: %q", got, c.want)
			}
		})
	}
}

func TestMarkdownTable(t *testing.T) {
	md := "| Name    | Age | City     |\n" +
		"|---------|-----|----------|\n" +
		"| Alice   | 30  | New York |\n" +
		"| Bob     | 25  | London   |\n" +
		"| Charlie | 35  | 北京     |"

	got := MarkdownToTelegramHTML(md)

	// Must be wrapped in <pre><code>
	if got[:len("<pre><code>")] != "<pre><code>" {
		t.Fatalf("expected <pre><code> prefix, got: %q", got[:20])
	}
	// Must contain box-drawing characters
	for _, ch := range []string{"┌", "│", "├", "└", "─"} {
		if !contains(got, ch) {
			t.Errorf("table missing box char %q", ch)
		}
	}
	// Header row must appear before data rows
	alicePos := index(got, "Alice")
	headerPos := index(got, "Name")
	if headerPos < 0 || alicePos < 0 || headerPos > alicePos {
		t.Error("header row should appear before data rows")
	}
}

func TestTablePipeInCode(t *testing.T) {
	// | inside backtick span must NOT be treated as a column separator
	md := "| Feature | Type |\n" +
		"|---------|------|\n" +
		"| Constraint | `~int | ~float64` |"

	got := MarkdownToTelegramHTML(md)

	// The table should have 2 data columns, not 3+
	// Verify the pipe inside backticks didn't create an extra column
	if !contains(got, "~int | ~float64") {
		t.Errorf("pipe inside backtick span was incorrectly split as column separator\ngot: %s", got)
	}
}

func TestTableCellDisplayWidth(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"hello", 5},
		{"北京", 4},   // 2 CJK chars × 2 = 4
		{"Go语言", 6}, // 2 ASCII + 2 CJK×2 = 6
	}
	for _, c := range cases {
		got := cellDisplayWidth(c.s)
		if got != c.want {
			t.Errorf("cellDisplayWidth(%q) = %d, want %d", c.s, got, c.want)
		}
	}
}

// helpers

func contains(s, sub string) bool { return len(s) >= len(sub) && index(s, sub) >= 0 }

func index(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
