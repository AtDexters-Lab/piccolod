package catalog

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"piccolod/internal/api"

	"golang.org/x/mod/semver"
	"gopkg.in/yaml.v3"
)

const (
	DefaultRepoURL   = "https://raw.githubusercontent.com/AtDexters-Lab/piccolo-store/main"
	DefaultIndexFile = "index.yaml"
	CacheDuration    = 15 * time.Minute
	DefaultPageSize  = 20
	MaxPageSize      = 100
)

type Manager struct {
	repoURL    string
	cacheDir   string
	httpClient *http.Client

	cacheMu     sync.RWMutex
	cachedApps  []api.CatalogItem
	lastUpdated time.Time
}

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

	if cacheDir != "" {
		if err := os.MkdirAll(cacheDir, 0755); err != nil {
			log.Printf("WARN: failed to create catalog cache dir %s: %v", cacheDir, err)
		}
	}

	return &Manager{
		repoURL:    repoURL,
		cacheDir:   cacheDir,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
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
		path := filepath.Join(m.cacheDir, DefaultIndexFile)
		if err := os.WriteFile(path, body, 0644); err != nil {
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

func (m *Manager) GetAppTemplate(ctx context.Context, appName string) (string, error) {
	if err := m.ensureCache(ctx, false); err != nil {
		// Proceed if we have cached data, otherwise fail
		m.cacheMu.RLock()
		if len(m.cachedApps) == 0 {
			m.cacheMu.RUnlock()
			return "", err
		}
		m.cacheMu.RUnlock()
		log.Printf("WARN: serving stale catalog template lookup due to fetch error: %v", err)
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(body), nil
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
