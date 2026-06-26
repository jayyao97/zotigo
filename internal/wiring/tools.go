package wiring

import (
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/lsp"
	"github.com/jayyao97/zotigo/core/tools"
	"github.com/jayyao97/zotigo/core/tools/builtin"
)

type ToolSetConfig struct {
	Config  *config.Config
	Profile config.ProfileConfig

	ShellPolicy *builtin.ShellPolicy
	LSPManager  *lsp.Manager
	Spawn       bool
}

func RegisterDefaultTools(ag *agent.Agent, cfg ToolSetConfig) error {
	childTools := []tools.Tool{
		&builtin.ReadFileTool{},
		&builtin.WriteFileTool{},
		&builtin.EditTool{},
	}
	for _, tool := range childTools {
		ag.RegisterTool(tool)
	}

	shellOpts := []builtin.ShellOption{}
	if cfg.ShellPolicy != nil {
		shellOpts = append(shellOpts, builtin.WithPolicy(cfg.ShellPolicy))
	}
	shellTool, err := builtin.NewShellTool(shellOpts...)
	if err != nil {
		return err
	}
	childTools = append(childTools, shellTool)
	ag.RegisterTool(shellTool)

	grepTool := &builtin.GrepTool{}
	globTool := &builtin.GlobTool{}
	childTools = append(childTools, grepTool, globTool)
	ag.RegisterTool(grepTool)
	ag.RegisterTool(globTool)

	if cfg.LSPManager != nil {
		lspTool := builtin.NewLSPTool(cfg.LSPManager)
		childTools = append(childTools, lspTool)
		ag.RegisterTool(lspTool)
	}

	if cfg.Config != nil {
		webClient := builtin.NewWebClient(builtin.WebConfig{
			TavilyAPIKey: cfg.Config.Tools.Web.TavilyAPIKey,
			UserAgent:    cfg.Config.Tools.Web.UserAgent,
			Timeout:      time.Duration(cfg.Config.Tools.Web.TimeoutSec) * time.Second,
			MaxPageSize:  cfg.Config.Tools.Web.MaxPageSize,
		})
		if sp := builtin.NewSearchProvider(webClient); sp != nil {
			webSearchTool := builtin.NewWebSearchTool(sp)
			childTools = append(childTools, webSearchTool)
			ag.RegisterTool(webSearchTool)
		}
		webFetchTool := builtin.NewWebFetchTool(webClient)
		childTools = append(childTools, webFetchTool)
		ag.RegisterTool(webFetchTool)
	}

	if cfg.Spawn {
		ag.RegisterTool(builtin.NewSpawnTool(cfg.Profile, childTools))
	}

	return nil
}
