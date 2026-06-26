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

	PromptBuilder *prompt.SystemPromptBuilder
	UserWrapper   *prompt.UserPromptWrapper

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
	}
	if cfg.PromptBuilder != nil {
		opts = append(opts, agent.WithSystemPromptBuilder(cfg.PromptBuilder))
	}
	if cfg.UserWrapper != nil {
		opts = append(opts, agent.WithUserPromptWrapper(cfg.UserWrapper))
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

func configureClassifier(ag *agent.Agent, cfg AgentConfig) {
	if cfg.Config == nil || !cfg.Profile.Safety.Classifier.IsEnabled() {
		return
	}

	classifierProfileName, classifierProfile, err := cfg.Config.ResolveClassifierProfile(cfg.ProfileName)
	if err != nil {
		ag.SetApprovalPolicy(agent.ApprovalPolicyManual)
		agent.WithClassifierUnavailableReason(err.Error())(ag)
		return
	}

	agent.WithClassifierProfile(classifierProfileName, classifierProfile)(ag)
	classifierProvider, err := providers.NewProvider(classifierProfile)
	if err != nil {
		agent.WithClassifierUnavailableReason(
			fmt.Sprintf("failed to create classifier provider %q: %v", classifierProfileName, err),
		)(ag)
		return
	}

	classifierOpts := []agent.ClassifierOption{}
	if cfg.Observer != nil {
		classifierOpts = append(classifierOpts, agent.WithClassifierObserver(cfg.Observer, classifierProfile.Model))
	}
	classifier := agent.NewProviderSafetyClassifier(
		classifierProvider,
		cfg.Profile.Safety.Classifier,
		classifierOpts...,
	)
	agent.WithSafetyClassifier(classifier)(ag)
}
