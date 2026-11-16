package plugin

import "context"

// DependencyReporter is implemented by plugins that can report published
// objects which depend on another plugin. The integrations service uses it
// before disabling or deleting a contributing plugin instance.
type DependencyReporter interface {
	Dependents(ctx context.Context, pluginID string) ([]string, error)
}
