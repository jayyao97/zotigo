package wiring

import (
	"fmt"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/agent/prompt"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/observability"
	"github.com/jayyao97/zotigo/core/providers"
)

// AgentConfig captures the host-specific wiring needed to construct an agent.
// It deliberately stays above core/agent: callers still decide which prompt,
// tools, observer, and safety policy fit their transport.
type AgentConfig struct {
	Config      *config.Config
	ProfileName string
	Profile     config.ProfileConfig
	Executor    executor.Executor

	PromptBuilder      *prompt.SystemPromptBuilder
	UserContextBuilder *prompt.UserContextBuilder

	ApprovalPolicy agent.ApprovalPolicy
	TranscriptDir  string
	Observer       observability.Observer
	Middleware     []agent.Middleware

	ConfigureClassifier bool
}

// NewAgent constructs an agent with shared Zotigo wiring. Transport-specific
// callers still register the tool set they want after construction.
func NewAgent(cfg AgentConfig) (*agent.Agent, error) {
	opts := []agent.AgentOption{
		agent.WithApprovalPolicy(cfg.ApprovalPolicy),
		agent.WithProfileName(cfg.ProfileName),
	}
	if cfg.PromptBuilder != nil {
		opts = append(opts, agent.WithSystemPromptBuilder(cfg.PromptBuilder))
	}
	if cfg.UserContextBuilder != nil {
		opts = append(opts, agent.WithUserContextBuilder(cfg.UserContextBuilder))
	}
	if cfg.TranscriptDir != "" {
		opts = append(opts, agent.WithTranscriptDir(cfg.TranscriptDir))
	}
	if cfg.Observer != nil {
		opts = append(opts, agent.WithObserver(cfg.Observer))
	}
	for _, middleware := range cfg.Middleware {
		opts = append(opts, agent.WithMiddleware(middleware))
	}

	ag, err := agent.New(cfg.Profile, cfg.Executor, opts...)
	if err != nil {
		return nil, err
	}

	if cfg.ConfigureClassifier {
		configureClassifier(ag, cfg)
	}

	return ag, nil
}

// NewRuntimeProfile prepares a complete profile-dependent runtime bundle.
func NewRuntimeProfile(cfg AgentConfig) (agent.RuntimeProfile, error) {
	provider, err := providers.NewProvider(cfg.Profile)
	if err != nil {
		return agent.RuntimeProfile{}, fmt.Errorf("create provider: %w", err)
	}
	runtime := agent.RuntimeProfile{
		Name:     cfg.ProfileName,
		Config:   cfg.Profile,
		Provider: provider,
	}
	classifier := buildClassifierRuntime(cfg)
	runtime.Classifier = classifier.classifier
	runtime.ClassifierProfileName = classifier.profileName
	runtime.ClassifierProfile = classifier.profile
	runtime.ClassifierUnavailableReason = classifier.unavailableReason
	runtime.ForceManualApproval = classifier.forceManual
	return runtime, nil
}

func configureClassifier(ag *agent.Agent, cfg AgentConfig) {
	classifier := buildClassifierRuntime(cfg)
	if classifier.profileName == "" && classifier.unavailableReason == "" {
		return
	}
	if classifier.profileName != "" {
		agent.WithClassifierProfile(classifier.profileName, classifier.profile)(ag)
	}
	if classifier.unavailableReason != "" {
		agent.WithClassifierUnavailableReason(classifier.unavailableReason)(ag)
	}
	if classifier.forceManual {
		ag.SetProfileApprovalFallback(true)
	}
	if classifier.classifier != nil {
		agent.WithSafetyClassifier(classifier.classifier)(ag)
	}
}

type classifierRuntime struct {
	classifier        agent.SafetyClassifier
	profileName       string
	profile           config.ProfileConfig
	unavailableReason string
	forceManual       bool
}

func buildClassifierRuntime(cfg AgentConfig) classifierRuntime {
	if !cfg.ConfigureClassifier || cfg.Config == nil || !cfg.Profile.Safety.Classifier.IsEnabled() {
		return classifierRuntime{}
	}
	profileName, profile, err := cfg.Config.ResolveClassifierProfile(cfg.ProfileName)
	if err != nil {
		return classifierRuntime{unavailableReason: err.Error(), forceManual: true}
	}
	runtime := classifierRuntime{profileName: profileName, profile: profile}
	provider, err := providers.NewProvider(profile)
	if err != nil {
		runtime.unavailableReason = fmt.Sprintf("failed to create classifier provider %q: %v", profileName, err)
		return runtime
	}
	classifierOpts := []agent.ClassifierOption{}
	if cfg.Observer != nil {
		classifierOpts = append(classifierOpts, agent.WithClassifierObserver(cfg.Observer, profile.Model))
	}
	runtime.classifier = agent.NewProviderSafetyClassifier(provider, cfg.Profile.Safety.Classifier, classifierOpts...)
	return runtime
}
