package providers

import (
	"fmt"
	"sync"

	"github.com/jayyao97/zotigo/core/config"
)

// FactoryFunc is a function that creates a new Provider instance given a profile configuration.
type FactoryFunc func(cfg config.ProfileConfig) (Provider, error)

var (
	// registry stores the registered provider factories.
	registry = make(map[string]FactoryFunc)
	mu       sync.RWMutex
)

// Register adds a provider factory to the registry.
// name is the PROVIDER name (e.g. "openai"), not the profile name.
func Register(name string, factory FactoryFunc) {
	mu.Lock()
	defer mu.Unlock()
	registry[name] = factory
}

// NewProvider creates a new instance of a provider based on the profile config.
// It uses cfg.Provider to find the correct factory.
func NewProvider(cfg config.ProfileConfig) (Provider, error) {
	mu.RLock()
	factory, ok := registry[cfg.Provider]
	mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("provider factory not found: %s", cfg.Provider)
	}

	return factory(cfg)
}

// ListProviders returns the names of all registered provider factories.
func ListProviders() []string {
	mu.RLock()
	defer mu.RUnlock()

	var names []string
	for name := range registry {
		names = append(names, name)
	}
	return names
}
