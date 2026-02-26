package prompt

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed system_prompt.md
var StaticSystemPrompt string

// ContextSection is an XML-tagged block of dynamic context.
type ContextSection struct {
	Tag     string
	Content string
}

// DynamicContext holds per-session/per-request context sections.
type DynamicContext struct {
	Sections []ContextSection
}

// AddSection appends a context section. Empty content is skipped.
func (dc *DynamicContext) AddSection(tag, content string) {
	if strings.TrimSpace(content) == "" {
		return
	}
	dc.Sections = append(dc.Sections, ContextSection{Tag: tag, Content: content})
}

// Build renders all sections as XML-tagged blocks.
func (dc *DynamicContext) Build() string {
	if len(dc.Sections) == 0 {
		return ""
	}
	var b strings.Builder
	for _, s := range dc.Sections {
		fmt.Fprintf(&b, "<%s>\n%s\n</%s>\n\n", s.Tag, s.Content, s.Tag)
	}
	return strings.TrimRight(b.String(), "\n")
}

// SystemPromptBuilder assembles system prompt messages.
type SystemPromptBuilder struct {
	StaticPrompt   string         // Part 1: cacheable, never changes
	DynamicContext *DynamicContext // Part 2: per-session context
}

// NewSystemPromptBuilder returns a builder initialized with the embedded default prompt.
func NewSystemPromptBuilder() *SystemPromptBuilder {
	return &SystemPromptBuilder{
		StaticPrompt:   StaticSystemPrompt,
		DynamicContext: &DynamicContext{},
	}
}

// SetStaticPrompt replaces the built-in static prompt entirely.
// This is the override mechanism for SDK users.
func (b *SystemPromptBuilder) SetStaticPrompt(s string) {
	b.StaticPrompt = s
}

// BuildMessages returns ordered system prompt texts as separate strings.
// Each string becomes its own protocol.Message with RoleSystem.
// Order: [static] → [dynamic context]
// Skill injection is handled separately by the agent.
func (b *SystemPromptBuilder) BuildMessages() []string {
	var msgs []string
	if s := strings.TrimSpace(b.StaticPrompt); s != "" {
		msgs = append(msgs, s)
	}
	if b.DynamicContext != nil {
		if d := b.DynamicContext.Build(); d != "" {
			msgs = append(msgs, d)
		}
	}
	return msgs
}

// UserPromptWrapper wraps raw user input with context.
type UserPromptWrapper struct {
	ContextSections []ContextSection
}

// NewUserPromptWrapper creates an empty wrapper.
func NewUserPromptWrapper() *UserPromptWrapper {
	return &UserPromptWrapper{}
}

// AddContext appends a context section. Empty content is skipped.
func (w *UserPromptWrapper) AddContext(tag, content string) {
	if strings.TrimSpace(content) == "" {
		return
	}
	w.ContextSections = append(w.ContextSections, ContextSection{Tag: tag, Content: content})
}

// Wrap returns rawInput wrapped with <user_query> tags and interleaved context.
// Returns rawInput unchanged if no context sections are present.
func (w *UserPromptWrapper) Wrap(rawInput string) string {
	if len(w.ContextSections) == 0 {
		return rawInput
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<user_query>\n%s\n</user_query>", rawInput)
	for _, s := range w.ContextSections {
		fmt.Fprintf(&b, "\n\n<%s>\n%s\n</%s>", s.Tag, s.Content, s.Tag)
	}
	return b.String()
}
