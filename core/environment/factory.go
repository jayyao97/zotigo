package environment

import (
	"fmt"
)

// New creates an environment from the given configuration.
func New(cfg Config) (Environment, error) {
	switch cfg.Type {
	case TypeLocal:
		return NewLocal(cfg.WorkDir, cfg.DataDir)

	case TypeE2B:
		if cfg.E2B == nil {
			return nil, fmt.Errorf("E2B config is required for e2b environment")
		}
		// TODO: Implement E2BEnvironment when ready
		return nil, fmt.Errorf("e2b environment not implemented yet")

	case TypeDocker:
		if cfg.Docker == nil {
			return nil, fmt.Errorf("Docker config is required for docker environment")
		}
		// TODO: Implement DockerEnvironment when ready
		return nil, fmt.Errorf("docker environment not implemented yet")

	case TypeCustom:
		if cfg.Custom == nil {
			return nil, fmt.Errorf("Custom config is required for custom environment")
		}
		if cfg.Custom.Executor == nil {
			return nil, fmt.Errorf("executor is required for custom environment")
		}
		if cfg.Custom.Store == nil {
			return nil, fmt.Errorf("store is required for custom environment")
		}
		return NewCustom(cfg.Custom.Executor, cfg.Custom.Store), nil

	default:
		return nil, fmt.Errorf("unknown environment type: %s", cfg.Type)
	}
}
