// Package plugin implements the HTTP reverse proxy adapter for on-prem services.
package plugin

import (
	"os"
	"strings"

	"github.com/memento-knowledge/mkonnect/internal/config"
)

// Registry holds a map of plugin name -> base URL.
type Registry struct {
	plugins map[string]string
}

// Load reads PLUGIN_* environment variables and builds a Registry.
// Keys are lowercased plugin names (e.g. PLUGIN_JENKINS -> "jenkins").
func Load(_ *config.Config) (*Registry, error) {
	plugins := make(map[string]string)
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "PLUGIN_") {
			continue
		}
		parts := strings.SplitN(env, "=", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.ToLower(strings.TrimPrefix(parts[0], "PLUGIN_"))
		if name == "" || parts[1] == "" {
			continue
		}
		plugins[name] = parts[1]
	}
	return &Registry{plugins: plugins}, nil
}

// Get returns the base URL for the named plugin.
func (r *Registry) Get(name string) (string, bool) {
	v, ok := r.plugins[name]
	return v, ok
}
