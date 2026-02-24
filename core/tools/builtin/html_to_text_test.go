package builtin

import (
	"strings"
	"testing"
)

func TestHTMLToPlainText(t *testing.T) {
	tests := []struct {
		name     string
		html     string
		contains []string
		excludes []string
	}{
		{
			name:     "basic text extraction",
			html:     `<html><body><p>Hello World</p></body></html>`,
			contains: []string{"Hello World"},
		},
		{
			name:     "strips script and style",
			html:     `<html><body><p>Visible</p><script>alert('hidden')</script><style>body{}</style></body></html>`,
			contains: []string{"Visible"},
			excludes: []string{"alert", "body{}"},
		},
		{
			name:     "preserves paragraph breaks",
			html:     `<html><body><p>Para 1</p><p>Para 2</p></body></html>`,
			contains: []string{"Para 1", "Para 2"},
		},
		{
			name:     "handles br tags",
			html:     `<html><body><p>Line 1<br>Line 2</p></body></html>`,
			contains: []string{"Line 1", "Line 2"},
		},
		{
			name:     "empty input",
			html:     ``,
			contains: []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := htmlToPlainText([]byte(tc.html))
			for _, want := range tc.contains {
				if !strings.Contains(result, want) {
					t.Errorf("expected output to contain %q, got: %q", want, result)
				}
			}
			for _, unwanted := range tc.excludes {
				if strings.Contains(result, unwanted) {
					t.Errorf("expected output NOT to contain %q, got: %q", unwanted, result)
				}
			}
		})
	}
}

func TestHTMLToReadableText(t *testing.T) {
	t.Run("headings formatted as markdown", func(t *testing.T) {
		html := `<html><body><h1>Title</h1><h2>Subtitle</h2><p>Body text.</p></body></html>`
		result := htmlToReadableText([]byte(html))
		if !strings.Contains(result, "# Title") {
			t.Errorf("expected '# Title' in output, got: %q", result)
		}
		if !strings.Contains(result, "## Subtitle") {
			t.Errorf("expected '## Subtitle' in output, got: %q", result)
		}
		if !strings.Contains(result, "Body text.") {
			t.Errorf("expected 'Body text.' in output, got: %q", result)
		}
	})

	t.Run("links formatted as markdown", func(t *testing.T) {
		html := `<html><body><p>Visit <a href="https://example.com">Example</a> for more.</p></body></html>`
		result := htmlToReadableText([]byte(html))
		if !strings.Contains(result, "[Example](https://example.com)") {
			t.Errorf("expected markdown link in output, got: %q", result)
		}
	})

	t.Run("list items formatted", func(t *testing.T) {
		html := `<html><body><ul><li>First</li><li>Second</li><li>Third</li></ul></body></html>`
		result := htmlToReadableText([]byte(html))
		if !strings.Contains(result, "- First") {
			t.Errorf("expected '- First' in output, got: %q", result)
		}
		if !strings.Contains(result, "- Second") {
			t.Errorf("expected '- Second' in output, got: %q", result)
		}
	})

	t.Run("code blocks with fences", func(t *testing.T) {
		html := `<html><body><pre><code class="language-go">fmt.Println("hello")</code></pre></body></html>`
		result := htmlToReadableText([]byte(html))
		if !strings.Contains(result, "```go") {
			t.Errorf("expected fenced code block with language, got: %q", result)
		}
		if !strings.Contains(result, `fmt.Println("hello")`) {
			t.Errorf("expected code content in output, got: %q", result)
		}
		if !strings.Contains(result, "```") {
			t.Errorf("expected closing fence in output, got: %q", result)
		}
	})

	t.Run("inline code", func(t *testing.T) {
		html := `<html><body><p>Use <code>foo()</code> to call it.</p></body></html>`
		result := htmlToReadableText([]byte(html))
		if !strings.Contains(result, "`foo()`") {
			t.Errorf("expected inline code in output, got: %q", result)
		}
	})

	t.Run("strips chrome elements", func(t *testing.T) {
		html := `<html><body><nav>Menu</nav><main><p>Content</p></main><footer>Copyright</footer></body></html>`
		result := htmlToReadableText([]byte(html))
		if strings.Contains(result, "Menu") {
			t.Errorf("expected nav content to be stripped, got: %q", result)
		}
		if strings.Contains(result, "Copyright") {
			t.Errorf("expected footer content to be stripped, got: %q", result)
		}
		if !strings.Contains(result, "Content") {
			t.Errorf("expected main content to be present, got: %q", result)
		}
	})

	t.Run("prefers main/article as content root", func(t *testing.T) {
		html := `<html><body>
			<nav>Navigation</nav>
			<aside>Sidebar</aside>
			<article><h1>Article Title</h1><p>Article body.</p></article>
			<footer>Footer</footer>
		</body></html>`
		result := htmlToReadableText([]byte(html))
		if !strings.Contains(result, "Article Title") {
			t.Errorf("expected article content, got: %q", result)
		}
		if strings.Contains(result, "Navigation") {
			t.Errorf("expected nav to be excluded, got: %q", result)
		}
	})

	t.Run("bold and italic", func(t *testing.T) {
		html := `<html><body><p>This is <strong>bold</strong> and <em>italic</em>.</p></body></html>`
		result := htmlToReadableText([]byte(html))
		if !strings.Contains(result, "**bold**") {
			t.Errorf("expected **bold** in output, got: %q", result)
		}
		if !strings.Contains(result, "*italic*") {
			t.Errorf("expected *italic* in output, got: %q", result)
		}
	})

	t.Run("image alt text", func(t *testing.T) {
		html := `<html><body><p>Look: <img src="photo.jpg" alt="A cat"></p></body></html>`
		result := htmlToReadableText([]byte(html))
		if !strings.Contains(result, "[image: A cat]") {
			t.Errorf("expected image alt text, got: %q", result)
		}
	})

	t.Run("skips javascript links", func(t *testing.T) {
		html := `<html><body><a href="javascript:void(0)">Click</a></body></html>`
		result := htmlToReadableText([]byte(html))
		if strings.Contains(result, "javascript:") {
			t.Errorf("expected javascript links to be stripped, got: %q", result)
		}
		if !strings.Contains(result, "Click") {
			t.Errorf("expected link text to remain, got: %q", result)
		}
	})
}

func TestCollapseWhitespace(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hello   world", "hello world"},
		{"  leading", " leading"},
		{"trailing  ", "trailing "},
		{"\n\t  mixed\n\n", " mixed "},
		{"no-change", "no-change"},
	}

	for _, tc := range tests {
		result := collapseWhitespace(tc.input)
		if result != tc.expected {
			t.Errorf("collapseWhitespace(%q) = %q, want %q", tc.input, result, tc.expected)
		}
	}
}

func TestCollapseBlankLines(t *testing.T) {
	input := "line1\n\n\n\n\nline2\n\n\nline3"
	result := collapseBlankLines(input)
	if strings.Contains(result, "\n\n\n") {
		t.Errorf("expected no triple newlines, got: %q", result)
	}
	if !strings.Contains(result, "line1\n\nline2") {
		t.Errorf("expected double newlines preserved, got: %q", result)
	}
}
