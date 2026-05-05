package l4

import (
	"net"
	"sync"

	"piccolod/internal/services/middleware"
)

// MetricsSample is a single observed event keyed by (App, Listener, SourceIP,
// DenyReason). DenyReason is empty for accepted connections.
//
// App is required because listener names are unique only within an app:
// two installed apps each declaring a listener named "web" would otherwise
// share counters for the same source IP and silently mix tenants.
type MetricsSample struct {
	App        string
	Listener   string
	SourceIP   string
	DenyReason string
}

// MetricsRegistry is the in-memory store backing conn_metrics. A single
// registry is shared across all listeners; per-event labels carry
// (listener, source_ip, deny_reason). The store is thread-safe.
//
// Counter semantics:
//   - Received: every conn observed at the L4 chain entry (recorded by
//     conn_metrics as outermost canonical). Counts pre-decision intake.
//   - Denied: every conn vetoed by a rule middleware (recorded by the rule
//     itself via RecordDenied with its reason label).
//
// Effective-accept count is computed by the consumer as Received − sum(Denied
// per IP). Future metrics endpoint surfaces the raw counters with labels
// {listener, source_ip, deny_reason} per F9 fix in plan §D19.
type MetricsRegistry struct {
	mu       sync.Mutex
	received map[MetricsSample]uint64
	denied   map[MetricsSample]uint64
}

// MetricsSnapshot is the value returned by Snapshot — counters keyed by
// MetricsSample. Both maps hold copies so mutations after Snapshot are
// invisible to the caller.
type MetricsSnapshot struct {
	Received map[MetricsSample]uint64
	Denied   map[MetricsSample]uint64
}

// NewMetricsRegistry constructs an empty store.
func NewMetricsRegistry() *MetricsRegistry {
	return &MetricsRegistry{
		received: map[MetricsSample]uint64{},
		denied:   map[MetricsSample]uint64{},
	}
}

// RecordReceived increments the received counter for (app, listener, sourceIP).
// Called by ConnMetrics for every conn observed at the L4 chain entry.
func (r *MetricsRegistry) RecordReceived(app, listener, sourceIP string) {
	r.mu.Lock()
	r.received[MetricsSample{App: app, Listener: listener, SourceIP: sourceIP}]++
	r.mu.Unlock()
}

// RecordDenied increments the denied counter for (app, listener, sourceIP, reason).
// Each rule middleware (ip_allowlist, ip_rate_limit, connection_auth) calls
// this directly with its own reason label before returning without calling
// next. ConnMetrics never invents a deny — rule middlewares own that surface.
func (r *MetricsRegistry) RecordDenied(app, listener, sourceIP, reason string) {
	r.mu.Lock()
	r.denied[MetricsSample{App: app, Listener: listener, SourceIP: sourceIP, DenyReason: reason}]++
	r.mu.Unlock()
}

// Snapshot returns a copy of the current counter state. Safe to call
// concurrently with RecordReceived/RecordDenied.
func (r *MetricsRegistry) Snapshot() MetricsSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	snap := MetricsSnapshot{
		Received: make(map[MetricsSample]uint64, len(r.received)),
		Denied:   make(map[MetricsSample]uint64, len(r.denied)),
	}
	for k, v := range r.received {
		snap.Received[k] = v
	}
	for k, v := range r.denied {
		snap.Denied[k] = v
	}
	return snap
}

// ConnMetrics is a canonical L4 middleware that records RECEIVED counters
// for every conn observed at the L4 chain entry (pre-decision intake). Deny
// counters are recorded directly by rule middlewares that emit denials
// (ip_allowlist, ip_rate_limit, connection_auth) so the deny reason label
// travels with the deny site rather than via shared context state.
//
// As the OUTERMOST canonical L4 entry, ConnMetrics observes every conn
// before any rule has a chance to short-circuit. The metrics consumer
// computes the effective-accept count as Received − sum(Denied per IP).
func ConnMetrics(reg *MetricsRegistry) middleware.L4Middleware {
	return func(next middleware.ConnHandler) middleware.ConnHandler {
		return func(ctx middleware.ConnContext, c net.Conn) {
			reg.RecordReceived(ctx.Endpoint.App, ctx.Endpoint.Listener, sourceIPLabel(ctx))
			next(ctx, c)
		}
	}
}

// ConnMetricsUDP is the L4UDP analogue.
func ConnMetricsUDP(reg *MetricsRegistry) middleware.L4UDPMiddleware {
	return func(next middleware.UDPHandler) middleware.UDPHandler {
		return func(ctx middleware.UDPContext, payload []byte, sink middleware.UDPSink) {
			reg.RecordReceived(ctx.Endpoint.App, ctx.Endpoint.Listener, udpSourceIPLabel(ctx))
			next(ctx, payload, sink)
		}
	}
}

func sourceIPLabel(ctx middleware.ConnContext) string {
	ip := middleware.EffectiveSourceIP(ctx)
	if ip == nil {
		return "unknown"
	}
	return ip.String()
}

func udpSourceIPLabel(ctx middleware.UDPContext) string {
	if ctx.Source == nil || ctx.Source.IP == nil {
		return "unknown"
	}
	return ctx.Source.IP.String()
}
