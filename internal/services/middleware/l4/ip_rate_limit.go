package l4

import (
	"container/list"
	"fmt"
	"net"
	"sync"
	"time"

	"piccolod/internal/services/middleware"
)

const (
	defaultRateMaxIPs = 4096 // bound on tracked IPs to prevent unbounded growth
)

// ipRateLimitConfig is the parsed shape of an ip_rate_limit params block.
type ipRateLimitConfig struct {
	// PerSecond is the steady-state allowance (events/second/IP).
	PerSecond float64
	// Burst is the maximum events the bucket can hold (allows brief spikes).
	Burst int
	// MaxIPs caps the per-IP token-bucket map. LRU eviction kicks in at the
	// limit. Defaults to 4096.
	MaxIPs int
}

func parseIPRateLimitParams(params map[string]any) (*ipRateLimitConfig, error) {
	cfg := &ipRateLimitConfig{MaxIPs: defaultRateMaxIPs}

	rateRaw, ok := params["per_second"]
	if !ok {
		return nil, fmt.Errorf("ip_rate_limit: required param \"per_second\" missing")
	}
	switch v := rateRaw.(type) {
	case float64:
		cfg.PerSecond = v
	case int:
		cfg.PerSecond = float64(v)
	default:
		return nil, fmt.Errorf("ip_rate_limit: per_second: expected number, got %T", rateRaw)
	}
	if cfg.PerSecond <= 0 {
		return nil, fmt.Errorf("ip_rate_limit: per_second must be > 0, got %v", cfg.PerSecond)
	}

	burstRaw, ok := params["burst"]
	if !ok {
		return nil, fmt.Errorf("ip_rate_limit: required param \"burst\" missing")
	}
	switch v := burstRaw.(type) {
	case int:
		cfg.Burst = v
	case float64:
		cfg.Burst = int(v)
	default:
		return nil, fmt.Errorf("ip_rate_limit: burst: expected integer, got %T", burstRaw)
	}
	if cfg.Burst <= 0 {
		return nil, fmt.Errorf("ip_rate_limit: burst must be > 0, got %d", cfg.Burst)
	}

	if maxRaw, ok := params["max_ips"]; ok {
		switch v := maxRaw.(type) {
		case int:
			cfg.MaxIPs = v
		case float64:
			cfg.MaxIPs = int(v)
		default:
			return nil, fmt.Errorf("ip_rate_limit: max_ips: expected integer, got %T", maxRaw)
		}
		if cfg.MaxIPs <= 0 {
			return nil, fmt.Errorf("ip_rate_limit: max_ips must be > 0, got %d", cfg.MaxIPs)
		}
	}

	return cfg, nil
}

// ipBucket is a single-IP token bucket. Refills at perSecond rate, capped at
// burst tokens. Thread-safe via the parent limiter's mutex.
type ipBucket struct {
	tokens    float64
	updatedAt time.Time
	lruElem   *list.Element // back-pointer for O(1) LRU bump
}

// ipRateLimiter is the shared per-listener rate-limit state. One instance per
// IPRateLimit middleware (built per ip_rate_limit operator entry; a listener
// with multiple ip_rate_limit entries gets multiple limiters).
type ipRateLimiter struct {
	cfg *ipRateLimitConfig
	now func() time.Time // injected for tests

	mu      sync.Mutex
	buckets map[string]*ipBucket
	lru     *list.List // front=newest, back=oldest
}

func newIPRateLimiter(cfg *ipRateLimitConfig) *ipRateLimiter {
	return &ipRateLimiter{
		cfg:     cfg,
		now:     time.Now,
		buckets: make(map[string]*ipBucket, cfg.MaxIPs),
		lru:     list.New(),
	}
}

// allow consumes one token from the bucket for ipKey. Returns true if a
// token was available (request allowed); false if the bucket is empty
// (request denied).
func (l *ipRateLimiter) allow(ipKey string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	bucket, ok := l.buckets[ipKey]
	if !ok {
		// Evict oldest if at capacity.
		if len(l.buckets) >= l.cfg.MaxIPs {
			oldest := l.lru.Back()
			if oldest != nil {
				oldKey := oldest.Value.(string)
				delete(l.buckets, oldKey)
				l.lru.Remove(oldest)
			}
		}
		bucket = &ipBucket{
			tokens:    float64(l.cfg.Burst),
			updatedAt: now,
		}
		bucket.lruElem = l.lru.PushFront(ipKey)
		l.buckets[ipKey] = bucket
	} else {
		// Refill since last call.
		elapsed := now.Sub(bucket.updatedAt).Seconds()
		bucket.tokens += elapsed * l.cfg.PerSecond
		if bucket.tokens > float64(l.cfg.Burst) {
			bucket.tokens = float64(l.cfg.Burst)
		}
		bucket.updatedAt = now
		l.lru.MoveToFront(bucket.lruElem)
	}

	if bucket.tokens >= 1.0 {
		bucket.tokens -= 1.0
		return true
	}
	return false
}

// IPRateLimit constructs an L4 (TCP) ip_rate_limit middleware. The chain
// signals deny by returning without calling next; the chain entry closes
// the conn (see IPAllowlist comment for the contract). Metrics record the
// deny event with reason "ip_rate_limit".
func IPRateLimit(params map[string]any, metrics *MetricsRegistry) (middleware.L4Middleware, error) {
	cfg, err := parseIPRateLimitParams(params)
	if err != nil {
		return nil, err
	}
	limiter := newIPRateLimiter(cfg)
	return func(next middleware.ConnHandler) middleware.ConnHandler {
		return func(ctx middleware.ConnContext, c net.Conn) {
			ip := middleware.EffectiveSourceIP(ctx)
			key := ipRateKey(ip)
			if limiter.allow(key) {
				next(ctx, c)
				return
			}
			recordDeny(metrics, ctx.Endpoint.App, ctx.Endpoint.Listener, ip, "ip_rate_limit")
		}
	}, nil
}

// IPRateLimitUDP constructs the L4UDP analogue.
func IPRateLimitUDP(params map[string]any, metrics *MetricsRegistry) (middleware.L4UDPMiddleware, error) {
	cfg, err := parseIPRateLimitParams(params)
	if err != nil {
		return nil, err
	}
	limiter := newIPRateLimiter(cfg)
	return func(next middleware.UDPHandler) middleware.UDPHandler {
		return func(ctx middleware.UDPContext, payload []byte, sink middleware.UDPSink) {
			var ip net.IP
			if ctx.Source != nil {
				ip = middleware.CanonicalIP(ctx.Source.IP)
			}
			key := ipRateKey(ip)
			if limiter.allow(key) {
				next(ctx, payload, sink)
				return
			}
			recordDeny(metrics, ctx.Endpoint.App, ctx.Endpoint.Listener, ip, "ip_rate_limit")
		}
	}, nil
}

func ipRateKey(ip net.IP) string {
	if ip == nil {
		return "unknown"
	}
	return ip.String()
}
