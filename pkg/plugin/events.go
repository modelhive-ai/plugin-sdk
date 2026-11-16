package plugin

import (
	"context"
	"time"
)

// Event is something the gateway itself noticed, apart from any one request:
// a provider deployment that keeps failing, a license about to expire, traces
// that could not be stored. A log-sink handler that also implements
// EventObserver receives them, alongside the finished exchanges it observes.
//
// Every replica of a scaled-out gateway emits its own events, so a plugin that
// notifies people should deduplicate across replicas (Host.Storage with
// IfRevision -1 and an expiry is enough).
type Event struct {
	// Kind names what happened, as "<area>.<condition>"; see the Event*
	// constants. Plugins should ignore kinds they do not know.
	Kind string `json:"kind"`
	// Severity is SeverityInfo, SeverityWarning or SeverityCritical.
	Severity string `json:"severity"`
	// Subject is what the event is about: a deployment, a plugin instance, a
	// budget. Together with Kind it identifies one condition.
	Subject string `json:"subject,omitempty"`
	// Message is one line for a person, with no secret in it.
	Message string `json:"message"`
	// Resolved marks the end of a condition an earlier event of the same Kind
	// and Subject reported. Not every kind ends with one.
	Resolved bool `json:"resolved,omitempty"`
	// Attrs carries details by name, such as "model" or "error".
	Attrs map[string]string `json:"attrs,omitempty"`
	Time  time.Time         `json:"time"`
}

// Event severities, from the least to the most urgent.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// Event kinds the gateway emits.
const (
	// EventProviderFailing: a deployment failed often enough that the router
	// stopped sending it requests for a while. Subject is the deployment.
	// Resolved when it answers again.
	EventProviderFailing = "provider.failing"
	// EventLicense: the license changed state (expiring, grace, expired,
	// invalid, missing). Resolved when it is valid again.
	EventLicense = "license.state"
	// EventTracesDropped: AI traffic traces were dropped (queue full or a
	// failed write), so some requests are missing from the console.
	EventTracesDropped = "traffic.dropped"
	// EventStoreFailing: the gateway could not write to its database (spend,
	// traces). Subject names what was being written.
	EventStoreFailing = "store.failing"
	// EventSharedStateFailing: Redis, which replicas share budgets and state
	// through, did not answer. Resolved when it does again.
	EventSharedStateFailing = "sharedstate.failing"
	// EventPluginFailed: a plugin instance failed to start or apply. Subject is
	// the instance name. Resolved when it runs again.
	EventPluginFailed = "plugin.failed"
)

// EventObserver sees the gateway's own events. It is called asynchronously,
// off any request path, within a few seconds; like Observe it should enqueue
// and return.
type EventObserver interface {
	ObserveEvent(ctx context.Context, e *Event) error
}

// SeverityRank orders severities: info 1, warning 2, critical 3, unknown 0.
func SeverityRank(s string) int {
	switch s {
	case SeverityInfo:
		return 1
	case SeverityWarning:
		return 2
	case SeverityCritical:
		return 3
	}
	return 0
}
