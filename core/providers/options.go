package providers

// StreamChatOption configures a single StreamChat invocation. It is the
// extensible shape for per-call knobs (tool choice, reasoning effort,
// future response formats, etc.) — add a new WithX helper when a new
// knob is needed without changing the interface signature.
type StreamChatOption func(*streamChatOptions)

// streamChatOptions is the private accumulator that each provider reads
// after applying the supplied options. Field set can evolve without
// affecting callers.
type streamChatOptions struct {
	toolChoice      ToolChoice
	reasoningEffort string
}

// ResolveOptions applies the supplied option functions to a fresh
// options struct and returns it. Provider implementations typically
// call ResolveOptions at the top of StreamChat and then read the
// fields they care about.
func ResolveOptions(opts []StreamChatOption) ResolvedOptions {
	cfg := &streamChatOptions{}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	return ResolvedOptions{
		ToolChoice:      cfg.toolChoice,
		ReasoningEffort: cfg.reasoningEffort,
	}
}

// ResolvedOptions is the read-side view of the applied options, exposed
// to provider implementations (which live in sibling packages and can't
// touch the unexported streamChatOptions directly).
type ResolvedOptions struct {
	ToolChoice      ToolChoice
	ReasoningEffort string
}

// ToolChoiceMode selects how the model decides whether to invoke a tool.
// Zero value (ToolChoiceAuto) means "let the model decide" — the
// provider's default behavior.
type ToolChoiceMode int

const (
	// ToolChoiceAuto lets the model pick between replying in text or
	// invoking a tool. Provider default.
	ToolChoiceAuto ToolChoiceMode = iota
	// ToolChoiceRequired forces the model to invoke some tool from the
	// supplied list, but does not pin which one.
	ToolChoiceRequired
	// ToolChoiceSpecific forces the model to invoke the named tool.
	ToolChoiceSpecific
)

// ToolChoice describes the forced-tool-call behavior for a single call.
type ToolChoice struct {
	Mode ToolChoiceMode
	// Name is the tool name to force. Required when Mode == ToolChoiceSpecific.
	Name string
}

// WithToolChoice overrides the tool-choice behavior for the call.
func WithToolChoice(tc ToolChoice) StreamChatOption {
	return func(o *streamChatOptions) { o.toolChoice = tc }
}

// WithToolChoiceTool is a convenience wrapper for
// WithToolChoice(ToolChoice{Mode: ToolChoiceSpecific, Name: name}).
// Use it to pin the model to a specific tool (e.g. structured-output
// schemas encoded as a single tool).
func WithToolChoiceTool(name string) StreamChatOption {
	return func(o *streamChatOptions) {
		o.toolChoice = ToolChoice{Mode: ToolChoiceSpecific, Name: name}
	}
}

// WithToolChoiceRequired forces the model to invoke some tool from the
// supplied list without pinning which one.
func WithToolChoiceRequired() StreamChatOption {
	return func(o *streamChatOptions) {
		o.toolChoice = ToolChoice{Mode: ToolChoiceRequired}
	}
}

// WithReasoningEffort overrides the reasoning effort for this call
// ("low", "medium", "high"). Empty string means "use the provider's
// default" — useful for callers (like the safety classifier) that want
// predictable latency regardless of the main agent's settings.
func WithReasoningEffort(effort string) StreamChatOption {
	return func(o *streamChatOptions) { o.reasoningEffort = effort }
}
