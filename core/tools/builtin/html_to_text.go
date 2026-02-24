package builtin

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// skipTags are elements whose text content should be omitted entirely.
var skipTags = map[atom.Atom]bool{
	atom.Script:   true,
	atom.Style:    true,
	atom.Noscript: true,
	atom.Svg:      true,
}

// chromeTags are elements removed in readable mode (navigation chrome).
var chromeTags = map[atom.Atom]bool{
	atom.Nav:    true,
	atom.Header: true,
	atom.Footer: true,
	atom.Aside:  true,
}

// blockTags produce line breaks before/after their content.
var blockTags = map[atom.Atom]bool{
	atom.P:          true,
	atom.Div:        true,
	atom.Section:    true,
	atom.Article:    true,
	atom.Main:       true,
	atom.Blockquote: true,
	atom.Ul:         true,
	atom.Ol:         true,
	atom.Table:      true,
	atom.Tr:         true,
	atom.Dt:         true,
	atom.Dd:         true,
	atom.Figure:     true,
	atom.Figcaption: true,
}

// htmlToPlainText extracts all visible text from HTML, preserving paragraph structure.
func htmlToPlainText(data []byte) string {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return string(data)
	}

	var sb strings.Builder
	plainWalk(doc, &sb)

	return collapseBlankLines(sb.String())
}

func plainWalk(n *html.Node, sb *strings.Builder) {
	if n.Type == html.ElementNode && skipTags[n.DataAtom] {
		return
	}

	if n.Type == html.TextNode {
		text := strings.TrimSpace(n.Data)
		if text != "" {
			sb.WriteString(text)
			sb.WriteByte(' ')
		}
	}

	// Add line breaks around block elements.
	isBlock := n.Type == html.ElementNode && blockTags[n.DataAtom]
	if isBlock {
		sb.WriteByte('\n')
	}

	if n.Type == html.ElementNode && n.DataAtom == atom.Br {
		sb.WriteByte('\n')
	}

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		plainWalk(c, sb)
	}

	if isBlock {
		sb.WriteByte('\n')
	}
}

// htmlToReadableText converts HTML into a human-friendly text format.
// It tries to find <main> or <article> as the primary content area,
// strips chrome elements, and formats headings, links, lists, and code blocks.
func htmlToReadableText(data []byte) string {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return string(data)
	}

	// Try to locate <main> or <article> as the content root.
	root := findContentRoot(doc)
	if root == nil {
		root = findBody(doc)
	}
	if root == nil {
		root = doc
	}

	var sb strings.Builder
	readableWalk(root, &sb, false)

	return collapseBlankLines(sb.String())
}

// findContentRoot searches for <main> or <article> in the tree.
func findContentRoot(n *html.Node) *html.Node {
	if n.Type == html.ElementNode {
		if n.DataAtom == atom.Main || n.DataAtom == atom.Article {
			return n
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findContentRoot(c); found != nil {
			return found
		}
	}
	return nil
}

// findBody returns the <body> element if present.
func findBody(n *html.Node) *html.Node {
	if n.Type == html.ElementNode && n.DataAtom == atom.Body {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findBody(c); found != nil {
			return found
		}
	}
	return nil
}

func readableWalk(n *html.Node, sb *strings.Builder, inPre bool) {
	if n.Type == html.ElementNode {
		// Skip script/style.
		if skipTags[n.DataAtom] {
			return
		}
		// Skip chrome.
		if chromeTags[n.DataAtom] {
			return
		}
	}

	if n.Type == html.TextNode {
		text := n.Data
		if !inPre {
			text = collapseWhitespace(text)
		}
		if text != "" {
			sb.WriteString(text)
		}
		return
	}

	if n.Type != html.ElementNode {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			readableWalk(c, sb, inPre)
		}
		return
	}

	switch n.DataAtom {
	case atom.H1:
		sb.WriteString("\n\n# ")
		walkChildren(n, sb, false)
		sb.WriteString("\n\n")
	case atom.H2:
		sb.WriteString("\n\n## ")
		walkChildren(n, sb, false)
		sb.WriteString("\n\n")
	case atom.H3:
		sb.WriteString("\n\n### ")
		walkChildren(n, sb, false)
		sb.WriteString("\n\n")
	case atom.H4:
		sb.WriteString("\n\n#### ")
		walkChildren(n, sb, false)
		sb.WriteString("\n\n")
	case atom.H5:
		sb.WriteString("\n\n##### ")
		walkChildren(n, sb, false)
		sb.WriteString("\n\n")
	case atom.H6:
		sb.WriteString("\n\n###### ")
		walkChildren(n, sb, false)
		sb.WriteString("\n\n")

	case atom.P, atom.Div, atom.Section, atom.Blockquote:
		sb.WriteString("\n\n")
		walkChildren(n, sb, false)
		sb.WriteString("\n\n")

	case atom.Br:
		sb.WriteByte('\n')

	case atom.Li:
		sb.WriteString("\n- ")
		walkChildren(n, sb, false)

	case atom.Ul, atom.Ol:
		sb.WriteByte('\n')
		walkChildren(n, sb, false)
		sb.WriteByte('\n')

	case atom.A:
		href := getAttr(n, "href")
		var linkText strings.Builder
		walkChildren(n, &linkText, false)
		text := strings.TrimSpace(linkText.String())
		if text == "" {
			text = href
		}
		if href != "" && !strings.HasPrefix(href, "#") && !strings.HasPrefix(href, "javascript:") {
			sb.WriteString("[")
			sb.WriteString(text)
			sb.WriteString("](")
			sb.WriteString(href)
			sb.WriteString(")")
		} else {
			sb.WriteString(text)
		}

	case atom.Pre:
		// Check if contains <code>.
		codeNode := findChildByTag(n, atom.Code)
		lang := ""
		if codeNode != nil {
			lang = detectCodeLang(codeNode)
		}
		sb.WriteString("\n\n```")
		sb.WriteString(lang)
		sb.WriteByte('\n')
		if codeNode != nil {
			walkChildren(codeNode, sb, true)
		} else {
			walkChildren(n, sb, true)
		}
		sb.WriteString("\n```\n\n")

	case atom.Code:
		if !inPre {
			sb.WriteByte('`')
			walkChildren(n, sb, false)
			sb.WriteByte('`')
		} else {
			walkChildren(n, sb, true)
		}

	case atom.Strong, atom.B:
		sb.WriteString("**")
		walkChildren(n, sb, inPre)
		sb.WriteString("**")

	case atom.Em, atom.I:
		sb.WriteString("*")
		walkChildren(n, sb, inPre)
		sb.WriteString("*")

	case atom.Img:
		alt := getAttr(n, "alt")
		if alt != "" {
			sb.WriteString("[image: ")
			sb.WriteString(alt)
			sb.WriteString("]")
		}

	case atom.Table:
		sb.WriteString("\n\n")
		walkChildren(n, sb, false)
		sb.WriteString("\n\n")

	case atom.Tr:
		sb.WriteString("| ")
		walkChildren(n, sb, false)
		sb.WriteByte('\n')

	case atom.Td, atom.Th:
		walkChildren(n, sb, false)
		sb.WriteString(" | ")

	default:
		walkChildren(n, sb, inPre)
	}
}

func walkChildren(n *html.Node, sb *strings.Builder, inPre bool) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		readableWalk(c, sb, inPre)
	}
}

func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func findChildByTag(n *html.Node, tag atom.Atom) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.DataAtom == tag {
			return c
		}
	}
	return nil
}

// detectCodeLang attempts to extract a language hint from a <code> element's class
// (e.g., class="language-go" or class="hljs-python").
func detectCodeLang(n *html.Node) string {
	cls := getAttr(n, "class")
	if cls == "" {
		return ""
	}
	for _, part := range strings.Fields(cls) {
		if strings.HasPrefix(part, "language-") {
			return strings.TrimPrefix(part, "language-")
		}
		if strings.HasPrefix(part, "lang-") {
			return strings.TrimPrefix(part, "lang-")
		}
	}
	return ""
}

// collapseWhitespace replaces sequences of whitespace with a single space.
func collapseWhitespace(s string) string {
	var sb strings.Builder
	prevSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !prevSpace {
				sb.WriteByte(' ')
				prevSpace = true
			}
		} else {
			sb.WriteRune(r)
			prevSpace = false
		}
	}
	return sb.String()
}

// collapseBlankLines reduces runs of 3+ newlines to 2, and trims leading/trailing space.
func collapseBlankLines(s string) string {
	// Replace 3+ consecutive newlines with 2.
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(s)
}
