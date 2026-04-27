package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/events"
	"piccolod/internal/fsutil"

	"golang.org/x/mod/semver"
	"gopkg.in/yaml.v3"
)

const (
	DefaultRepoURL   = "https://raw.githubusercontent.com/AtDexters-Lab/piccolo-store/main"
	DefaultIndexFile = "index.yaml"
	CacheDuration    = 15 * time.Minute
	DefaultPageSize  = 20
	MaxPageSize      = 100

	// Icon caching constants
	IconCacheDuration  = 24 * time.Hour
	IconMaxSize        = 1 << 20          // 1MB
	IconDialTimeout    = 10 * time.Second // TCP + TLS handshake
	IconRequestTimeout = 30 * time.Second // full request lifecycle

	// Concurrency control for external icon fetches
	MaxConcurrentIconFetches = 3
)

type Manager struct {
	repoURL    string
	cacheDir   string
	httpClient *http.Client

	// SSRF-safe HTTP client for fetching external icon URLs
	iconHTTPClient *http.Client

	cacheMu     sync.RWMutex
	cachedApps  []api.CatalogItem
	lastUpdated time.Time

	// Lifecycle fields
	lifecycleMu   sync.Mutex
	lifecycleCtx  context.Context
	lifecycleStop context.CancelFunc
	wg            sync.WaitGroup
	iconSemaphore chan struct{} // limits concurrent external icon fetches
	cacheDirReady bool
	cacheDirMu    sync.Mutex

	// Event bus subscription cleanup
	lockStateCancel func()
}

// IconResult contains the fetched icon data and content type.
type IconResult struct {
	Data        []byte
	ContentType string
}

// iconMeta stores metadata for cached icons on disk.
type iconMeta struct {
	ContentType string    `json:"content_type"`
	FetchedAt   time.Time `json:"fetched_at"`
}

// SSRF protection errors
var (
	ErrSSRFBlocked     = errors.New("SSRF: blocked private/loopback/link-local IP")
	ErrIconNotFound    = errors.New("icon not found for app")
	ErrNoIconURL       = errors.New("app has no icon URL")
	ErrInvalidIconURL  = errors.New("invalid icon URL")
	ErrIconTooLarge    = errors.New("icon exceeds maximum size")
	ErrInvalidMIMEType = errors.New("invalid MIME type for icon")
	ErrInvalidAppName  = errors.New("invalid app name for cache")
)

type FilterOptions struct {
	Query         string
	Category      string
	Page          int
	PageSize      int
	SystemVersion string // Current piccolod version
}

func NewManager(repoURL, cacheDir string) *Manager {
	if repoURL == "" {
		repoURL = DefaultRepoURL
	}
	// Ensure no trailing slash
	repoURL = strings.TrimRight(repoURL, "/")

	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		repoURL:        repoURL,
		cacheDir:       cacheDir,
		httpClient:     &http.Client{Timeout: 10 * time.Second},
		iconHTTPClient: newSSRFSafeClient(),
		lifecycleCtx:   ctx,
		lifecycleStop:  cancel,
		iconSemaphore:  make(chan struct{}, MaxConcurrentIconFetches),
	}

	// Best-effort cache dir creation at startup; may fail on read-only root.
	// EnsureCacheDir will retry after unlock.
	if cacheDir != "" {
		if err := m.EnsureCacheDir(); err != nil {
			log.Printf("DEBUG: catalog cache dir not ready at startup: %v", err)
		}
	}

	return m
}

// EnsureCacheDir creates the cache directory and icons subdirectory.
// It is idempotent: once it succeeds, subsequent calls are no-ops.
func (m *Manager) EnsureCacheDir() error {
	if m.cacheDir == "" {
		return nil
	}
	m.cacheDirMu.Lock()
	defer m.cacheDirMu.Unlock()
	if m.cacheDirReady {
		return nil
	}
	if err := os.MkdirAll(m.cacheDir, 0755); err != nil {
		return fmt.Errorf("create catalog cache dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(m.cacheDir, "icons"), 0755); err != nil {
		return fmt.Errorf("create icons cache dir: %w", err)
	}
	m.cacheDirReady = true
	return nil
}

// ensureCacheDirOnce is a lazy defense-in-depth call for write paths.
func (m *Manager) ensureCacheDirOnce() {
	m.cacheDirMu.Lock()
	ready := m.cacheDirReady
	m.cacheDirMu.Unlock()
	if ready {
		return
	}
	if err := m.EnsureCacheDir(); err != nil {
		log.Printf("DEBUG: catalog cache dir still not ready: %v", err)
	}
}

// ObserveLockState subscribes to lock state events. On unlock, it creates the
// cache directory and triggers a background catalog index refresh.
// Safe to call again after Stop (supports supervisor restart cycles).
func (m *Manager) ObserveLockState(bus *events.Bus) {
	if bus == nil {
		return
	}
	ch, cancel := bus.SubscribeWithCancel(events.TopicLockStateChanged, 8)

	m.lifecycleMu.Lock()
	m.lockStateCancel = cancel
	ctx := m.lifecycleCtx
	m.lifecycleMu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			select {
			case evt, ok := <-ch:
				if !ok {
					return
				}
				payload, ok := evt.Payload.(events.LockStateChanged)
				if !ok || payload.Locked {
					continue
				}
				// Unlock: ensure cache dirs exist, then pre-warm index
				if err := m.EnsureCacheDir(); err != nil {
					log.Printf("WARN: catalog cache dir creation failed on unlock: %v", err)
					continue
				}
				log.Printf("INFO: catalog cache dir ready after unlock; refreshing index")
				m.wg.Add(1)
				go func() {
					defer m.wg.Done()
					refreshCtx, refreshCancel := context.WithTimeout(ctx, 30*time.Second)
					defer refreshCancel()
					if err := m.refreshCache(refreshCtx); err != nil {
						log.Printf("WARN: catalog index pre-warm failed: %v", err)
					}
				}()
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Stop cancels event subscriptions and waits for background goroutines.
// It is safe to call multiple times. After Stop returns, ObserveLockState
// may be called again to restart observation (supervisor restart cycle).
func (m *Manager) Stop() {
	m.lifecycleMu.Lock()
	if m.lockStateCancel != nil {
		m.lockStateCancel()
		m.lockStateCancel = nil
	}
	m.lifecycleStop()
	m.lifecycleMu.Unlock()

	m.wg.Wait()

	// Reset lifecycle context so ObserveLockState can be called again.
	m.lifecycleMu.Lock()
	m.lifecycleCtx, m.lifecycleStop = context.WithCancel(context.Background())
	m.lifecycleMu.Unlock()
}

// newSSRFSafeClient creates an HTTP client with SSRF protection.
// It blocks connections to private, loopback, and link-local IP addresses.
func newSSRFSafeClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   IconDialTimeout,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return ssrfSafeDialContext(ctx, dialer, network, addr)
		},
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &http.Client{
		Timeout:   IconRequestTimeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("stopped after 3 redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("blocked redirect to non-HTTP scheme: %s", req.URL.Scheme)
			}
			return nil
		},
	}
}

// ssrfSafeDialContext resolves DNS and blocks private/loopback/link-local IPs.
// It dials validated IPs directly to prevent DNS rebinding (TOCTOU) attacks,
// and tries all safe IPs in order for Happy Eyeballs-like fallback on dual-stack networks.
func ssrfSafeDialContext(ctx context.Context, dialer *net.Dialer, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	// Resolve DNS first to check the actual IPs
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}

	// Validate all IPs — reject immediately if any resolve to blocked ranges
	var safeIPs []net.IP
	for _, ip := range ips {
		if isBlockedIP(ip.IP) {
			return nil, fmt.Errorf("%w: %s resolved to %s", ErrSSRFBlocked, host, ip.IP)
		}
		safeIPs = append(safeIPs, ip.IP)
	}

	if len(safeIPs) == 0 {
		return nil, fmt.Errorf("no IP addresses found for %s", host)
	}

	// Try each safe IP in order (handles dual-stack fallback when IPv6 is unreachable)
	var lastErr error
	for _, ip := range safeIPs {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// isBlockedIP checks if an IP should be blocked for SSRF protection.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// Block loopback (127.0.0.0/8, ::1)
	if ip.IsLoopback() {
		return true
	}
	// Block private addresses (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, fc00::/7)
	if ip.IsPrivate() {
		return true
	}
	// Block link-local (169.254.0.0/16, fe80::/10)
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	// Block unspecified (0.0.0.0, ::)
	if ip.IsUnspecified() {
		return true
	}
	return false
}

// ensureCache checks if cache is valid, otherwise refreshes it.
// If force is true, it ignores the cache duration.
func (m *Manager) ensureCache(ctx context.Context, force bool) error {
	m.cacheMu.RLock()
	valid := !m.lastUpdated.IsZero() && time.Since(m.lastUpdated) < CacheDuration
	hasItems := len(m.cachedApps) > 0
	m.cacheMu.RUnlock()

	if valid && !force {
		return nil
	}

	// Try loading from disk first if we have no items in memory (e.g. restart)
	if !hasItems && m.cacheDir != "" {
		if err := m.loadFromDisk(); err == nil {
			// Check validity again after load
			m.cacheMu.RLock()
			valid = !m.lastUpdated.IsZero() && time.Since(m.lastUpdated) < CacheDuration
			m.cacheMu.RUnlock()
			if valid && !force {
				return nil
			}
		}
	}

	return m.refreshCache(ctx)
}

func (m *Manager) loadFromDisk() error {
	if m.cacheDir == "" {
		return fmt.Errorf("no cache dir")
	}
	path := filepath.Join(m.cacheDir, DefaultIndexFile)
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var index struct {
		Apps []api.CatalogItem `yaml:"apps"`
	}
	if err := yaml.NewDecoder(f).Decode(&index); err != nil {
		return err
	}

	info, err := f.Stat()
	if err != nil {
		return err
	}

	m.cacheMu.Lock()
	m.cachedApps = index.Apps
	m.lastUpdated = info.ModTime()
	m.cacheMu.Unlock()
	return nil
}

func (m *Manager) refreshCache(ctx context.Context) error {
	// If refresh fails, we might want to keep using stale data if available.
	// But ensureCache logic handles the "try to get fresh data" part.

	url := fmt.Sprintf("%s/%s", m.repoURL, DefaultIndexFile)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch catalog index: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to fetch catalog index: status %d", resp.StatusCode)
	}

	// Read body to parse AND write to disk
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	var index struct {
		Apps []api.CatalogItem `yaml:"apps"`
	}

	if err := yaml.Unmarshal(body, &index); err != nil {
		return fmt.Errorf("failed to parse catalog index: %w", err)
	}

	m.cacheMu.Lock()
	m.cachedApps = index.Apps
	m.lastUpdated = time.Now()
	m.cacheMu.Unlock()

	// Persist to disk
	if m.cacheDir != "" {
		m.ensureCacheDirOnce()
		path := filepath.Join(m.cacheDir, DefaultIndexFile)
		if err := fsutil.AtomicWriteFile(path, body, 0644); err != nil {
			log.Printf("WARN: failed to write catalog cache: %v", err)
		}
	}

	return nil
}

func (m *Manager) GetApps(ctx context.Context, opts FilterOptions) (*api.CatalogResponse, error) {
	if err := m.ensureCache(ctx, false); err != nil {
		// If fetch fails but we have stale cache, we serve it.
		m.cacheMu.RLock()
		if len(m.cachedApps) == 0 {
			m.cacheMu.RUnlock()
			return nil, err
		}
		m.cacheMu.RUnlock()

		log.Printf("WARN: serving stale catalog due to fetch error: %v", err)
	}

	m.cacheMu.RLock()
	defer m.cacheMu.RUnlock()

	var filtered []api.CatalogItem

	// sanitize system version for semver check
	sysVer := opts.SystemVersion
	if sysVer != "" && !strings.HasPrefix(sysVer, "v") {
		sysVer = "v" + sysVer
	}

	for _, app := range m.cachedApps {
		// 1. Search Filter
		if opts.Query != "" {
			q := strings.ToLower(opts.Query)
			if !strings.Contains(strings.ToLower(app.Name), q) &&
				!strings.Contains(strings.ToLower(app.Description), q) &&
				!containsTag(app.Tags, q) {
				continue
			}
		}

		// 2. Category Filter
		if opts.Category != "" {
			if !strings.EqualFold(app.Category, opts.Category) {
				continue
			}
		}

		// 3. Version Compatibility Filter
		if sysVer != "" && app.Compatibility != "" {
			// app.Compatibility is something like ">=0.1.0"
			// semver.Compare requires "v" prefix.
			// This is a bit complex because ">=0.1.0" isn't directly parsed by standard semver package
			// which mostly does Compare(v1, v2).
			// We might need a proper constraint parser.
			// For this MVP, let's do a simple check:
			// If compatibility starts with ">=", extract version, compare.
			// If it's just a version, treat as minimum.

			// Simple logic:
			// ">=0.1.0" -> allow if sysVer >= 0.1.0
			// "0.1.0" -> allow if sysVer >= 0.1.0

			reqVer := strings.TrimPrefix(app.Compatibility, ">=")
			reqVer = strings.TrimSpace(reqVer)
			if !strings.HasPrefix(reqVer, "v") {
				reqVer = "v" + reqVer
			}

			if semver.IsValid(sysVer) && semver.IsValid(reqVer) {
				if semver.Compare(sysVer, reqVer) < 0 {
					continue // System version is older than required
				}
			}
		}

		filtered = append(filtered, app)
	}

	// Pagination
	total := len(filtered)
	page := opts.Page
	if page < 1 {
		page = 1
	}
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}
	if pageSize > MaxPageSize {
		pageSize = MaxPageSize
	}

	start := (page - 1) * pageSize
	if start >= total {
		return &api.CatalogResponse{
			Apps:       []api.CatalogItem{},
			Page:       page,
			PageSize:   pageSize,
			Total:      total,
			TotalPages: (total + pageSize - 1) / pageSize,
		}, nil
	}

	end := start + pageSize
	if end > total {
		end = total
	}

	return &api.CatalogResponse{
		Apps:       filtered[start:end],
		Page:       page,
		PageSize:   pageSize,
		Total:      total,
		TotalPages: (total + pageSize - 1) / pageSize,
	}, nil
}

// resolveAppPath looks up the manifest path for an app from the cached catalog index.
// Ensures the cache is populated first, falling back to stale data on fetch errors.
func (m *Manager) resolveAppPath(ctx context.Context, appName string) (string, error) {
	if err := m.ensureCache(ctx, false); err != nil {
		m.cacheMu.RLock()
		if len(m.cachedApps) == 0 {
			m.cacheMu.RUnlock()
			return "", err
		}
		m.cacheMu.RUnlock()
		log.Printf("WARN: serving stale catalog lookup due to fetch error: %v", err)
	}

	m.cacheMu.RLock()
	var appPath string
	for _, app := range m.cachedApps {
		if strings.EqualFold(app.Name, appName) {
			appPath = app.Path
			break
		}
	}
	m.cacheMu.RUnlock()

	if appPath == "" {
		return "", fmt.Errorf("app %s not found in catalog", appName)
	}
	return appPath, nil
}

func (m *Manager) GetAppTemplate(ctx context.Context, appName string) (string, error) {
	appPath, err := m.resolveAppPath(ctx, appName)
	if err != nil {
		return "", err
	}

	// Fetch the actual app.yaml
	// If path is relative, append to repoURL
	var url string
	if strings.HasPrefix(appPath, "http") {
		url = appPath
	} else {
		// remove leading slash if present
		appPath = strings.TrimPrefix(appPath, "/")
		url = fmt.Sprintf("%s/%s", m.repoURL, appPath)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch app template: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			return "", fmt.Errorf("template not found: status 404")
		}
		return "", fmt.Errorf("failed to fetch app template: status %d", resp.StatusCode)
	}

	// Limit read to 1MB to prevent OOM from a hostile or misconfigured catalog
	// returning an oversized manifest. Matches FetchAppFile's cap.
	const maxManifestSize = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxManifestSize {
		return "", fmt.Errorf("catalog manifest exceeds 1MB limit")
	}

	return string(body), nil
}

// FetchAppFile fetches a file relative to an app's directory in the store.
// For example, appName="immich", relativePath="scripts/init.js" fetches
// {repoURL}/apps/immich/scripts/init.js (assuming the app's path is apps/immich/app.yaml).
func (m *Manager) FetchAppFile(ctx context.Context, appName, relativePath string) ([]byte, error) {
	if strings.Contains(relativePath, "..") {
		return nil, fmt.Errorf("relative path must not contain '..': %s", relativePath)
	}

	appPath, err := m.resolveAppPath(ctx, appName)
	if err != nil {
		return nil, err
	}

	// Construct the sibling file URL. Handle two catalog path formats:
	// (a) Absolute URL: resolve relative to the manifest URL's directory,
	//     preserving query parameters (e.g., presigned tokens, ?raw=1).
	// (b) Relative path: derive base dir and join onto repoURL.
	var fileURL string
	if strings.HasPrefix(appPath, "http://") || strings.HasPrefix(appPath, "https://") {
		parsed, parseErr := url.Parse(appPath)
		if parseErr != nil {
			return nil, fmt.Errorf("failed to parse catalog path %s: %w", appPath, parseErr)
		}
		parsed.Path = path.Join(path.Dir(parsed.Path), relativePath)
		// Preserve RawQuery and Fragment so presigned URLs / required tokens
		// still apply to the sibling file.
		fileURL = parsed.String()
	} else {
		baseDir := path.Dir(strings.TrimPrefix(appPath, "/"))
		joined, joinErr := url.JoinPath(m.repoURL, baseDir, relativePath)
		if joinErr != nil {
			return nil, fmt.Errorf("failed to construct URL for %s/%s: %w", appName, relativePath, joinErr)
		}
		fileURL = joined
	}

	req, err := http.NewRequestWithContext(ctx, "GET", fileURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch app file %s: %w", relativePath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("app file %s not found for %s (404)", relativePath, appName)
		}
		return nil, fmt.Errorf("failed to fetch app file %s: status %d", relativePath, resp.StatusCode)
	}

	// Limit read to 1MB to prevent OOM from oversized files.
	const maxFileSize = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read app file %s: %w", relativePath, err)
	}
	if len(body) > maxFileSize {
		return nil, fmt.Errorf("app file %s exceeds 1MB limit", relativePath)
	}

	return body, nil
}

func (m *Manager) GetCategories(ctx context.Context) ([]string, error) {
	if err := m.ensureCache(ctx, false); err != nil {
		m.cacheMu.RLock()
		if len(m.cachedApps) == 0 {
			m.cacheMu.RUnlock()
			return nil, err
		}
		m.cacheMu.RUnlock()
		log.Printf("WARN: serving stale categories due to fetch error: %v", err)
	}

	m.cacheMu.RLock()
	defer m.cacheMu.RUnlock()

	unique := make(map[string]struct{})
	for _, app := range m.cachedApps {
		if app.Category != "" {
			unique[app.Category] = struct{}{}
		}
	}

	categories := make([]string, 0, len(unique))
	for c := range unique {
		categories = append(categories, c)
	}
	sort.Strings(categories)
	return categories, nil
}
func containsTag(tags []string, query string) bool {
	for _, t := range tags {
		if strings.Contains(strings.ToLower(t), query) {
			return true
		}
	}
	return false
}

// GetIconByName fetches and caches the icon for a catalog app.
// Returns the icon data and content type, using disk cache with 24h TTL.
// isStale=true means the disk copy was past TTL and the caller should emit a
// short browser Cache-Control so the client picks up the background refresh
// without waiting another full TTL cycle.
func (m *Manager) GetIconByName(ctx context.Context, appName string) (result *IconResult, isStale bool, err error) {
	if err := m.ensureCache(ctx, false); err != nil {
		m.cacheMu.RLock()
		if len(m.cachedApps) == 0 {
			m.cacheMu.RUnlock()
			return nil, false, err
		}
		m.cacheMu.RUnlock()
		log.Printf("WARN: serving stale catalog for icon lookup due to fetch error: %v", err)
	}

	m.cacheMu.RLock()
	var iconURL string
	var found bool
	for _, app := range m.cachedApps {
		if strings.EqualFold(app.Name, appName) {
			iconURL = app.Icon
			found = true
			break
		}
	}
	m.cacheMu.RUnlock()

	if !found {
		return nil, false, fmt.Errorf("%w: %s", ErrIconNotFound, appName)
	}

	if iconURL == "" {
		return nil, false, fmt.Errorf("%w: %s", ErrNoIconURL, appName)
	}

	cacheKey := strings.ToLower(appName)

	if m.cacheDir != "" {
		if result, isStale, err := m.loadIconFromDisk(cacheKey); err == nil {
			if isStale {
				m.spawnIconRefresh(iconURL, cacheKey)
			}
			return result, isStale, nil
		}
	}

	res, err := m.fetchAndCacheIcon(ctx, iconURL, cacheKey)
	return res, false, err
}

// spawnIconRefresh launches a stale-while-revalidate background refresh under
// lifecycleMu so the new goroutine is reliably tracked by m.wg before Stop()
// can observe a zero counter, and the captured ctx cannot be reassigned out
// from under us by Stop()'s post-Wait reset. If the manager is already shutting
// down (lifecycleCtx already cancelled), the refresh is skipped.
func (m *Manager) spawnIconRefresh(iconURL, cacheKey string) {
	m.lifecycleMu.Lock()
	if m.lifecycleCtx.Err() != nil {
		m.lifecycleMu.Unlock()
		return
	}
	parentCtx := m.lifecycleCtx
	m.wg.Add(1)
	m.lifecycleMu.Unlock()

	go func() {
		defer m.wg.Done()
		refreshCtx, cancel := context.WithTimeout(parentCtx, IconRequestTimeout)
		defer cancel()
		if _, err := m.fetchAndCacheIcon(refreshCtx, iconURL, cacheKey); err != nil {
			log.Printf("DEBUG: background icon refresh failed for %s: %v", cacheKey, err)
		}
	}()
}

// fetchAndCacheIcon performs the upstream icon fetch and writes the result to
// the disk cache. Used by both the cold-miss synchronous path and the
// stale-while-revalidate background refresh path.
func (m *Manager) fetchAndCacheIcon(ctx context.Context, iconURL, cacheKey string) (*IconResult, error) {
	select {
	case m.iconSemaphore <- struct{}{}:
		defer func() { <-m.iconSemaphore }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if !strings.HasPrefix(iconURL, "http://") && !strings.HasPrefix(iconURL, "https://") {
		return nil, fmt.Errorf("%w: %s", ErrInvalidIconURL, iconURL)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", iconURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create icon request: %w", err)
	}

	resp, err := m.iconHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch icon: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("icon fetch failed: status %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	baseMIME := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	allowedTypes := map[string]bool{
		"image/png":                true,
		"image/jpeg":               true,
		"image/gif":                true,
		"image/webp":               true,
		"image/svg+xml":            true,
		"image/x-icon":             true,
		"image/vnd.microsoft.icon": true,
	}
	if !allowedTypes[baseMIME] {
		return nil, fmt.Errorf("%w: got %s", ErrInvalidMIMEType, contentType)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, IconMaxSize+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read icon body: %w", err)
	}
	if len(data) > IconMaxSize {
		return nil, ErrIconTooLarge
	}

	result := &IconResult{
		Data:        data,
		ContentType: baseMIME,
	}

	if m.cacheDir != "" {
		if err := m.saveIconToDisk(cacheKey, result); err != nil {
			log.Printf("WARN: failed to cache icon for %s: %v", cacheKey, err)
		}
	}

	return result, nil
}

// sanitizeAppNameForCache validates the app name is safe to use as a filename.
// Prevents path traversal attacks via malicious app names.
func sanitizeAppNameForCache(appName string) (string, error) {
	// Only allow alphanumeric, hyphen, underscore, and period (for things like "my.app")
	// Must not start with a period (hidden files) or contain path separators
	if appName == "" {
		return "", ErrInvalidAppName
	}
	for _, r := range appName {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.') {
			return "", fmt.Errorf("%w: contains disallowed character", ErrInvalidAppName)
		}
	}
	if appName[0] == '.' {
		return "", fmt.Errorf("%w: cannot start with period", ErrInvalidAppName)
	}
	if strings.Contains(appName, "..") {
		return "", fmt.Errorf("%w: cannot contain '..'", ErrInvalidAppName)
	}
	return appName, nil
}

// loadIconFromDisk loads a cached icon from disk. Returns isStale=true when the
// on-disk copy is older than IconCacheDuration but otherwise valid; the caller
// is expected to serve the stale copy and trigger a background refresh.
func (m *Manager) loadIconFromDisk(appName string) (result *IconResult, isStale bool, err error) {
	if m.cacheDir == "" {
		return nil, false, fmt.Errorf("no cache dir")
	}

	safeName, err := sanitizeAppNameForCache(appName)
	if err != nil {
		return nil, false, err
	}

	metaPath := filepath.Join(m.cacheDir, "icons", safeName+".meta")
	dataPath := filepath.Join(m.cacheDir, "icons", safeName+".bin")

	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, false, err
	}

	var meta iconMeta
	if err := json.Unmarshal(metaData, &meta); err != nil {
		return nil, false, err
	}

	data, err := os.ReadFile(dataPath)
	if err != nil {
		return nil, false, err
	}

	return &IconResult{
		Data:        data,
		ContentType: meta.ContentType,
	}, time.Since(meta.FetchedAt) > IconCacheDuration, nil
}

// saveIconToDisk saves an icon and its metadata to disk cache.
func (m *Manager) saveIconToDisk(appName string, result *IconResult) error {
	if m.cacheDir == "" {
		return fmt.Errorf("no cache dir")
	}
	m.ensureCacheDirOnce()

	safeName, err := sanitizeAppNameForCache(appName)
	if err != nil {
		return err
	}

	metaPath := filepath.Join(m.cacheDir, "icons", safeName+".meta")
	dataPath := filepath.Join(m.cacheDir, "icons", safeName+".bin")

	// Write icon data
	if err := fsutil.AtomicWriteFile(dataPath, result.Data, 0644); err != nil {
		return err
	}

	// Write metadata
	meta := iconMeta{
		ContentType: result.ContentType,
		FetchedAt:   time.Now(),
	}
	metaData, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	return fsutil.AtomicWriteFile(metaPath, metaData, 0644)
}
