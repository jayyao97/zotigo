package prompt

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed system_prompt.md
var StaticSystemPrompt string

// ToolCallResult is a lightweight summary of an executed tool call.
// Used by ReminderProvider to make decisions based on tool execution.
type ToolCallResult struct {
	Name    string
	Result  string
	IsError bool
}

// ReminderProvider returns reminder text to inject after tool execution.
// Called with the current PromptContext and the tool results from this batch.
// Return empty string to skip injection.
type ReminderProvider func(PromptContext, []ToolCallResult) string

// ReminderBuilder collects ReminderProviders and builds the injection text.
type ReminderBuilder struct {
	Providers []ReminderProvider
}

// ReminderOption configures a ReminderBuilder during construction.
type ReminderOption func(*ReminderBuilder)

// WithReminderProvider returns a ReminderOption that appends a provider.
func WithReminderProvider(p ReminderProvider) ReminderOption {
	return func(rb *ReminderBuilder) {
		rb.Providers = append(rb.Providers, p)
	}
}

// NewReminderBuilder creates a ReminderBuilder with the given options.
func NewReminderBuilder(opts ...ReminderOption) *ReminderBuilder {
	rb := &ReminderBuilder{}
	for _, opt := range opts {
		opt(rb)
	}
	return rb
}

// Build calls all providers and returns the combined reminder text
// wrapped in <system-reminder> tags. Returns empty string if no
// provider produces output.
func (rb *ReminderBuilder) Build(ctx PromptContext, results []ToolCallResult) string {
	if len(rb.Providers) == 0 {
		return ""
	}
	var parts []string
	for _, rp := range rb.Providers {
		if s := strings.TrimSpace(rp(ctx, results)); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "\n\n<system-reminder>\n" +
		strings.Join(parts, "\n\n") + "\n</system-reminder>"
}

// PromptContext carries per-request data available to lazy providers.
type PromptContext struct {
	WorkDir   string
	SessionID string
	Platform  string // "darwin", "linux", "windows"
	Model     string // e.g. "claude-sonnet-4-20250514"
}

// ContextSection is an XML-tagged block of dynamic context.
// Provider is called lazily at Build/Wrap time with the current PromptContext.
type ContextSection struct {
	Tag        string
	Attributes string
	Provider   func(PromptContext) string
}

// DynamicContext holds per-session/per-request context sections.
type DynamicContext struct {
	Sections []ContextSection
}

// DynamicOption configures a DynamicContext during construction.
type DynamicOption func(*DynamicContext)

// WithSection returns a DynamicOption that appends a lazy context section.
func WithSection(tag string, provider func(PromptContext) string) DynamicOption {
	return func(dc *DynamicContext) {
		dc.Sections = append(dc.Sections, ContextSection{Tag: tag, Provider: provider})
	}
}

// WithAttributedSection appends a lazy context section whose opening tag carries
// pre-rendered attributes, for example `<project_instructions source="AGENTS.md">`.
func WithAttributedSection(tag, attributes string, provider func(PromptContext) string) DynamicOption {
	return func(dc *DynamicContext) {
		dc.Sections = append(dc.Sections, ContextSection{Tag: tag, Attributes: attributes, Provider: provider})
	}
}

// NewDynamicContext creates a DynamicContext with the given options.
func NewDynamicContext(opts ...DynamicOption) *DynamicContext {
	dc := &DynamicContext{}
	for _, opt := range opts {
		opt(dc)
	}
	return dc
}

// Build renders all sections as XML-tagged blocks.
// Providers are called lazily; empty results are skipped.
func (dc *DynamicContext) Build(ctx PromptContext) string {
	return renderContextSections(ctx, dc.Sections)
}

// SystemPromptBuilder assembles system prompt messages.
type SystemPromptBuilder struct {
	StaticPrompt   string          // Part 1: cacheable, never changes
	DynamicContext *DynamicContext // Part 2: per-session context
}

// SystemPromptOption configures a SystemPromptBuilder during construction.
type SystemPromptOption func(*SystemPromptBuilder)

// WithStaticPrompt returns a SystemPromptOption that replaces the default static prompt.
func WithStaticPrompt(s string) SystemPromptOption {
	return func(b *SystemPromptBuilder) { b.StaticPrompt = s }
}

// WithDynamicSection returns a SystemPromptOption that appends a lazy context section.
func WithDynamicSection(tag string, provider func(PromptContext) string) SystemPromptOption {
	return func(b *SystemPromptBuilder) {
		b.DynamicContext.Sections = append(b.DynamicContext.Sections, ContextSection{Tag: tag, Provider: provider})
	}
}

// NewSystemPromptBuilder returns a builder initialized with the embedded default prompt.
func NewSystemPromptBuilder(opts ...SystemPromptOption) *SystemPromptBuilder {
	b := &SystemPromptBuilder{
		StaticPrompt:   StaticSystemPrompt,
		DynamicContext: &DynamicContext{},
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
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
func (b *SystemPromptBuilder) BuildMessages(ctx PromptContext) []string {
	var msgs []string
	if s := strings.TrimSpace(b.StaticPrompt); s != "" {
		msgs = append(msgs, s)
	}
	if b.DynamicContext != nil {
		if d := b.DynamicContext.Build(ctx); d != "" {
			msgs = append(msgs, d)
		}
	}
	return msgs
}

// UserContextBuilder builds a transient user-context message.
type UserContextBuilder struct {
	ContextSections []ContextSection
}

// UserContextOption configures a UserContextBuilder during construction.
type UserContextOption func(*UserContextBuilder)

// WithContext returns a UserContextOption that appends a lazy context section.
func WithContext(tag string, provider func(PromptContext) string) UserContextOption {
	return func(w *UserContextBuilder) {
		w.ContextSections = append(w.ContextSections, ContextSection{Tag: tag, Provider: provider})
	}
}

// WithAttributedContext appends a lazy user-context section with attributes on
// the opening tag.
func WithAttributedContext(tag, attributes string, provider func(PromptContext) string) UserContextOption {
	return func(w *UserContextBuilder) {
		w.ContextSections = append(w.ContextSections, ContextSection{Tag: tag, Attributes: attributes, Provider: provider})
	}
}

// NewUserContextBuilder creates a builder with the given options.
func NewUserContextBuilder(opts ...UserContextOption) *UserContextBuilder {
	w := &UserContextBuilder{}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// Build renders all context sections as one meta user-context payload.
// Providers are called lazily; empty outputs are skipped.
func (w *UserContextBuilder) Build(ctx PromptContext) string {
	if len(w.ContextSections) == 0 {
		return ""
	}
	return renderContextSections(ctx, w.ContextSections)
}

// UserPromptWrapper is kept as a compatibility alias for callers that still use
// the old name. Agent request assembly treats it as a user-context builder and
// no longer mutates the real user message that is persisted in history.
type UserPromptWrapper = UserContextBuilder

// UserPromptOption is kept for compatibility with NewUserPromptWrapper.
type UserPromptOption = UserContextOption

// NewUserPromptWrapper creates a user-context builder with the given options.
func NewUserPromptWrapper(opts ...UserPromptOption) *UserPromptWrapper {
	return NewUserContextBuilder(opts...)
}

// Wrap is retained for legacy direct callers. New agent code should use Build
// and send the result as a separate transient user-context message.
func (w *UserContextBuilder) Wrap(rawInput string, ctx PromptContext) string {
	context := w.Build(ctx)
	if context == "" {
		return rawInput
	}
	var b strings.Builder
	b.WriteString(context)
	b.WriteString("\n\n")
	b.WriteString(rawInput)
	return b.String()
}

// BuildMetaUserContext wraps the rendered sections in a single marker so the
// provider can distinguish contextual user fragments from the real user request.
func (w *UserContextBuilder) BuildMetaUserContext(ctx PromptContext) string {
	context := w.Build(ctx)
	if context == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("<user_context>\n")
	b.WriteString(context)
	b.WriteString("\n</user_context>")
	return b.String()
}

func renderContextSections(ctx PromptContext, sections []ContextSection) string {
	if len(sections) == 0 {
		return ""
	}
	var b strings.Builder
	for _, s := range sections {
		content := s.Provider(ctx)
		if strings.TrimSpace(content) == "" {
			continue
		}
		openTag := s.Tag
		if strings.TrimSpace(s.Attributes) != "" {
			openTag += " " + strings.TrimSpace(s.Attributes)
		}
		fmt.Fprintf(&b, "<%s>\n%s\n</%s>\n\n", openTag, content, s.Tag)
	}
	return strings.TrimRight(b.String(), "\n")
}
