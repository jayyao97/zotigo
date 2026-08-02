package prompt

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	"html"
	"sort"
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
	Key        string
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

// UserContextBuilder builds persistent, append-only user-context messages.
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
		w.ContextSections = append(w.ContextSections, ContextSection{
			Tag:        tag,
			Attributes: attributes,
			Provider:   provider,
		})
	}
}

// WithKeyedContext appends a lazy user-context section with a stable diff key.
func WithKeyedContext(key, tag string, provider func(PromptContext) string) UserContextOption {
	return func(w *UserContextBuilder) {
		w.ContextSections = append(w.ContextSections, ContextSection{Key: key, Tag: tag, Provider: provider})
	}
}

// WithKeyedAttributedContext appends a keyed lazy user-context section with
// attributes on the opening tag.
func WithKeyedAttributedContext(
	key string,
	tag string,
	attributes string,
	provider func(PromptContext) string,
) UserContextOption {
	return func(w *UserContextBuilder) {
		w.ContextSections = append(w.ContextSections, ContextSection{
			Key:        key,
			Tag:        tag,
			Attributes: attributes,
			Provider:   provider,
		})
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
// and persist the result as a separate contextual user message.
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

const userContextStateVersion = 1

type RenderedUserContextSection struct {
	Key        string
	Tag        string
	Attributes string
	Content    string
	Digest     string
}

type UserContextState struct {
	Version  int               `json:"version"`
	Sections map[string]string `json:"sections,omitempty"`
}

func (s *UserContextState) Clone() *UserContextState {
	if s == nil {
		return nil
	}
	sections := make(map[string]string, len(s.Sections))
	for key, digest := range s.Sections {
		sections[key] = digest
	}
	return &UserContextState{Version: s.Version, Sections: sections}
}

// BuildUpdate returns a full context for an uninitialized state, or only
// changed and removed sections for an existing state.
func (w *UserContextBuilder) BuildUpdate(
	ctx PromptContext,
	previous *UserContextState,
) (string, UserContextState, error) {
	sections, state, err := w.renderSections(ctx)
	if err != nil {
		return "", UserContextState{}, err
	}

	if previous == nil || previous.Version != userContextStateVersion {
		return renderUserContextUpdate("full", sections, nil), state, nil
	}

	changed := make([]RenderedUserContextSection, 0, len(sections))
	for _, section := range sections {
		if previous.Sections[section.Key] != section.Digest {
			changed = append(changed, section)
		}
	}
	var removed []string
	for key := range previous.Sections {
		if _, ok := state.Sections[key]; !ok {
			removed = append(removed, key)
		}
	}
	sort.Strings(removed)
	return renderUserContextUpdate("delta", changed, removed), state, nil
}

func (w *UserContextBuilder) BuildFull(ctx PromptContext) (string, UserContextState, error) {
	return w.BuildUpdate(ctx, nil)
}

func (w *UserContextBuilder) renderSections(
	ctx PromptContext,
) ([]RenderedUserContextSection, UserContextState, error) {
	rendered := make([]RenderedUserContextSection, 0, len(w.ContextSections))
	state := UserContextState{
		Version:  userContextStateVersion,
		Sections: make(map[string]string, len(w.ContextSections)),
	}
	seen := make(map[string]struct{}, len(w.ContextSections))
	for _, section := range w.ContextSections {
		key := strings.TrimSpace(section.Key)
		if key == "" {
			baseKey := defaultContextSectionKey(section.Tag, section.Attributes)
			key = baseKey
			for occurrence := 2; ; occurrence++ {
				if _, exists := seen[key]; !exists {
					break
				}
				key = fmt.Sprintf("%s#%d", baseKey, occurrence)
			}
		}
		if _, exists := seen[key]; exists {
			return nil, UserContextState{}, fmt.Errorf("duplicate user context key %q", key)
		}
		seen[key] = struct{}{}
		content := section.Provider(ctx)
		if strings.TrimSpace(content) == "" {
			continue
		}
		attributes := strings.TrimSpace(section.Attributes)
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(
			section.Tag+"\x00"+attributes+"\x00"+content,
		)))
		state.Sections[key] = digest
		rendered = append(rendered, RenderedUserContextSection{
			Key:        key,
			Tag:        section.Tag,
			Attributes: attributes,
			Content:    content,
			Digest:     digest,
		})
	}
	return rendered, state, nil
}

func renderUserContextUpdate(
	update string,
	sections []RenderedUserContextSection,
	removed []string,
) string {
	if len(sections) == 0 && len(removed) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<user_context update=%q>\n", update)
	for _, section := range sections {
		openTag := section.Tag
		if section.Attributes != "" {
			openTag += " " + section.Attributes
		}
		fmt.Fprintf(&b, "<%s>\n%s\n</%s>\n\n", openTag, section.Content, section.Tag)
	}
	for _, key := range removed {
		fmt.Fprintf(
			&b,
			"<context_removed key=%q>\nThe previously supplied context section no longer applies.\n</context_removed>\n\n",
			html.EscapeString(key),
		)
	}
	b.WriteString("</user_context>")
	return b.String()
}

func defaultContextSectionKey(tag, attributes string) string {
	attributes = strings.TrimSpace(attributes)
	if attributes == "" {
		return tag
	}
	return tag + "[" + attributes + "]"
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
