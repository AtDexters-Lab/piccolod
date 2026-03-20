package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"piccolod/internal/api"
	"piccolod/internal/app"
	"piccolod/internal/app/catalog"
	authpkg "piccolod/internal/auth"
	"piccolod/internal/cluster"
	"piccolod/internal/consensus"
	"piccolod/internal/container"
	crypt "piccolod/internal/crypt"
	pki "piccolod/internal/crypto"
	"piccolod/internal/events"
	"piccolod/internal/firewall"
	"piccolod/internal/health"
	"piccolod/internal/identity"
	"piccolod/internal/onboarding"
	hostnamepkg "piccolod/internal/hostname"
	"piccolod/internal/mdns"
	"piccolod/internal/oidc"
	"piccolod/internal/pcv"
	"piccolod/internal/persistence"
	"piccolod/internal/remote"
	"piccolod/internal/remote/nexusclient"
	"piccolod/internal/router"
	"piccolod/internal/runner"
	"piccolod/internal/runtime/commands"
	"piccolod/internal/runtime/supervisor"
	"piccolod/internal/services"
	"piccolod/internal/terminal"
	"piccolod/internal/tpm"
	"piccolod/internal/state/paths"
	"piccolod/internal/storage"
	"piccolod/internal/storage/diskprep"
	"piccolod/internal/storage/drbd"
	"piccolod/internal/storage/nbd"
	"piccolod/internal/update"

	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/gin-contrib/gzip"
	"github.com/gin-gonic/gin"

	webassets "piccolod"
)

const (
	acmeHTTPFallbackPort  = services.ACMEHTTPFallbackPort
	maxStaticAssetPathLen = 4 * 1024 // guard against path-based DoS
)

var errInvalidStaticPath = errors.New("invalid static asset path")

type unlockReloader interface {
	ReloadFromStorage() error
}

type osUpdateManager interface {
	Status(context.Context) (update.Status, error)
	Apply(context.Context) error
	Rollback(context.Context, string) error
	Reboot(context.Context) error
	ForceReboot(context.Context) error
	PowerOff(context.Context) error
	Watch(context.Context) error
}

// GinServer holds all the core components for our application using Gin framework.
type GinServer struct {
	appManager     *app.AppManager
	serviceManager *services.ServiceManager
	persistence    persistence.Service
	authRepo       persistence.AuthRepo
	mdnsManager    *mdns.Manager
	remoteManager  *remote.Manager
	router         *gin.Engine
	version        string
	events         *events.Bus
	progress       *events.BusProgressReporter
	leadership     *cluster.Registry
	supervisor     *supervisor.Supervisor
	dispatcher     *commands.Dispatcher
	routeManager   *router.Manager
	tlsMux         *services.TlsMux
	remoteResolver *serviceRemoteResolver
	httpSrv        *http.Server

	secureSrv      *http.Server
	secureListener net.Listener
	securePort     int

	// Precomputed ETags and cache policies for embedded web assets
	staticCache *staticAssetCache

	// Optional OpenAPI request validation (Phase 0)
	apiValidator *openAPIValidator

	// Auth & sessions (Phase 1)
	authManager *authpkg.Manager
	sessions    *authpkg.SessionStore
	userManager *authpkg.UserManager
	// simple rate-limit counters for login failures
	loginFailures int
	resetFailures int

	// Serializes concurrent /crypto/setup requests to prevent parallel LUKS init.
	setupMu sync.Mutex

	// Crypto manager for lock/unlock of app data volumes
	cryptoManager  *crypt.Manager
	storageMgr     *storage.Manager
	pcvPublisher   *pcv.Publisher
	pcvImporter    *pcv.Importer
	healthTracker  *health.Tracker
	updateManager  osUpdateManager
	catalogManager *catalog.Manager

	// Remote state tracking for OIDC app cache busting
	remoteStateMu         sync.Mutex
	remoteStateHosts      string // sorted, joined host list for diff detection
	oidcRestartTimer      *time.Timer
	oidcRestartDebounceMs int

	// OIDC Provider (cached)
	oidcProvider   *oidc.Provider
	oidcProviderMu sync.Mutex

	// Internal CA for OIDC Back-Channel
	internalCA   *pki.InternalCA
	internalCAMu sync.Mutex
	internalSrv *http.Server

	// Cached TLS certificate for GetCertificate callback (avoids disk I/O per handshake)
	cachedCert   *tls.Certificate
	cachedCertMu sync.RWMutex

	// Cert SAN refresh subscription cleanup
	certRefreshUnsub func()

	reloadersMu     sync.RWMutex
	unlockReloaders []unlockReloader

	// Onboarding and Install to Disk
	onboardingMgr *onboarding.Manager
	installer     *onboarding.Installer
	execRunner    runner.CommandRunner

	// Persistent terminal sessions
	terminalManager *terminal.Manager

	// Namek identity service (RFC 20260312)
	identityService *identity.Service
	tpmMu           sync.Mutex // protects tpmResult (written by recovery goroutine, read by Stop)
	tpmResult       *tpm.OpenResult
	certProvider    *remote.FileCertProvider

	// Namek adapter lifecycle (owned by gin_server, not remote.Manager)
	namekAdapter       nexusclient.Adapter
	namekAdapterCancel context.CancelFunc
	namekLastKey       string
	namekStopped       atomic.Bool
	namekMu            sync.RWMutex // protects namek adapter fields + namek domain state
	namekACME          *identity.NamekACMEClient
	namekDomainClient  *identity.NamekDomainClient           // domain management client
	namekDomains       map[string]*namekDomainState           // alias hostname → namek state
	namekReconcileStop context.CancelFunc                     // cancels in-flight reconciliation
}

type secureContextKey struct{}

var secureContextKeyInstance = secureContextKey{}

// portUnpublisherFunc adapts a function into services.PortUnpublisher.
type portUnpublisherFunc func(int)

func (f portUnpublisherFunc) Unpublish(p int) { f(p) }

// portPublisherFunc adapts a function into services.PortPublisher.
type portPublisherFunc func(int)

func (f portPublisherFunc) Publish(p int) { f(p) }

// remoteBase represents a single remote hostname base for resolution.
// The resolver maintains a unified list, source-tagged for independent management.
type remoteBase struct {
	source     string // source identifier for targeted removal
	portalHost string // e.g., "portal.home.example.com" or "slug.test.local"
	domain     string // base domain for app subdomain matching
}

type serviceRemoteResolver struct {
	services    *services.ServiceManager
	mu          sync.RWMutex
	aliases     map[string]string // hostname → DerivedHostLabel (or nexusclient.PortalHostLabel)
	port        int
	tlsMuxPort  int
	remoteBases []remoteBase // unified list of all remote bases (RFC 20260312)
}

func newServiceRemoteResolver(svc *services.ServiceManager) *serviceRemoteResolver {
	port := 80
	if p := os.Getenv("PORT"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v > 0 {
			port = v
		}
	}
	return &serviceRemoteResolver{services: svc, port: port}
}

// UpdateAliases sets the alias hostname→hostLabel mapping for routing.
// hostLabel is a DerivedHostLabel or PortalHostLabel for portal-targeted aliases.
func (r *serviceRemoteResolver) UpdateAliases(aliases map[string]string) {
	r.mu.Lock()
	r.aliases = aliases
	r.mu.Unlock()
}

// AliasHostLabels returns a snapshot of the current alias hostname→hostLabel map.
// Safe for concurrent use: UpdateAliases replaces the entire map atomically,
// so the returned reference remains immutable after this call.
func (r *serviceRemoteResolver) AliasHostLabels() map[string]string {
	r.mu.RLock()
	aliases := r.aliases
	r.mu.RUnlock()
	return aliases
}

// SetRemoteBases replaces all remote bases for a given source, preserving entries from other sources.
// Self-hosted bases are kept first so PortalHosts()[0] is the user-configured domain when
// both sources are active. Resolution correctness does not depend on ordering — Resolve and
// PortalHostForRequest use two-pass specificity matching (exact portal first, then subdomain).
func (r *serviceRemoteResolver) SetRemoteBases(source string, bases []remoteBase) {
	r.mu.Lock()
	var kept []remoteBase
	for _, b := range r.remoteBases {
		if b.source != source {
			kept = append(kept, b)
		}
	}
	// Self-hosted first for stable PortalHosts()[0] ordering.
	if source == "self-hosted" {
		r.remoteBases = append(bases, kept...)
	} else {
		r.remoteBases = append(kept, bases...)
	}
	r.mu.Unlock()
}

func (r *serviceRemoteResolver) IsRemoteHostname(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return false
	}
	r.mu.RLock()
	aliases := r.aliases
	bases := r.remoteBases
	r.mu.RUnlock()

	for _, rb := range bases {
		if host == rb.portalHost {
			return true
		}
		if rb.domain != "" && strings.HasSuffix(host, "."+rb.domain) {
			return true
		}
	}
	if _, ok := aliases[host]; ok {
		return true
	}
	return false
}

func (r *serviceRemoteResolver) SetTlsMuxPort(p int) { r.mu.Lock(); r.tlsMuxPort = p; r.mu.Unlock() }

// PortalHostForRequest returns the portal hostname that the given request host belongs to.
// For "app.slug.test.local" it returns "slug.test.local"; for "portal.example.com" it returns
// "portal.example.com". Returns "" if the host is not a recognized remote hostname.
// Only considers active portals (those registered via SetRemoteBases).
// Exact portal matches are checked before suffix (subdomain) matches to avoid
// misclassifying a more-specific portal as a subdomain of a less-specific one
// (e.g., slug.example.com should not match the example.com base).
func (r *serviceRemoteResolver) PortalHostForRequest(host string) string {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return ""
	}
	r.mu.RLock()
	aliases := r.aliases
	bases := r.remoteBases
	r.mu.RUnlock()

	// Pass 1: exact portal match (highest specificity).
	for _, rb := range bases {
		if rb.portalHost != "" && host == rb.portalHost {
			return rb.portalHost
		}
	}
	// Pass 2: subdomain match (longest domain wins to handle nested bases correctly).
	var bestHost string
	var bestLen int
	for _, rb := range bases {
		if rb.portalHost != "" && rb.domain != "" && strings.HasSuffix(host, "."+rb.domain) {
			if len(rb.domain) > bestLen {
				bestHost = rb.portalHost
				bestLen = len(rb.domain)
			}
		}
	}
	if bestHost != "" {
		return bestHost
	}
	// Alias domains are registered alongside the self-hosted portal config.
	// Map them to the first available portal host (aliases are self-hosted only today).
	if _, ok := aliases[host]; ok && len(bases) > 0 {
		for _, rb := range bases {
			if rb.portalHost != "" {
				return rb.portalHost
			}
		}
	}
	return ""
}

// PortalHosts returns all distinct active portal hostnames from remote bases.
// Only includes portals whose source is currently active (set via SetRemoteBases).
func (r *serviceRemoteResolver) PortalHosts() []string {
	r.mu.RLock()
	bases := r.remoteBases
	r.mu.RUnlock()

	seen := make(map[string]struct{})
	var hosts []string
	for _, rb := range bases {
		if rb.portalHost != "" {
			if _, ok := seen[rb.portalHost]; !ok {
				seen[rb.portalHost] = struct{}{}
				hosts = append(hosts, rb.portalHost)
			}
		}
	}
	return hosts
}

func (r *serviceRemoteResolver) RecordConnectionHint(localPort, sourcePort, remotePort int, isTLS bool) {
	if r.services == nil || sourcePort <= 0 {
		return
	}
	if localPort == r.port {
		return
	}
	r.services.RegisterProxyHint(localPort, sourcePort, remotePort, isTLS)
}

func (r *serviceRemoteResolver) Resolve(hostname string, remotePort int, isTLS bool) (int, bool) {
	h := strings.TrimSuffix(strings.ToLower(hostname), ".")
	r.mu.RLock()
	aliases := r.aliases
	portalPort := r.port
	tlsMuxPort := r.tlsMuxPort
	bases := r.remoteBases
	r.mu.RUnlock()

	normPort := remotePort
	if normPort == acmeHTTPFallbackPort {
		normPort = 80
	}

	// Check aliases first — they carry per-hostname routing (hostLabel).
	if aliases != nil {
		if port, ok := r.resolveAlias(h, aliases, portalPort, tlsMuxPort, normPort, isTLS); ok {
			return port, true
		}
	}

	// Resolve against all remote bases. Use two passes to ensure exact portal
	// matches take priority over subdomain matches (avoids misrouting when one
	// portal hostname is a subdomain of another's domain).
	// Pass 1: exact portal match only.
	for _, rb := range bases {
		if rb.portalHost != "" && h == rb.portalHost {
			if port, ok := r.resolveAgainstBase(h, rb.portalHost, rb.domain, portalPort, tlsMuxPort, normPort, isTLS); ok {
				return port, true
			}
		}
	}
	// Pass 2: subdomain matches (longest domain wins — consistent with PortalHostForRequest).
	var bestBase *remoteBase
	var bestLen int
	for i := range bases {
		rb := &bases[i]
		if rb.portalHost == "" || h == rb.portalHost {
			continue // already checked in pass 1
		}
		if rb.domain != "" && strings.HasSuffix(h, "."+rb.domain) && len(rb.domain) > bestLen {
			bestBase = rb
			bestLen = len(rb.domain)
		}
	}
	if bestBase != nil {
		if port, ok := r.resolveAgainstBase(h, bestBase.portalHost, bestBase.domain, portalPort, tlsMuxPort, normPort, isTLS); ok {
			return port, true
		}
	}

	return 0, false
}

// resolveAlias resolves a hostname against the alias map.
func (r *serviceRemoteResolver) resolveAlias(
	h string,
	aliases map[string]string,
	portalPort, tlsMuxPort, normPort int,
	isTLS bool,
) (int, bool) {
	hostLabel, ok := aliases[h]
	if !ok {
		return 0, false
	}
	if hostLabel == nexusclient.PortalHostLabel || hostLabel == "" {
		if normPort == 80 && portalPort > 0 {
			return portalPort, true
		}
		if isTLS && tlsMuxPort > 0 {
			return tlsMuxPort, true
		}
		return 0, false
	}
	if r.services != nil {
		if ep, found := r.services.ResolveByHostLabel(hostLabel, normPort); found {
			if ep.Flow == api.FlowTLS {
				return ep.PublicPort, true
			}
			if normPort == 80 {
				return ep.PublicPort, true
			}
			if isTLS && tlsMuxPort > 0 {
				return tlsMuxPort, true
			}
			return ep.PublicPort, true
		}
	}
	return 0, false
}

// resolveAgainstBase resolves a hostname against a single portal+domain base.
func (r *serviceRemoteResolver) resolveAgainstBase(
	h, portal, domain string,
	portalPort, tlsMuxPort, normPort int,
	isTLS bool,
) (int, bool) {
	// Portal host (apex): route to portal port / TLS mux.
	if h == portal {
		if normPort == 80 && portalPort > 0 {
			return portalPort, true
		}
		if isTLS && tlsMuxPort > 0 {
			return tlsMuxPort, true
		}
		return 0, false
	}

	// Extract host label from <app>.<base> or <listener>-<app>.<base>
	if domain != "" {
		suffix := "." + domain
		if strings.HasSuffix(h, suffix) {
			label := h[:len(h)-len(suffix)]
			if idx := strings.Index(label, "."); idx != -1 {
				label = label[:idx]
			}
			if label != "" && r.services != nil {
				if ep, ok := r.services.ResolveByHostLabel(label, normPort); ok {
					if ep.Flow == api.FlowTLS {
						return ep.PublicPort, true
					}
					if normPort == 80 {
						return ep.PublicPort, true
					}
					if isTLS && tlsMuxPort > 0 {
						return tlsMuxPort, true
					}
					return ep.PublicPort, true
				}
			}
		}
	}

	return 0, false
}

// remoteStatusAdapter adapts remote.Manager to services.RemoteStatusProvider.
// This bridges the remote manager to the services layer for health computation.
type remoteStatusAdapter struct {
	rm *remote.Manager
}

func (a *remoteStatusAdapter) GetRemoteStatus() services.RemoteStatus {
	if a.rm == nil {
		return services.RemoteStatus{}
	}
	st := a.rm.Status()
	return services.RemoteStatus{
		Enabled:        st.Enabled,
		Solver:         st.Solver,
		PortalHostname: st.PortalHostname,
	}
}

func (a *remoteStatusAdapter) GetCertificates() []services.RemoteCertificate {
	if a.rm == nil {
		return nil
	}
	certs := a.rm.ListCertificates()
	result := make([]services.RemoteCertificate, len(certs))
	for i, c := range certs {
		result[i] = services.RemoteCertificate{
			ID:            c.ID,
			Domains:       c.Domains,
			Status:        c.Status,
			FailureClass:  string(c.FailureClass),
			FailureCode:   c.FailureCode,
			FailureReason: c.FailureReason,
			RetryAt:       c.RetryAt,
			ExpiresAt:     c.ExpiresAt,
		}
	}
	return result
}

func (a *remoteStatusAdapter) GetAliases() []services.RemoteAlias {
	if a.rm == nil {
		return nil
	}
	aliases := a.rm.ListAliases()
	result := make([]services.RemoteAlias, len(aliases))
	for i, al := range aliases {
		result[i] = services.RemoteAlias{
			Hostname: al.Hostname,
			Listener: al.Listener,
		}
	}
	return result
}

// GinServerOption is a function that configures a GinServer.
type GinServerOption func(*GinServer)

// WithVersion sets the version for the server.
func WithGinVersion(version string) GinServerOption {
	return func(s *GinServer) {
		s.version = version
	}
}

// WithUpdateManager allows tests to inject a stub update manager.
func WithUpdateManager(m osUpdateManager) GinServerOption {
	return func(s *GinServer) {
		s.updateManager = m
	}
}

// NewGinServer creates the main server application using Gin and initializes all its components.
func NewGinServer(opts ...GinServerOption) (*GinServer, error) {
	// Create Podman CLI for app management
	podmanCLI := &container.PodmanCLI{}

	// Initialize shared infrastructure
	eventsBus := events.NewBus()
	progressReporter := events.NewBusProgressReporter(eventsBus)
	leadershipReg := cluster.NewRegistry()
	sup := supervisor.New()
	dispatch := commands.NewDispatcher()
	consensusMgr := consensus.NewStub(leadershipReg, eventsBus)
	stateDir := paths.CoreRoot()
	cmgr, err := crypt.NewManager(stateDir)
	if err != nil {
		return nil, fmt.Errorf("crypto manager init: %w", err)
	}
	healthTracker := health.NewTracker()

	// Wire key material changed callback to emit control store commit event.
	// This signals the PCV publisher (Phase 7) to re-snapshot after key rotations.
	cmgr.OnKeyMaterialChanged = func() {
		eventsBus.Publish(events.Event{
			Topic:   events.TopicControlStoreCommit,
			Payload: events.ControlStoreCommit{Revision: 0},
		})
	}

	// Initialize storage manager for disk preparation and LVM lifecycle.
	execRunner := runner.ExecRunner{}
	diskPreparer := diskprep.NewPreparer(execRunner)
	storageMgr := storage.NewManager(diskPreparer, eventsBus, execRunner)

	// Initialize NBD server and DRBD resource manager for block-native volume stack.
	// On single-node deployments (no cluster), these are nil — the volume manager
	// uses a simplified stack: thin LV → LUKS → ext4 (no NBD/DRBD layers).
	// TODO: enable when cluster support is wired in.
	var nbdSrv *nbd.Server
	var drbdMgr *drbd.ResourceManager

	// Initialize onboarding manager (detects boot mode for USB onboarding flow).
	bootMode, bootErr := storage.DetectBootMode(context.Background(), execRunner)
	if bootErr != nil {
		log.Printf("WARN: boot mode detection failed during init: %v", bootErr)
		bootMode = storage.BootModeUnknown
	}
	onboardingMgr := onboarding.NewManager(bootMode)
	installer := onboarding.NewInstaller(execRunner, events.NewBusProgressReporter(eventsBus), onboardingMgr)

	// Initialize PCV publisher and importer.
	pcvPub := pcv.NewPublisher(eventsBus, execRunner)
	pcvImp := pcv.NewImporter(execRunner)

	// Initialize app manager with filesystem state management
	svcMgr := services.NewServiceManager()
	routeMgr := router.NewManager()
	remoteResolver := newServiceRemoteResolver(svcMgr)
	svcMgr.ObserveRuntimeEvents(eventsBus)
	svcMgr.SetEventBus(eventsBus)
	// TLS mux (loopback, remote-only) — created now, started when remote is configured
	tlsMux := services.NewTlsMux(svcMgr)
	// Wire ACME HTTP-01 handler into HTTP proxies (set after remote manager init)
	appMgr, err := app.NewAppManagerWithServices(podmanCLI, "", svcMgr, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to init app manager: %w", err)
	}
	appMgr.ObserveRuntimeEvents(eventsBus)
	appMgr.SetRouter(routeMgr)
	appMgr.SetProgressReporter(progressReporter)
	appMgr.SetEventBus(eventsBus)

	// Initialize persistence module (skeleton; concrete components wired later)
	persist, err := persistence.NewService(persistence.Options{
		Events:     eventsBus,
		Leadership: leadershipReg,
		Consensus:  consensusMgr,
		Dispatcher: dispatch,
		Crypto:     cmgr,
		StateDir:   stateDir,
		DataDir:    paths.CoreRoot(),
		Runner:     execRunner,
		LVMgr:      storageMgr.LVMVolumes(),
		PoolMgr:    storageMgr.LVMPool(),
		NBDSrv:     nbdSrv,
		DRBDMgr:    drbdMgr,
		FlattenFn:   appMgr.MakeFlattenFn(),
		ImageSizeFn: appMgr.MakeImageSizeFn(),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to init persistence module: %w", err)
	}

	controlDir := persist.ControlVolume().MountDir
	if strings.TrimSpace(controlDir) == "" {
		return nil, fmt.Errorf("control volume mount unavailable")
	}
	// NOTE: Today we do not migrate existing app state into the control volume because we have
	// no pre-existing deployments. If that assumption changes we must add a migration path,
	// otherwise legacy installations would appear empty after upgrade.
	appMgr.SetStateBaseDir(controlDir)
	appMgr.SetLockReader(persist)
	appMgr.SetVolumeManager(persist.Volumes())
	if rootfs := persist.Rootfs(); rootfs != nil {
		appMgr.SetRootfsManager(rootfs)
	}
	svcMgr.SetLockReader(persist)

	// Set Gin to release mode for production (can be overridden by GIN_MODE env var)
	gin.SetMode(gin.ReleaseMode)

	// Dispatcher middleware slot available for metrics/auditing; lock/leader
	// enforcement is handled in persistence and managers to avoid duplication.

	mdnsDisabled := os.Getenv("PICCOLO_DISABLE_MDNS") == "1"
	var mdnsMgr *mdns.Manager
	if !mdnsDisabled {
		mdnsMgr = mdns.NewManager()
		// Wire the HTTP port to mDNS so SRV records advertise the correct port
		mdnsPort := 80
		if p := os.Getenv("PORT"); p != "" {
			if v, err := strconv.Atoi(p); err == nil && v > 0 {
				mdnsPort = v
			}
		}
		mdnsMgr.SetPort(mdnsPort)
		mdnsMgr.SetEventBus(eventsBus)
		mdnsMgr.ObserveServiceEndpoints(eventsBus)
	}

	catalogMgr := catalog.NewManager(os.Getenv("PICCOLO_APP_STORE_URL"), paths.CoreJoin("cache", "catalog"))

	s := &GinServer{
		appManager:     appMgr,
		serviceManager: svcMgr,
		persistence:    persist,
		mdnsManager:    mdnsMgr,
		routeManager:   routeMgr,
		tlsMux:         tlsMux,
		remoteResolver: remoteResolver,
		events:         eventsBus,
		progress:       progressReporter,
		leadership:     leadershipReg,
		supervisor:     sup,
		dispatcher:     dispatch,
		cryptoManager:  cmgr,
		storageMgr:     storageMgr,
		pcvPublisher:   pcvPub,
		pcvImporter:    pcvImp,
		healthTracker:  healthTracker,
		catalogManager: catalogMgr,
		onboardingMgr:  onboardingMgr,
		installer:      installer,
		execRunner:     execRunner,
	}
	// Initialize persistent terminal session manager
	s.terminalManager = terminal.NewManager()
	s.terminalManager.SetEventBus(eventsBus)

	// Seed baseline health statuses
	healthTracker.Setf("http", health.LevelOK, "HTTP server initialized")
	healthTracker.Setf("app-manager", health.LevelWarn, "app manager gated by lock state")
	healthTracker.Setf("service-manager", health.LevelOK, "service manager running")
	if mdnsDisabled {
		healthTracker.Setf("mdns", health.LevelWarn, "mdns disabled via PICCOLO_DISABLE_MDNS")
	} else {
		healthTracker.Setf("mdns", health.LevelOK, "mdns supervisor registered")
	}
	healthTracker.Setf("remote", health.LevelWarn, "remote manager initializing")
	healthTracker.Setf("persistence", health.LevelWarn, "control store locked")
	healthTracker.Setf("storage", health.LevelWarn, "storage awaiting disk preparation")
	healthTracker.Setf("update", health.LevelWarn, "update manager initializing")

	if !mdnsDisabled {
		s.supervisor.Register(supervisor.NewComponent("mdns", func(ctx context.Context) error {
			return s.mdnsManager.Start()
		}, func(ctx context.Context) error {
			return s.mdnsManager.Stop()
		}))
	}

	s.supervisor.Register(supervisor.NewComponent("service-manager", func(ctx context.Context) error {
		s.serviceManager.StartBackground()
		return nil
	}, func(ctx context.Context) error {
		s.serviceManager.Stop()
		return nil
	}))

	s.supervisor.Register(s.terminalManager)

	s.supervisor.Register(supervisor.NewComponent("app-manager", func(ctx context.Context) error {
		s.appManager.StartBackground()
		return nil
	}, func(ctx context.Context) error {
		s.appManager.StopBackground()
		return nil
	}))

	s.supervisor.Register(storageMgr)
	// pcvPub must be registered after storageMgr: supervisor stops in reverse
	// order, so pcvPub flushes before storage locks.
	s.supervisor.Register(pcvPub)
	s.supervisor.Register(supervisor.NewComponent("consensus", consensusMgr.Start, consensusMgr.Stop))
	s.supervisor.Register(newLeadershipObserver(eventsBus))
	s.supervisor.Register(supervisor.NewComponent("catalog", func(ctx context.Context) error {
		catalogMgr.ObserveLockState(eventsBus)
		return nil
	}, func(ctx context.Context) error {
		catalogMgr.Stop()
		return nil
	}))
	s.observeLockState(eventsBus)
	s.observeLeadership(eventsBus)
	s.observeRemoteConfig(eventsBus)
	s.observeProxyOIDCClients(eventsBus)
	s.observeStorageEvents(eventsBus)

	for _, opt := range opts {
		opt(s)
	}

	// Wire version to mDNS service metadata (after options are applied)
	if s.mdnsManager != nil && s.version != "" {
		s.mdnsManager.SetVersion(s.version)
	}

	// Initialize auth & sessions
	authRepo := persist.Control().Auth()
	authStorage := newPersistenceAuthStorage(authRepo)
	var am *authpkg.Manager
	if authStorage != nil {
		am, err = authpkg.NewManagerWithStorage(authStorage)
	} else {
		am, err = authpkg.NewManager(stateDir)
	}
	if err != nil {
		return nil, fmt.Errorf("auth manager init: %w", err)
	}
	s.authManager = am
	s.sessions = authpkg.NewSessionStore()
	s.authRepo = authRepo
	s.userManager = authpkg.NewUserManager(persist.Control().Users())

	// Wire proxy auth dependencies (listener auth rules enforcement happens in services.ProxyManager).
	if svcMgr != nil {
		svcMgr.ProxyManager().SetAuthConfig(s.userManager, func(r *http.Request) (*authpkg.Session, bool) {
			if r == nil {
				return nil, false
			}
			// RFC 20260122 §6.1: Check port-based app session cookie first for non-default ports.
			// On port-based access the browser sends both piccolo_session and piccolo_app_session_p<port>;
			// the app cookie must take priority to avoid returning the portal session (wrong audience).
			if _, port, splitErr := net.SplitHostPort(r.Host); splitErr == nil && port != "80" && port != "443" {
				portCookie, portErr := r.Cookie("piccolo_app_session_p" + port)
				if portErr == nil && strings.TrimSpace(portCookie.Value) != "" {
					return s.sessions.Get(portCookie.Value)
				}
			}
			// Fallback to standard portal session cookie
			c, err := r.Cookie("piccolo_session")
			if err == nil && strings.TrimSpace(c.Value) != "" {
				return s.sessions.Get(c.Value)
			}
			return nil, false
		})

		svcMgr.ProxyManager().SetPortalOriginResolver(s.portalOriginForRequest)

		// RFC 20260112: alias-domain warnings for protected/headers strategies.
		// The callback receives host and DerivedHostLabel (not listener name).
		svcMgr.ProxyManager().SetAliasChecker(func(host, hostLabel string) bool {
			if s == nil || s.remoteManager == nil {
				return false
			}
			h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
			if h == "" || hostLabel == "" {
				return false
			}
			for _, a := range s.remoteManager.ListAliases() {
				if strings.TrimSpace(a.Hostname) == "" {
					continue
				}
				if a.Listener != hostLabel {
					continue
				}
				aliasHost := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(a.Hostname)), ".")
				if aliasHost != "" && strings.EqualFold(h, aliasHost) {
					return true
				}
			}
			return false
		})

		// RFC 20260122: Configure proxy OIDC for headers/protected auth strategies
		svcMgr.ProxyManager().SetSessionStore(s.sessions)
		svcMgr.ProxyManager().SetLocalHostnameGetter(func() string {
			if s.mdnsManager != nil {
				return s.mdnsManager.Hostname()
			}
			return "piccolo.local"
		})
		svcMgr.ProxyManager().SetProxyOIDCConfig(services.ProxyOIDCConfig{
			SessionStore: s.sessions,
			UserManager:  s.userManager,
			GetPortalOrigin: s.portalOriginForRequest,
			GetLocalHostname: func() string {
				if s.mdnsManager != nil {
					return s.mdnsManager.Hostname()
				}
				return "piccolo.local"
			},
			ExchangeCode: s.proxyOIDCExchangeCode,
			GetUserInfo:  s.proxyOIDCGetUserInfo,
			UserCanAccessApp: func(ctx context.Context, userID, appName string) (bool, error) {
				if s.userManager == nil {
					return false, errors.New("user manager unavailable")
				}
				return s.userManager.IsAppAllowed(ctx, userID, appName)
			},
			// X-Forwarded-Proto is NOT trusted anywhere: the TLS mux terminates
			// TLS and forwards cleartext HTTP via io.Copy — it never injects
			// HTTP headers. TLS is detected via RequestArrivedViaTLS
			// (r.TLS + proxy hints).
		})
	}

	// Remote manager — uses network-bootstrap dir on the core filesystem.
	networkBootstrapDir := paths.CoreJoin("network-bootstrap")
	if err := os.MkdirAll(networkBootstrapDir, 0o711); err != nil {
		return nil, fmt.Errorf("ensure network-bootstrap dir: %w", err)
	}
	// Widen from 0700 (pre-rootless default) so per-app users can traverse
	// to reach the internal CA cert that gets bind-mounted into containers.
	_ = os.Chmod(networkBootstrapDir, 0o711)
	remoteStorage := newBootstrapRemoteStorage(persist.Control().Remote(), networkBootstrapDir)
	var rm *remote.Manager
	if remoteStorage != nil {
		rm, err = remote.NewManagerWithStorage(remoteStorage, networkBootstrapDir)
	} else {
		rm, err = remote.NewManager(networkBootstrapDir)
	}
	if err != nil {
		return nil, fmt.Errorf("remote manager init: %w", err)
	}
	s.remoteManager = rm
	s.registerUnlockReloader(rm)
	rm.SetEventsBus(eventsBus)
	s.observeRemoteCertQueuing(eventsBus)
	s.observeRemotePortClaims(eventsBus)

	// Wire remote status provider for health aggregation (RFC 20260125)
	if rm != nil && svcMgr != nil {
		svcMgr.SetRemoteStatusProvider(&remoteStatusAdapter{rm: rm})
	}

	// Internal CA is initialized at Start() from network-bootstrap dir (pre-unlock).

	// Now that remote manager exists, wire ACME challenge handler and cert provider
	if rm != nil && svcMgr != nil {
		svcMgr.ProxyManager().SetAcmeHandler(rm.HTTPChallengeHandler())
		certProv := remote.NewFileCertProvider(rm.CertDirectory())
		// Add network-bootstrap cert dir for namek certs (available pre-unlock)
		certProv.AddFallbackDir(paths.CoreJoin("network-bootstrap", "remote", "certs"))
		identitySvcRef := &s.identityService // capture pointer for closure
		certProv.SetMissingHandler(func(host string) {
			if rm == nil {
				return
			}
			h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
			if h == "" {
				return
			}

			// Namek cert recovery: if the missing cert is for the namek hostname,
			// force-enqueue the specific cert so even "ok" inventory entries get reissued.
			if idSvc := *identitySvcRef; idSvc != nil && idSvc.IsEnrolled() && idSvc.IsEnabled() {
				cfg := idSvc.DeviceConfig()
				namekHost := cfg.Hostname
				if custom := cfg.CustomFQDN(); custom != "" {
					namekHost = custom
				}
				namekHost = strings.TrimSuffix(strings.ToLower(namekHost), ".")
				if namekHost != "" && (h == namekHost || strings.HasSuffix(h, "."+namekHost)) {
					certDir := paths.CoreJoin("network-bootstrap", "remote", "certs")
					if h == namekHost {
						rm.EnqueueCertIssuance(remote.CertIssuanceRequest{
							ID: "namek-portal", Source: "namek", Solver: "dns-01",
							CertDir: certDir, CommonName: namekHost, Domains: []string{namekHost},
							Force: true,
						})
					} else {
						wildcard := "*." + namekHost
						rm.EnqueueCertIssuance(remote.CertIssuanceRequest{
							ID: "namek-wildcard", Source: "namek", Solver: "dns-01",
							CertDir: certDir, CommonName: wildcard, Domains: []string{wildcard, namekHost},
							Force: true,
						})
					}
					return
				}
			}

			// Self-hosted per-hostname cert recovery (HTTP-01 only)
			st := rm.Status()
			if !st.Enabled || !strings.EqualFold(st.Solver, "http-01") {
				return
			}
			base := remoteBaseHostname(&st)
			if base == "" {
				return
			}
			portal := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(st.PortalHostname)), ".")
			if portal != "" && h == portal {
				return
			}
			if !strings.HasSuffix(h, "."+base) {
				return
			}
			label := strings.TrimSuffix(h, "."+base)
			if i := strings.Index(label, "."); i != -1 {
				label = label[:i]
			}
			if label == "" || !isValidDNSLabel(label) {
				return
			}
			// Only queue a cert if an active service endpoint exists for this label.
			// Without this check, any TLS connection to *.<base> (bots, scanners,
			// typos) would trigger real ACME issuance and leave orphaned certs.
			if _, ok := svcMgr.ResolveByHostLabel(label, 0); !ok {
				return
			}
			rm.EnqueueCertIssuance(remote.CertIssuanceRequest{
				ID: "host:" + h, Source: "self-hosted", Solver: "http-01",
				Domains: []string{h}, CommonName: h,
			})
		})
		tlsMux.SetCertProvider(certProv)
		s.certProvider = certProv
	}
	// TPM and identity service (RFC 20260312: namek-managed remote access)
	akStateDir := paths.CoreJoin("network-bootstrap", "tpm")
	swtpmStateDir := paths.CoreJoin("swtpm")
	_ = os.MkdirAll(akStateDir, 0o700)
	_ = os.MkdirAll(swtpmStateDir, 0o700)

	var tpmDevice tpm.Device
	tpmResult, tpmErr := tpm.Open(akStateDir, swtpmStateDir)
	if tpmErr != nil {
		log.Printf("WARN: TPM unavailable: %v (identity service will be limited)", tpmErr)
	} else {
		tpmDevice = tpmResult.Device
		s.tpmResult = tpmResult
	}

	identityConfigPath := paths.CoreJoin("network-bootstrap", "remote", "identity.json")
	identitySvc := identity.NewService(identityConfigPath, tpmDevice)
	identitySvc.SetTPMDirs(akStateDir, swtpmStateDir)
	identitySvc.SetEventsBus(eventsBus)
	identitySvc.SetTPMReplacedHandler(func(old tpm.Device, newResult *tpm.OpenResult) {
		// Close the full OpenResult (Device + SwtpmProc) to avoid leaking
		// swtpm child processes on repeated AK recoveries.
		s.tpmMu.Lock()
		oldResult := s.tpmResult
		s.tpmResult = newResult
		s.tpmMu.Unlock()
		if oldResult != nil {
			oldResult.Close()
		} else if old != nil {
			// Fallback: no OpenResult tracked (shouldn't happen), close Device directly.
			old.Close()
		}
	})
	s.identityService = identitySvc

	s.supervisor.Register(identitySvc)

	// Self-hosted adapter (existing)
	var nexusAdapter nexusclient.Adapter
	if os.Getenv("PICCOLO_NEXUS_USE_STUB") == "1" {
		nexusAdapter = nexusclient.NewStub()
	} else {
		nexusAdapter = nexusclient.NewBackendAdapter(routeMgr, remoteResolver)
	}
	rm.SetNexusAdapter(nexusAdapter)
	svcMgr.SetFirewallManager(firewall.NewFirewalldManager()) // falls back to no-op stub if firewall-cmd absent
	rm.SetPortClaimProvider(svcMgr)

	// Namek adapter (new, with TPM token provider) — owned by GinServer
	if os.Getenv("PICCOLO_NEXUS_USE_STUB") != "1" {
		namekTP := identity.NewNamekTokenProvider(identitySvc.NamekClient, identitySvc.HandleTokenError)
		s.namekAdapter = nexusclient.NewBackendAdapter(routeMgr, remoteResolver,
			nexusclient.WithAdapterTokenProvider(namekTP),
			nexusclient.WithAdapterName("piccolo-namek"),
		)

		// Register namek orchClient in remote manager's source-agnostic registry
		s.namekACME = identity.NewNamekACMEClient(identitySvc.NamekClient)
		rm.RegisterOrchClient("namek", s.namekACME)

		// Domain management client for alias domain lifecycle with namek
		s.namekDomainClient = identity.NewNamekDomainClient(identitySvc.NamekClient)

	}

	// Subscribe to identity events for namek adapter state changes.
	// TopicIdentityReady fires on boot when an already-enrolled device starts;
	// TopicIdentityChanged fires on enrollment, enable/disable, hostname change.
	identityReadyCh := eventsBus.Subscribe(events.TopicIdentityReady, 8)
	identityChangedCh := eventsBus.Subscribe(events.TopicIdentityChanged, 8)
	go func() {
		// Seed with current state to avoid spurious "activated" log on boot.
		var lastLoggedState string
		if s.identityService != nil {
			lastLoggedState = s.identityService.Status().State
		}
		for {
			select {
			case _, ok := <-identityReadyCh:
				if !ok {
					identityReadyCh = nil
				}
			case _, ok := <-identityChangedCh:
				if !ok {
					identityChangedCh = nil
				}
			}
			if identityReadyCh == nil && identityChangedCh == nil {
				return
			}
			s.applyNamekState()
			// Log identity state change to remote activity log (only on actual transitions)
			if s.remoteManager != nil && s.identityService != nil {
				ids := s.identityService.Status()
				if ids.State != lastLoggedState {
					lastLoggedState = ids.State
					var msg string
					switch ids.State {
					case "active":
						msg = "Namek remote access activated"
					case "disabled":
						msg = "Namek remote access disabled"
					case "suspended":
						msg = "Namek identity suspended by server"
					case "not_enrolled":
						msg = "Namek identity reset"
					}
					if msg != "" {
						s.logNamekEvent("info", "%s", msg)
					}
				}
			}
		}
	}()

	remote.RegisterHandlers(dispatch, rm)
	s.healthTracker.Setf("remote", health.LevelOK, "remote manager ready")
	// Init secure loopback before refreshing remote runtime so that securePort
	// is known when resolvePortalPort() configures the TLS mux upstream target.
	if err := s.initSecureLoopback(); err != nil {
		return nil, fmt.Errorf("secure loopback init: %w", err)
	}
	s.refreshRemoteRuntime()

	// Update manager (MicroOS transactional-update)
	if s.updateManager == nil {
		um, err := update.NewManager(update.WithCurrentVersion(s.version))
		if err != nil {
			s.stopSecureLoopback(context.Background())
			return nil, fmt.Errorf("update manager init: %w", err)
		}
		s.updateManager = um
	}
	s.healthTracker.Setf("update", health.LevelOK, "update manager ready")

	// Register the update watchdog. It blocks until context is cancelled.
	s.supervisor.Register(supervisor.NewComponent("os-update-watchdog", func(ctx context.Context) error {
		go s.updateManager.Watch(ctx) // Start the watch loop in a goroutine
		return nil                    // Return immediately to the supervisor
	}, func(ctx context.Context) error {
		// Cancellation is handled by the context passed to Watch
		return nil
	}))

	// Register the systemd service-level watchdog. Pings systemd at WatchdogSec/2
	// to prove piccolod is not stuck (e.g. D-state from btrfs hang). No-op when
	// WatchdogSec is not set in the service file (dev/test).
	s.supervisor.Register(supervisor.NewComponent("systemd-watchdog", func(ctx context.Context) error {
		go s.runWatchdogLoop(ctx)
		return nil
	}, nil))

	// (Simplified) No dynamic port publish/unpublish wiring; allow dial to fail gracefully.

	// Rehydrate proxies for containers that survived restarts
	appMgr.RestoreServices(context.Background())

	s.staticCache = newStaticAssetCache(webassets.FS, "web")

	s.setupGinRoutes()
	return s, nil
}

// Start runs the Gin HTTP server and starts mDNS advertising.
func (s *GinServer) Start() error {
	port := os.Getenv("PORT")
	if port == "" {
		port = "80"
	}

	if err := s.supervisor.Start(context.Background()); err != nil {
		return fmt.Errorf("failed to start runtime components: %w", err)
	}

	// Start disk preparation based on boot mode and onboarding state.
	if s.storageMgr != nil && s.onboardingMgr != nil {
		switch {
		case s.onboardingMgr.BootMode() == storage.BootModeInternal:
			log.Printf("INFO: internal boot detected; starting disk preparation")
			s.storageMgr.StartPartitioningAsync(context.Background())
		case s.onboardingMgr.State() == onboarding.StateTryPiccolo ||
			s.onboardingMgr.State() == onboarding.StateComplete:
			log.Printf("INFO: returning USB user (state=%s); starting disk preparation", s.onboardingMgr.State())
			s.storageMgr.StartPartitioningAsync(context.Background())
		default:
			// USB/unknown boot with state=pending. Check if the system was
			// previously set up (e.g. disk moved between controllers). If so,
			// auto-advance onboarding to try_piccolo and start partitioning
			// so the user sees the unlock screen instead of onboarding.
			if s.storageMgr.IsPreviouslySetUp(context.Background()) {
				log.Printf("INFO: previously set up system detected on %s boot; auto-advancing onboarding", s.onboardingMgr.BootMode())
				if err := s.onboardingMgr.Choose(onboarding.StateTryPiccolo); err != nil {
					log.Printf("WARN: failed to auto-advance onboarding: %v", err)
				}
				s.storageMgr.StartPartitioningAsync(context.Background())
			} else {
				log.Printf("INFO: boot mode is %s, onboarding state is %s; deferring disk preparation to onboarding",
					s.onboardingMgr.BootMode(), s.onboardingMgr.State())
			}
		}
	}

	s.startSecureLoopback()

	// Initialize internal CA and HTTPS listener from network-bootstrap dir (pre-unlock).
	// This ensures HTTPS is available for the unlock page and CA download.
	if err := s.ensureInternalCA(); err != nil {
		log.Printf("WARN: internal CA initialization failed (HTTPS unavailable): %v", err)
	} else {
		s.refreshServerCertSANs()
		s.startInternalHTTPSListener()
		s.subscribeCertRefresh()
	}

	log.Printf("INFO: Starting piccolod server with Gin on http://localhost:%s", port)

	// Notify systemd that we're ready (for Type=notify services)
	// This enables proper health checking and rollback functionality in MicroOS
	if sent, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
		log.Printf("WARN: Failed to notify systemd of readiness: %v", err)
	} else if sent {
		log.Printf("INFO: Notified systemd that service is ready")
	}

	s.httpSrv = &http.Server{
		Addr:     ":" + port,
		Handler:  s.router,
		ErrorLog: newFilteredErrorLogger(),
	}
	if err := s.httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// runWatchdogLoop pings the systemd service-level watchdog at half the
// configured WatchdogSec interval. If piccolod gets stuck (e.g. all threads
// blocked in D-state during a btrfs hang), pings stop and systemd restarts
// the service. The loop is a no-op when WatchdogSec is not configured.
func (s *GinServer) runWatchdogLoop(ctx context.Context) {
	interval, err := daemon.SdWatchdogEnabled(false)
	if err != nil || interval == 0 {
		log.Printf("INFO: systemd watchdog not configured, skipping keepalive loop")
		return
	}
	tick := interval / 2
	if tick <= 0 {
		tick = interval
	}
	log.Printf("INFO: systemd watchdog enabled, pinging every %s (timeout=%s)", tick, interval)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	var notifyFailed bool
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := daemon.SdNotify(false, daemon.SdNotifyWatchdog); err != nil && !notifyFailed {
				log.Printf("WARN: watchdog ping failed (suppressing future errors): %v", err)
				notifyFailed = true
			}
		}
	}
}

// Stop gracefully shuts down the server and all its components using a
// three-phase approach: FENCE (close listeners), DRAIN (stop apps/work),
// CLEANUP (unmount volumes).
// The context is used for overall timeout control during shutdown.
func (s *GinServer) Stop(ctx context.Context) error {
	log.Printf("INFO: Beginning graceful shutdown...")

	// Prevent identity event subscriber from racing with shutdown
	s.namekStopped.Store(true)
	s.stopNamekAdapter()

	// Cancel in-flight domain reconciliation to prevent stale network calls during shutdown
	s.namekMu.Lock()
	if s.namekReconcileStop != nil {
		s.namekReconcileStop()
		s.namekReconcileStop = nil
	}
	s.namekMu.Unlock()

	// Notify systemd that we're stopping
	if sent, err := daemon.SdNotify(false, daemon.SdNotifyStopping); err != nil {
		log.Printf("WARN: Failed to notify systemd of stopping: %v", err)
	} else if sent {
		log.Printf("INFO: Notified systemd that service is stopping")
	}

	// ── Phase 1: FENCE (5s) ─────────────────────────────────────────────
	// Close all listeners to stop accepting new connections and drain in-flight requests.
	log.Printf("INFO: Phase 1/3: FENCE — closing listeners")
	fenceCtx, fenceCancel := context.WithTimeout(ctx, 5*time.Second)
	defer fenceCancel()
	if s.httpSrv != nil {
		if err := s.httpSrv.Shutdown(fenceCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("WARN: HTTP server shutdown: %v", err)
		}
	}
	s.stopSecureLoopback(fenceCtx)
	s.stopInternalHTTPSListener(fenceCtx)
	if s.certRefreshUnsub != nil {
		s.certRefreshUnsub()
		s.certRefreshUnsub = nil
	}
	log.Printf("INFO: Phase 1/3: FENCE complete")

	// ── Phase 2: DRAIN (60s) ────────────────────────────────────────────
	// Stop app event observers, reconciliation, containers, and detach app volumes.
	log.Printf("INFO: Phase 2/3: DRAIN — stopping apps and background work")
	drainCtx, drainCancel := context.WithTimeout(ctx, 60*time.Second)
	defer drainCancel()
	if s.appManager != nil {
		s.appManager.StopRuntimeEvents()
		if err := s.appManager.StopAllApps(drainCtx); err != nil {
			log.Printf("WARN: Failed to stop all apps cleanly: %v", err)
		}
	}
	s.oidcProviderMu.Lock()
	if s.oidcProvider != nil {
		s.oidcProvider.Storage().Close()
	}
	s.oidcProviderMu.Unlock()
	log.Printf("INFO: Phase 2/3: DRAIN complete")

	// ── Phase 3: CLEANUP ────────────────────────────────────────────────
	// Stop supervisor components (mDNS goodbye, services) and unmount volumes.
	log.Printf("INFO: Phase 3/3: CLEANUP — supervisor stop and volume unmount")
	if err := s.supervisor.Stop(ctx); err != nil {
		log.Printf("WARN: Failed to stop components cleanly: %v", err)
	}
	if s.persistence != nil {
		if err := s.persistence.Shutdown(ctx); err != nil {
			log.Printf("WARN: Failed to shutdown persistence cleanly: %v", err)
		}
	}
	// Close TPM device (identity service does NOT own TPM lifecycle)
	s.tpmMu.Lock()
	tr := s.tpmResult
	s.tpmResult = nil
	s.tpmMu.Unlock()
	if tr != nil {
		if err := tr.Close(); err != nil {
			log.Printf("WARN: TPM close: %v", err)
		}
	}
	log.Printf("INFO: Phase 3/3: CLEANUP complete")

	log.Printf("INFO: Graceful shutdown completed")
	return nil
}

func (s *GinServer) portalOriginForRequest(r *http.Request) string {
	if s == nil || r == nil {
		return ""
	}

	// WAN via Nexus proxy: RemoteAddr is loopback. Use the resolver to map
	// the request Host to its portal hostname — source-agnostic (works for both
	// self-hosted and namek traffic without knowing which adapter delivered it).
	remoteHost, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(remoteHost); ip != nil && ip.IsLoopback() {
		if s.remoteResolver != nil {
			if portal := s.remoteResolver.PortalHostForRequest(canonicalHost(r.Host)); portal != "" {
				return "https://" + portal
			}
		}
	}

	scheme := "http"
	if s.isSecureRequest(r) || services.RequestArrivedViaTLS(r) {
		scheme = "https"
	}
	defaultPort := 80
	if scheme == "https" {
		defaultPort = 443
	}

	// Use piccolod's portal port (PORT), not the app listener port from r.Host.
	envPort := 80
	if p := os.Getenv("PORT"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v > 0 {
			envPort = v
		}
	}
	portalPort := 0
	switch scheme {
	case "http":
		portalPort = envPort
	case "https":
		// When the request arrived over TLS (r.TLS or secureContextKeyInstance), the scheme
		// is https. PORT typically reflects the HTTP listener (e.g. 80); do not
		// append :80 to an https origin.
		if envPort != 80 {
			portalPort = envPort
		}
	}

	host := ""
	// Honor access mode: if the request arrived via IP, use that IP as the portal
	// host instead of the mDNS hostname. Validate via LocalAddrContextKey to ensure
	// the claimed Host IP matches the actual interface that received the connection.
	reqHost := canonicalHost(r.Host)
	if reqIP := net.ParseIP(reqHost); reqIP != nil && !reqIP.IsLoopback() {
		if localAddr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
			if localHost, _, err := net.SplitHostPort(localAddr.String()); err == nil {
				if localIP := net.ParseIP(localHost); localIP != nil && localIP.Equal(reqIP) {
					host = reqHost
				}
			}
		}
	}
	if host == "" && s.mdnsManager != nil {
		host = strings.TrimSpace(s.mdnsManager.Hostname())
	}
	if host == "" {
		host = reqHost
	}
	if host == "" {
		host = getPreferredOutboundIP()
	}
	if host == "" {
		return ""
	}
	if portalPort != 0 && portalPort != defaultPort {
		host = net.JoinHostPort(host, strconv.Itoa(portalPort))
	} else if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		// Bracket bare IPv6 for valid URI syntax when port is omitted (default port).
		// net.JoinHostPort handles this in the non-default-port branch above.
		host = "[" + host + "]"
	}
	return scheme + "://" + host
}

// setupGinRoutes defines all API endpoints using Gin router.
func (s *GinServer) setupGinRoutes() {
	r := gin.New()
	// For API routes, prefer deterministic 404s over implicit redirects.
	r.RedirectTrailingSlash = false
	r.RedirectFixedPath = false

	// Add basic middleware
	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	// LAN host routing must run BEFORE gzip and security headers:
	// 1. Before gzip: proxied app responses are already compressed; gzip middleware
	//    would strip Content-Encoding and corrupt the response.
	// 2. Before security headers: avoid portal-only headers (e.g., X-Frame-Options: DENY,
	//    Cross-Origin-Embedder-Policy) leaking into app responses.
	r.Use(s.lanHostRoutingMiddleware())
	r.Use(gzip.Gzip(gzip.DefaultCompression))
	r.Use(s.corsMiddleware())
	r.Use(s.httpsRedirectMiddleware())
	r.Use(s.securityHeadersMiddleware())

	// Optional: OpenAPI request validation (enabled when validator is initialized)
	if s.apiValidator == nil {
		// Try lazy init based on env var
		if os.Getenv("PICCOLO_API_VALIDATE") == "1" {
			if v, err := newOpenAPIValidator(); err == nil {
				s.apiValidator = v
			} else {
				log.Printf("OpenAPI validation disabled: %v", err)
			}
		}
	}
	if s.apiValidator != nil {
		r.Use(s.apiValidator.Middleware())
	}

	// ACME HTTP-01 challenge for portal hostname
	r.GET("/.well-known/acme-challenge/:token", func(c *gin.Context) {
		if s.remoteManager == nil {
			c.Status(http.StatusNotFound)
			return
		}
		h := s.remoteManager.HTTPChallengeHandler()
		if h == nil {
			c.Status(http.StatusNotFound)
			return
		}
		// Delegate to handler (ensures correct content-type and body)
		h.ServeHTTP(c.Writer, c.Request)
	})

	// OIDC Discovery
	r.GET("/.well-known/openid-configuration", s.handleOIDCDiscovery)

	// OIDC Endpoints
	r.GET("/oauth/authorize", s.handleOIDCAuthorize)
	r.POST("/oauth/authorize", s.handleOIDCAuthorize) // Support POST for form submissions
	r.POST("/oauth/token", s.handleOIDCToken)
	r.GET("/oauth/jwks", s.handleOIDCJwks)
	r.GET("/oauth/userinfo", s.handleOIDCUserinfo)
	r.POST("/oauth/userinfo", s.handleOIDCUserinfo)
	r.POST("/oauth/revoke", s.handleOIDCRevoke)
	r.POST("/oauth/introspect", s.handleOIDCIntrospect)
	r.GET("/oauth/logout", s.handleOIDCLogout)
	r.POST("/oauth/logout", s.handleOIDCLogout)

	// Emergency middleware: block most API access when storage is in emergency mode.
	r.Use(s.emergencyMiddleware())

	// API v1 group
	v1 := r.Group("/api/v1")
	{
		// Serve embedded OpenAPI document for tooling/debug (no auth)
		v1.GET("/openapi.yaml", func(c *gin.Context) {
			if b, err := loadOpenAPISpec(); err == nil {
				c.Data(http.StatusOK, "application/yaml; charset=utf-8", b)
			} else {
				c.JSON(http.StatusNotFound, gin.H{"error": "spec not found"})
			}
		})

		// Auth & sessions (selected public endpoints)
		v1.GET("/auth/session", s.handleAuthSession)
		v1.GET("/auth/initialized", s.handleAuthInitialized)
		v1.GET("/auth/validate-next", s.handleAuthValidateNext)
		v1.POST("/auth/login", s.handleAuthLogin)

		// Onboarding endpoints (public, no auth — needed pre-setup)
		v1.GET("/system/onboarding", s.handleOnboardingStatus)
		v1.POST("/system/onboarding", s.handleOnboardingChoice)
		v1.GET("/storage/disks", s.handleOnboardingDisks)
		v1.GET("/system/install-progress/stream", s.handleInstallProgressStream)

		// Install to Disk and reboot (LAN-only + conditional auth in handlers)
		lanInstall := v1.Group("/")
		lanInstall.Use(s.allowLANOnly())
		lanInstall.POST("/system/install-to-disk", s.handleInstallToDisk)
		lanInstall.POST("/system/reboot", s.handleOnboardingReboot)

		// Selected read-only status endpoints remain public
		v1.GET("/remote/status", s.handleRemoteStatus)
		v1.GET("/health/live", s.handleHealthLive)
		v1.GET("/health/ready", s.handleGinReadinessCheck)
		v1.GET("/health/detail", s.handleHealthDetail)

		// Storage emergency status (public)
		v1.GET("/system/emergency", s.handleEmergencyStatus)

		// Diagnostic log download (public, LAN-only, gated by error health state)
		lanDiag := v1.Group("/")
		lanDiag.Use(s.allowLANOnly())
		lanDiag.Use(s.requireUnhealthy())
		lanDiag.GET("/system/diagnostic-log", s.handleDiagnosticLog)

		// PCV import (public — used during setup/recovery when no auth exists)
		v1.POST("/system/pcv/import", s.handlePCVImport)

		// Allow unlocking without a session to break the initial lock/setup cycle.
		// Crypto: expose status/setup/unlock publicly to break circular dependency with sessions.
		v1.GET("/crypto/status", s.handleCryptoStatus)
		v1.POST("/crypto/setup", s.handleCryptoSetup)
		v1.POST("/crypto/unlock", s.handleCryptoUnlock)
		v1.POST("/crypto/reset-password", s.handleCryptoResetPassword)
		v1.GET("/crypto/recovery-key", s.handleCryptoRecoveryStatus)

		// Network discovery (public, LAN-only data)
		v1.GET("/network/peers", s.handleNetworkPeers)

		// CA certificate download (public - needed before login for HTTPS setup)
		v1.GET("/system/ca.crt", s.handleCADownload)

		// Icon proxy (public, read-only - icons are public catalog metadata;
		// unauthenticated so Image.network/SvgPicture.network work cross-origin in dev)
		v1.GET("/catalog/:name/icon", s.handleGinCatalogIcon)

		// All other API endpoints require session + CSRF
		authed := v1.Group("/")
		authed.Use(s.requireSession())
		authed.Use(s.csrfMiddleware())

		// Create Admin-only group
		admin := authed.Group("/")
		admin.Use(s.requireAdmin())

		// Crypto endpoints (session required for lock/recovery management)
		admin.POST("/crypto/lock", s.handleCryptoLock)
		admin.POST("/crypto/recovery-key/generate", s.handleCryptoRecoveryGenerate)

		// App management endpoints
		apps := authed.Group("/apps")
		{
			// Read-only / User-specific access (Handlers must enforce filtering)
			apps.GET("/check-instance", s.handleGinAppCheckInstance)
			apps.GET("", s.handleGinAppList)
			apps.GET("/:name", s.handleGinAppGet)
			apps.GET("/:name/services", s.handleGinServicesByApp)
			apps.GET("/:name/listeners/:listener/health", s.handleGinListenerHealth)
			apps.GET("/:name/logs", s.handleGinAppLogs)
			apps.GET("/:name/logs/stream", s.handleGinAppLogStream)

			// Admin-only actions
			apps.POST("", s.requireUnlocked(), s.requireAdmin(), s.handleGinAppInstall)
			apps.POST("/validate", s.requireAdmin(), s.handleGinAppValidate)
			apps.POST("/preflight", s.requireAdmin(), s.handleGinAppPreflight)
			apps.DELETE("/:name", s.requireUnlocked(), s.requireAdmin(), s.handleGinAppUninstall)
			apps.PATCH("/:name/listeners", s.requireUnlocked(), s.requireAdmin(), s.handleGinAppUpdateListeners)
			apps.POST("/:name/start", s.requireUnlocked(), s.requireAdmin(), s.handleGinAppStart)
			apps.POST("/:name/stop", s.requireUnlocked(), s.requireAdmin(), s.handleGinAppStop)
			apps.POST("/:name/update", s.requireUnlocked(), s.requireAdmin(), s.handleGinAppUpdate)
			apps.POST("/:name/rollback", s.requireUnlocked(), s.requireAdmin(), s.handleGinAppRollback)
			apps.POST("/:name/clone", s.requireUnlocked(), s.requireAdmin(), s.handleGinAppClone)
			apps.GET("/:name/clones", s.requireAdmin(), s.handleGinAppListClones)
			apps.GET("/:name/terminal", s.requireAdmin(), s.handleWorkspaceTerminal)

			// Persistent container terminal sessions (Admin only)
			apps.POST("/:name/terminal/sessions", s.requireAdmin(), s.handleCreateWorkspaceTerminalSession)
			apps.GET("/:name/terminal/sessions", s.requireAdmin(), s.handleListWorkspaceTerminalSessions)
			apps.DELETE("/:name/terminal/sessions/:id", s.requireAdmin(), s.handleDeleteWorkspaceTerminalSession)
			apps.GET("/:name/terminal/sessions/:id/attach", s.requireAdmin(), s.handleAttachWorkspaceTerminalSession)
		}

		// Image search (Admin only)
		admin.GET("/images/search", s.handleImageSearch)

		// System logs (Admin only)
		admin.GET("/system/logs/stream", s.handleGinSystemLogStream)

		// Diagnostic log download (Admin only, always available)
		admin.GET("/system/admin/diagnostic-log", s.handleDiagnosticLog)
		admin.GET("/system/network-check", s.handleNetworkCheck)
		admin.GET("/system/storage-check", s.handleStorageCheck)
		admin.GET("/system/storage-diagnostics", s.handleStorageDiagnostics)

		// Active tasks discovery (Admin only)
		admin.GET("/tasks/active", s.handleActiveTasks)

		// Task progress (Admin only?) - Maybe standard user needs to see progress of their own actions?
		// But they can't trigger actions. So Admin only is safe.
		// Actually, let's allow it for now as it's harmless read-only.
		authed.GET("/events/progress/stream", s.handleGinTaskProgressStream)

		// Unified event stream - streams app status, listener health, remote config, certificates
		authed.GET("/events/stream", s.handleGinEventStream)

		// Remote config endpoints (Admin only)
		remote := admin.Group("/remote")
		{
			remote.POST("/configure", s.handleRemoteConfigure)
			remote.POST("/managed/configure", s.handleRemoteManagedConfigure)
			remote.POST("/disable", s.handleRemoteDisable)
			remote.POST("/rotate", s.handleRemoteRotate)
			remote.POST("/preflight", s.handleRemotePreflight)
			remote.GET("/aliases", s.handleRemoteAliasesList)
			remote.POST("/aliases", s.handleRemoteAliasesCreate)
			remote.DELETE("/aliases/:id", s.handleRemoteAliasesDelete)
			remote.POST("/aliases/:id/verify-namek", s.handleRemoteAliasesVerifyNamek)
			remote.GET("/certificates", s.handleRemoteCertificatesList)
			remote.POST("/certificates/:id/renew", s.handleRemoteCertificateRenew)
			remote.GET("/events", s.handleRemoteEvents)
			remote.GET("/nexus-guide", s.handleRemoteGuideInfo)
			remote.POST("/nexus-guide/verify", s.handleRemoteGuideVerify)
		}

		// Identity / namek endpoints (Admin only)
		s.registerIdentityRoutes(admin)

		// PCV export/import (Admin only)
		admin.POST("/system/pcv/publish", s.requireUnlocked(), s.handlePCVPublish)
		admin.GET("/system/pcv/export", s.requireUnlocked(), s.handlePCVExport)

		// Auth-only endpoints (Accessible to all logged-in users)
		authed.POST("/auth/logout", s.handleAuthLogout)
		authed.POST("/auth/password", s.handleAuthPassword)
		authed.POST("/auth/staleness/ack", s.handleAuthStalenessAck)
		authed.GET("/auth/csrf", s.handleAuthCSRF)
		authed.POST("/oauth/resume", s.handleOIDCResume)

		// UI telemetry (Admin only)
		admin.POST("/telemetry/log", s.handleTelemetryLog)

		// Debug terminal (Admin only) — legacy ephemeral endpoint
		admin.GET("/terminal", s.handleTerminal)

		// Persistent terminal sessions (Admin only)
		termSessions := admin.Group("/terminal/sessions")
		{
			termSessions.POST("", s.handleCreateHostTerminalSession)
			termSessions.GET("", s.handleListHostTerminalSessions)
			termSessions.DELETE("/:id", s.handleDeleteHostTerminalSession)
			termSessions.GET("/:id/attach", s.handleAttachHostTerminalSession)
		}

		// OS updates (Admin only)
		updates := admin.Group("/updates/os")
		{
			updates.GET("", s.handleOSUpdateStatus)
			updates.POST("/apply", s.requireUnlocked(), s.handleOSUpdateApply)
			updates.POST("/rollback", s.requireUnlocked(), s.handleOSUpdateRollback)
			updates.POST("/reboot", s.requireUnlocked(), s.handleOSUpdateReboot)
		}

		// User management (Admin only)
		users := admin.Group("/users")
		{
			users.GET("", s.handleListUsers)
			users.POST("", s.handleCreateUser)
			users.GET("/:id", s.handleGetUser)
			users.PUT("/:id", s.handleUpdateUser)
			users.DELETE("/:id", s.handleDeleteUser)
			users.POST("/:id/password", s.handleSetUserPassword)
		}

		// Catalog (read-only) - Allow standard users to view catalog?
		// RFC says "Restricted to allowed_apps".
		// If they can't install, viewing catalog is useless or maybe confusing.
		// Let's restrict to Admin for now to be safe.
		admin.GET("/catalog", s.handleGinCatalog)
		admin.GET("/catalog/categories", s.handleGinCatalogCategories)
		admin.GET("/catalog/:name/template", s.handleGinCatalogTemplate)
		admin.GET("/catalog/:name/configure", s.handleGinCatalogConfigure)
		// Icon proxy is registered below outside admin group (public, read-only)

		// Services list (Needs filtering)
		authed.GET("/services", s.handleGinServicesAll)
	}

	// Admin routes
	r.GET("/version", s.handleGinVersion)

	// Static file serving for web UI and fallback
	r.NoRoute(func(c *gin.Context) {
		if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
			requestedPath := c.Request.URL.Path
			if strings.HasPrefix(requestedPath, "/api/") || strings.HasPrefix(requestedPath, "/oauth/") {
				c.Status(http.StatusNotFound)
				return
			}
			if strings.HasSuffix(requestedPath, "/") {
				requestedPath += "entry.html"
			}

			fspath := "web" + requestedPath
			if _, err := fs.Stat(webassets.FS, fspath); err != nil {
				fspath = "web/entry.html"
			}
			if etag := s.staticCache.ETag(fspath); etag != "" {
				c.Header("Cache-Control", cachePolicy(fspath))
				c.Header("ETag", etag)
			}
			c.FileFromFS(fspath, http.FS(webassets.FS))
		} else {
			c.Status(http.StatusNotFound)
		}
	})

	s.router = r
}

// handleGinServicesAll returns all service endpoints across apps
func (s *GinServer) handleGinServicesAll(c *gin.Context) {
	eps := s.serviceManager.GetAll()
	out := make([]gin.H, 0, len(eps))
	portalHosts := s.portalHosts()

	// Filter for standard users
	var allowedApps map[string]struct{}
	if sess := s.getSessionFromContext(c); sess != nil && sess.Role != "admin" {
		if s.userManager != nil {
			user, err := s.userManager.Get(c.Request.Context(), sess.UserID)
			if err != nil {
				writeGinSuccess(c, []gin.H{}, "Found 0 services")
				return
			}
			allowedApps = make(map[string]struct{})
			for _, id := range user.AllowedApps {
				allowedApps[id] = struct{}{}
			}
		}
	}

	for _, ep := range eps {
		if allowedApps != nil {
			if _, ok := allowedApps[ep.App]; !ok {
				continue
			}
		}
		out = append(out, s.formatServiceEndpoint(c, ep, portalHosts))
	}
	c.JSON(http.StatusOK, gin.H{"services": out})
}

// handleGinServicesByApp returns services for a single app
func (s *GinServer) handleGinServicesByApp(c *gin.Context) {
	name := c.Param("name")

	// Check access for standard users
	if sess := s.getSessionFromContext(c); sess != nil && sess.Role != "admin" {
		if s.userManager != nil {
			allowed, err := s.userManager.IsAppAllowed(c.Request.Context(), sess.UserID, name)
			if err != nil || !allowed {
				writeGinError(c, http.StatusForbidden, "Access denied")
				return
			}
		}
	}

	eps, err := s.serviceManager.GetByApp(name)
	if err != nil {
		writeGinError(c, http.StatusNotFound, err.Error())
		return
	}
	out := make([]gin.H, 0, len(eps))
	svcPortalHosts := s.portalHosts()
	for _, ep := range eps {
		formatted := s.formatServiceEndpoint(c, ep, svcPortalHosts)
		// Add listener health status (RFC 20260125)
		formatted["health"] = s.computeListenerHealth(ep)
		out = append(out, formatted)
	}
	c.JSON(http.StatusOK, gin.H{"services": out})
}

func (s *GinServer) formatServiceEndpoint(c *gin.Context, ep services.ServiceEndpoint, portalHosts []string) gin.H {
	remoteHost := s.contextAwareRemoteHost(c, ep, portalHosts)
	var remoteHostValue interface{}
	if remoteHost != "" {
		remoteHostValue = remoteHost
	}
	scheme := determineScheme(ep.Flow, ep.Protocol)
	lanPortURL := s.determineLocalURL(c, ep, scheme)

	result := gin.H{
		"app":                ep.App,
		"name":               ep.Name,
		"guest_port":         ep.GuestPort,
		"host_port":          ep.HostBind,
		"public_port":        ep.PublicPort,
		"remote_ports":       ep.RemotePorts,
		"remote_host":        remoteHostValue,
		"flow":               ep.Flow,
		"protocol":           ep.Protocol,
		"primary":            ep.Primary,
		"derived_host_label": ep.DerivedHostLabel,
		"middleware":         ep.Middleware,
		"scheme":             scheme,
		"local_url":          lanPortURL, // Keep for backward compatibility
		"lan_port_url":       lanPortURL, // New explicit name
	}

	if ep.Auth != nil {
		result["auth"] = ep.Auth
	}
	if ep.PortClaim != nil {
		result["port_claim"] = *ep.PortClaim
	}

	// Add host-based URLs only for HTTP/WS listeners (per RFC 20260114)
	if ep.DerivedHostLabel != "" {
		// LAN host URL: only if mDNS is enabled (mdnsManager is nil when disabled)
		if s.mdnsManager != nil {
			lanBase := s.mdnsManager.Hostname()
			// RFC 20260122 §4.4: Use 2-level mDNS format with hyphen separator
			lanHostname := hostnamepkg.DeriveLANHostname(ep.DerivedHostLabel, lanBase)
			// Honor request scheme: host-based URLs route through the :443 TLS mux
			// when the portal is on HTTPS, so return https:// directly instead of
			// forcing every frontend consumer to upgrade the scheme.
			lanScheme := scheme
			if s.isSecureRequest(c.Request) && (scheme == "http" || scheme == "ws") {
				if scheme == "http" {
					lanScheme = "https"
				} else {
					lanScheme = "wss"
				}
			}
			lanHostURL := fmt.Sprintf("%s://%s", lanScheme, lanHostname)
			result["lan_host_url"] = lanHostURL
		}

		// remote_url: context-aware, derived from the portal matching the request's Host header.
		// Used by the UI for iframe embedding so the app opens on the same nexus the user arrived through.
		if remoteHost != "" {
			result["remote_url"] = "https://" + remoteHost
		}

		// remote_hosts: complete list across all active portals. Used by the network tab
		// to show every access point. Not context-aware — always returns all portals.
		if allHosts := allRemoteHostsForEndpoint(ep, portalHosts); len(allHosts) > 0 {
			result["remote_hosts"] = allHosts
		}
	}

	// Add request-independent LAN fallback URL (RFC 20260125)
	// This is used by UI for "Access Locally" overlay when remote access is degraded.
	if ep.DerivedHostLabel != "" && s.mdnsManager != nil {
		// Host-based fallback via mDNS
		lanBase := s.mdnsManager.Hostname()
		lanHostname := hostnamepkg.DeriveLANHostname(ep.DerivedHostLabel, lanBase)
		result["lan_fallback_url"] = fmt.Sprintf("%s://%s", scheme, lanHostname)
	} else {
		// Port-based fallback using device's preferred outbound IP
		// Use computed scheme to match listener protocol (http/https/ws/wss)
		if deviceIP := getPreferredOutboundIP(); deviceIP != "" {
			result["lan_fallback_url"] = fmt.Sprintf("%s://%s:%d", scheme, deviceIP, ep.PublicPort)
		}
	}

	return result
}

func (s *GinServer) determineLocalURL(c *gin.Context, ep services.ServiceEndpoint, scheme string) *string {
	r := c.Request

	// Suppress local URLs only for nexus/TLS-mux proxied requests (remote access).
	// Identified by secureContextKey set in the secure loopback handler.
	// Internal HTTPS requests (r.TLS != nil from :443 listener) are still LAN requests
	// and should receive local URLs so the UI can offer new-tab fallback.
	// X-Forwarded-Proto is NOT trusted (spoofable by any client).
	if r.Context().Value(secureContextKeyInstance) != nil {
		return nil
	}

	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	url := fmt.Sprintf("%s://%s:%d", scheme, host, ep.PublicPort)
	return &url
}

// remoteBaseHostname returns the portal hostname from a remote.Status.
// Used by wiring code and event-driven cert orchestration.
func remoteBaseHostname(status *remote.Status) string {
	if status == nil || !status.Enabled {
		return ""
	}
	return remotePortalBase(status)
}

// remotePortalBase extracts the normalized base hostname from PortalHostname
// without gating on Enabled. Used for cert cleanup where we need the base
// even if remote access has been disabled.
func remotePortalBase(status *remote.Status) string {
	if status == nil {
		return ""
	}
	base := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(status.PortalHostname)), ".")
	if base == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(base); err == nil {
		base = h
	}
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(base)), ".")
}

// contextAwareRemoteHost returns the remote hostname for the endpoint derived from
// the portal that matches the current request's Host header. Falls back to the first
// portal when no match is found (e.g., LAN access).
func (s *GinServer) contextAwareRemoteHost(c *gin.Context, ep services.ServiceEndpoint, portalHosts []string) string {
	if ep.DerivedHostLabel == "" || s.remoteResolver == nil {
		return ""
	}
	reqHost := canonicalHost(c.Request.Host)
	if portal := s.remoteResolver.PortalHostForRequest(reqHost); portal != "" {
		if h := services.RemoteServiceHostname(ep.DerivedHostLabel, portal); h != "" {
			return h
		}
	}
	// LAN access or unrecognized host — fall back to first portal
	return remoteHostForEndpoint(ep, portalHosts)
}

// remoteHostForEndpoint returns the first matching remote hostname for the given endpoint
// by checking all active portal hosts. Returns "" if no portal is active.
func remoteHostForEndpoint(ep services.ServiceEndpoint, portalHosts []string) string {
	for _, portal := range portalHosts {
		if h := services.RemoteServiceHostname(ep.DerivedHostLabel, portal); h != "" {
			return h
		}
	}
	return ""
}

// allRemoteHostsForEndpoint returns the remote hostname for every active portal.
func allRemoteHostsForEndpoint(ep services.ServiceEndpoint, portalHosts []string) []string {
	if ep.DerivedHostLabel == "" {
		return nil
	}
	var hosts []string
	for _, portal := range portalHosts {
		if h := services.RemoteServiceHostname(ep.DerivedHostLabel, portal); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

func (s *GinServer) handleGinVersion(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"version": s.version,
		"service": "piccolod",
	})
}

func (s *GinServer) registerUnlockReloader(r unlockReloader) {
	if s == nil || r == nil {
		return
	}
	s.reloadersMu.Lock()
	s.unlockReloaders = append(s.unlockReloaders, r)
	s.reloadersMu.Unlock()
}

func (s *GinServer) reloadComponentsAfterUnlock() {
	if s == nil {
		return
	}

	// CA and HTTPS listener are initialized at Start() from network-bootstrap dir.
	// If pre-unlock init failed (e.g., directory not ready), retry now.
	if s.internalCA == nil {
		if err := s.ensureInternalCA(); err != nil {
			log.Printf("WARN: internal CA retry after unlock failed: %v", err)
		} else {
			s.refreshServerCertSANs()
			s.startInternalHTTPSListener()
			s.subscribeCertRefresh()
		}
	} else {
		// Refresh cert SANs after unlock in case mDNS hostnames changed.
		s.refreshServerCertSANs()
	}

	s.reloadersMu.RLock()
	reloaders := append([]unlockReloader(nil), s.unlockReloaders...)
	s.reloadersMu.RUnlock()
	for _, r := range reloaders {
		if r == nil {
			continue
		}
		if err := r.ReloadFromStorage(); err != nil {
			log.Printf("WARN: unlock reload failed: %v", err)
		}
	}
}

// subscribeCertRefresh watches for hostname/leadership changes to regenerate cert SANs.
// subscribeCertRefresh listens for TopicHostnamesChanged events (emitted by the
// mDNS NameRegistry after its hostname snapshot is updated) and regenerates the
// server TLS certificate with the current SAN list. This replaces the previous
// approach of subscribing to raw TopicServiceEndpointsChanged / TopicLeadershipRoleChanged
// events which raced with the mDNS debounce, causing stale SAN lists.
func (s *GinServer) subscribeCertRefresh() {
	if s.certRefreshUnsub != nil {
		s.certRefreshUnsub()
	}
	if s.events == nil || s.internalCA == nil {
		return
	}

	ch, unsub := s.events.SubscribeWithCancel(events.TopicHostnamesChanged, 4)
	refreshCh := make(chan struct{}, 1)
	done := make(chan struct{})
	s.certRefreshUnsub = func() {
		unsub()
		close(done)
	}
	trigger := func() {
		select {
		case refreshCh <- struct{}{}:
		default: // already pending
		}
	}
	go func() {
		for range ch {
			trigger()
		}
	}()
	go func() {
		for {
			select {
			case <-refreshCh:
				s.refreshServerCertSANs()
			case <-done:
				return
			}
		}
	}()
}

// ensureInternalCA initializes the internal CA from the network-bootstrap directory.
// The CA is stored outside the encrypted control volume so HTTPS is available pre-unlock.
// The network-bootstrap directory is always present on the root filesystem.
func (s *GinServer) ensureInternalCA() error {
	if s == nil {
		return nil
	}

	s.internalCAMu.Lock()
	defer s.internalCAMu.Unlock()

	// Already initialized
	if s.internalCA != nil {
		return nil
	}

	caDir := paths.CoreJoin("network-bootstrap")
	ca, err := pki.NewInternalCA(caDir)
	if err != nil {
		return fmt.Errorf("internal CA init: %w", err)
	}
	if err := ca.EnsureServerCertificate(); err != nil {
		return fmt.Errorf("ensure server cert: %w", err)
	}

	s.internalCA = ca
	if s.appManager != nil {
		s.appManager.SetInternalCAPath(ca.CertPath())
		if s.mdnsManager != nil {
			s.appManager.SetOIDCHostname(s.mdnsManager.SpecificHostname())
		}
	}

	log.Printf("INFO: internal CA initialized from %s", caDir)
	return nil
}

// handleCADownload serves the internal CA certificate for browser trust.
func (s *GinServer) handleCADownload(c *gin.Context) {
	s.internalCAMu.Lock()
	ca := s.internalCA
	s.internalCAMu.Unlock()
	if ca == nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="piccolo-ca.crt"`)
	c.Data(http.StatusOK, "application/x-x509-ca-cert", ca.CertPEM())
}

func (s *GinServer) refreshRemoteRuntime() {
	if s == nil || s.remoteManager == nil {
		return
	}
	status := s.remoteManager.Status()
	s.applyRemoteRuntimeFromStatus(status)
}

func (s *GinServer) applyRemoteRuntimeFromStatus(status remote.Status) {
	if s == nil || s.tlsMux == nil {
		return
	}
	// Build alias map from status — shared between resolver and TlsMux.
	aliasMap := make(map[string]string, len(status.Aliases))
	for _, a := range status.Aliases {
		h := strings.TrimSuffix(strings.ToLower(a.Hostname), ".")
		if h == "" {
			continue
		}
		hostLabel := a.Listener
		if hostLabel == "" || hostLabel == "portal" {
			hostLabel = nexusclient.PortalHostLabel
		}
		aliasMap[h] = hostLabel
	}

	if s.remoteResolver != nil {
		s.remoteResolver.UpdateAliases(aliasMap)
	}
	s.tlsMux.UpdateAliases(aliasMap)

	// Set self-hosted remote bases on resolver and TLS mux
	if status.Enabled && strings.TrimSpace(status.PortalHostname) != "" {
		portal := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(status.PortalHostname)), ".")
		if s.remoteResolver != nil {
			s.remoteResolver.SetRemoteBases("self-hosted", []remoteBase{
				{source: "self-hosted", portalHost: portal, domain: portal},
			})
		}
		s.tlsMux.SetRemoteBases("self-hosted", []services.TlsMuxBase{
			{Source: "self-hosted", PortalHost: portal, Domain: portal},
		})
	} else {
		if s.remoteResolver != nil {
			s.remoteResolver.SetRemoteBases("self-hosted", nil)
		}
		s.tlsMux.SetRemoteBases("self-hosted", nil)
	}

	s.recomputeFrameAncestors()

	// RFC 20260114: remote base is the portal hostname apex; app hosts are <label>.<base>.
	s.tlsMux.UpdateConfig(status.PortalHostname, status.PortalHostname, s.resolvePortalPort())
	if status.Enabled && strings.TrimSpace(status.PortalHostname) != "" {
		if port, err := s.tlsMux.Start(); err == nil {
			if s.remoteResolver != nil {
				s.remoteResolver.SetTlsMuxPort(port)
			}
		} else {
			log.Printf("WARN: TLS mux start failed: %v", err)
		}
	} else {
		// Only stop TLS mux if no other remote source needs it (e.g., namek-only setups).
		hasOtherBases := len(s.portalHosts()) > 0
		if !hasOtherBases {
			s.tlsMux.Stop()
			if s.remoteResolver != nil {
				s.remoteResolver.SetTlsMuxPort(0)
			}
		}
	}

	// Detect remote state transition and schedule OIDC app restart.
	// Uses the unified portal hosts list so transitions from either source
	// (self-hosted or namek) trigger a restart.
	s.detectRemoteTransitionAndRestart()
}

// clearNamekState tears down namek adapter and clears all namek routing/cert/domain state.
// Safe to call when namek is not ready or has no endpoints.
func (s *GinServer) clearNamekState(rm *remote.Manager) {
	s.stopNamekAdapter()

	// Cancel in-flight reconciliation and clear domain state
	s.namekMu.Lock()
	if s.namekReconcileStop != nil {
		s.namekReconcileStop()
		s.namekReconcileStop = nil
	}
	s.namekDomains = nil
	s.namekMu.Unlock()
	if rm != nil {
		rm.UnregisterOrchClient("namek")
	}
	if s.remoteResolver != nil {
		s.remoteResolver.SetRemoteBases("namek", nil)
	}
	if s.tlsMux != nil {
		s.tlsMux.SetRemoteBases("namek", nil)
		selfHostedActive := false
		if rm != nil {
			st := rm.Status()
			selfHostedActive = st.Enabled && strings.TrimSpace(st.PortalHostname) != ""
		}
		if !selfHostedActive {
			s.tlsMux.Stop()
			if s.remoteResolver != nil {
				s.remoteResolver.SetTlsMuxPort(0)
			}
		}
	}
	if s.certProvider != nil {
		s.certProvider.SetPortalMappings("namek", nil)
	}
	s.recomputeFrameAncestors()
	s.detectRemoteTransitionAndRestart()
}

// applyNamekState consolidates namek adapter lifecycle, routing, cert mappings, and cert
// issuance into a single method. Called when identity state changes.
// applyNamekState must only be called from the identity event subscriber goroutine.
// Concurrent calls would race on adapter lifecycle (namekLastKey, namekAdapterCancel).
func (s *GinServer) applyNamekState() {
	if s == nil || s.identityService == nil || s.namekStopped.Load() {
		return
	}
	svc := s.identityService
	rm := s.remoteManager
	ready := svc.IsEnrolled() && svc.IsEnabled() && !svc.IsSuspended()

	if !ready {
		s.clearNamekState(rm)
		return
	}

	idCfg := svc.DeviceConfig()
	if len(idCfg.NexusEndpoints) == 0 {
		log.Printf("INFO: server: namek enrolled but no nexus endpoints available")
		s.clearNamekState(rm)
		return
	}

	// Re-register orchClient (may have been unregistered on a prior !ready transition).
	// Reuse the init-time instance to preserve in-flight ACME challenge state.
	if rm != nil && s.namekACME != nil {
		rm.RegisterOrchClient("namek", s.namekACME)
	}

	// --- Adapter lifecycle ---
	// Use ResolvedEndpoints() for dedup/trim so whitespace-variant duplicates
	// from the identity service don't trigger unnecessary adapter restarts.
	endpoints := (nexusclient.Config{Endpoints: idCfg.NexusEndpoints}).ResolvedEndpoints()
	hostname := idCfg.Hostname
	// Prefer custom FQDN when set so routing/certs update immediately
	// without waiting for the namek server to push the new hostname.
	if custom := idCfg.CustomFQDN(); custom != "" {
		hostname = custom
	}

	// Build change-detection key from sorted endpoints + hostname fields.
	sortedEPs := make([]string, len(endpoints))
	copy(sortedEPs, endpoints)
	sort.Strings(sortedEPs)
	var keyBuilder strings.Builder
	for _, ep := range sortedEPs {
		keyBuilder.WriteString(ep)
		keyBuilder.WriteByte('\x00')
	}
	keyBuilder.WriteString(hostname)
	keyBuilder.WriteByte('\x00')
	keyBuilder.WriteString(idCfg.CustomHostname)
	key := keyBuilder.String()

	s.namekMu.Lock()
	changed := key != s.namekLastKey
	cancel := s.namekAdapterCancel
	adapter := s.namekAdapter
	s.namekMu.Unlock()

	if adapter != nil {
		if !changed && cancel != nil {
			// Adapter running with identical config — skip restart, but still update routing/certs below
		} else {
			adapterCfg := nexusclient.Config{
				Endpoints:      endpoints,
				PortalHostname: hostname,
			}
			if err := adapter.Configure(adapterCfg); err != nil {
				log.Printf("WARN: server: configure namek adapter: %v", err)
			}

			if cancel != nil {
				s.stopNamekAdapter() // resets namekLastKey to ""
			}

			// Set namekLastKey AFTER stop (which resets it) to avoid restart-on-every-event.
			s.namekMu.Lock()
			s.namekLastKey = key
			s.namekMu.Unlock()

			ctx, newCancel := context.WithCancel(context.Background())
			s.namekMu.Lock()
			s.namekAdapterCancel = newCancel
			s.namekMu.Unlock()

			go func(startedKey string) {
				if err := adapter.Start(ctx); err != nil {
					if !errors.Is(err, context.Canceled) {
						log.Printf("WARN: server: namek adapter exited: %v", err)
					}
					// Only clear the key if it still matches — a newer applyNamekState
					// may have already set a different key.
					s.namekMu.Lock()
					if s.namekLastKey == startedKey {
						s.namekLastKey = ""
					}
					s.namekMu.Unlock()
				}
			}(key)
			log.Printf("INFO: server: namek adapter started (endpoints=%d, hostname=%s)", len(endpoints), hostname)
		}
	}

	// --- Routing (resolver + TLS mux) ---
	var resolverBases []remoteBase
	var muxBases []services.TlsMuxBase
	if hostname != "" {
		resolverBases = append(resolverBases, remoteBase{
			source: "namek", portalHost: hostname, domain: hostname,
		})
		muxBases = append(muxBases, services.TlsMuxBase{
			Source: "namek", PortalHost: hostname, Domain: hostname,
		})
	}

	if s.remoteResolver != nil {
		s.remoteResolver.SetRemoteBases("namek", resolverBases)
	}
	if s.tlsMux != nil {
		s.tlsMux.SetRemoteBases("namek", muxBases)
		if len(muxBases) > 0 {
			if port, err := s.tlsMux.Start(); err == nil {
				if s.remoteResolver != nil {
					s.remoteResolver.SetTlsMuxPort(port)
				}
			} else {
				log.Printf("WARN: TLS mux start for namek failed: %v", err)
			}
		}
	}

	s.recomputeFrameAncestors()

	// --- Cert provider portal mappings ---
	if s.certProvider != nil {
		var mappings []services.PortalCertMapping
		if hostname != "" {
			mappings = append(mappings, services.PortalCertMapping{
				Hostname: strings.TrimSuffix(strings.ToLower(hostname), "."),
				CertName: "namek-portal",
			})
		}
		s.certProvider.SetPortalMappings("namek", mappings)
	}

	// --- Cert issuance ---
	if rm != nil && hostname != "" {
		// Requeue persisted certs that may have been skipped before orchClient was registered
		// (e.g., namek per-host certs or certs in error/pending from a prior boot).
		// Must run BEFORE explicit enqueue to avoid double-queueing the portal/wildcard certs.
		rm.RequeueOutstandingIssuances()

		certDir := paths.CoreJoin("network-bootstrap", "remote", "certs")
		rm.EnqueueCertIssuance(remote.CertIssuanceRequest{
			ID: "namek-portal", Source: "namek", Solver: "dns-01",
			CertDir: certDir, CommonName: hostname, Domains: []string{hostname},
		})
		wildcard := "*." + hostname
		rm.EnqueueCertIssuance(remote.CertIssuanceRequest{
			ID: "namek-wildcard", Source: "namek", Solver: "dns-01",
			CertDir: certDir, CommonName: wildcard, Domains: []string{wildcard, hostname},
		})
	}

	// --- Namek domain state rebuild ---
	// Cancel any in-flight reconciliation from a prior applyNamekState() call.
	s.namekMu.Lock()
	if s.namekReconcileStop != nil {
		s.namekReconcileStop()
	}
	reconcileCtx, reconcileCancel := context.WithCancel(context.Background())
	s.namekReconcileStop = reconcileCancel
	s.namekMu.Unlock()

	// Rebuild domain map and reconcile in the background.
	// All namek domain changes originate from this device (1:1 model).
	// When multi-device accounts are implemented, periodic sync will be needed.
	go s.rebuildNamekDomains(reconcileCtx)

	s.detectRemoteTransitionAndRestart()
}

// stopNamekAdapter cancels the namek adapter context and stops the adapter.
func (s *GinServer) stopNamekAdapter() {
	s.namekMu.Lock()
	cancel := s.namekAdapterCancel
	adapter := s.namekAdapter
	s.namekAdapterCancel = nil
	s.namekLastKey = ""
	s.namekMu.Unlock()

	if cancel != nil {
		cancel()
	}
	if adapter != nil {
		if err := adapter.Stop(context.Background()); err != nil {
			log.Printf("WARN: server: stopping namek adapter: %v", err)
		}
	}
}

// portalHosts returns the current list of active portal hostnames from the resolver.
// Returns nil when remoteResolver is not configured. Safe for concurrent use.
func (s *GinServer) portalHosts() []string {
	if s.remoteResolver != nil {
		return s.remoteResolver.PortalHosts()
	}
	return nil
}

// recomputeFrameAncestors rebuilds the CSP frame-ancestors list from all active
// remote portals, then pushes it to the proxy manager. Source-agnostic: uses the
// resolver as the single source of truth for active portal hostnames.
func (s *GinServer) recomputeFrameAncestors() {
	if s.serviceManager == nil {
		return
	}
	s.serviceManager.ProxyManager().SetAllowedAncestors(s.portalHosts())
}

// detectRemoteTransitionAndRestart checks whether the set of active portal hosts
// has changed (enable/disable, hostname rename, custom domain) and schedules an
// OIDC app restart on any change.
func (s *GinServer) detectRemoteTransitionAndRestart() {
	hosts := s.portalHosts()
	sort.Strings(hosts)
	nowKey := strings.Join(hosts, ",")

	s.remoteStateMu.Lock()
	changed := s.remoteStateHosts != nowKey
	s.remoteStateHosts = nowKey
	s.remoteStateMu.Unlock()

	if changed {
		s.scheduleOIDCAppsRestart()
	}
}

// scheduleOIDCAppsRestart schedules a debounced restart of apps that declare oidc_client.
// This is called when remote state changes to ensure apps refresh OIDC discovery behavior.
func (s *GinServer) scheduleOIDCAppsRestart() {
	if s == nil || s.appManager == nil {
		return
	}

	// Only run on kernel leader (local mode)
	if s.routeManager != nil && s.routeManager.KernelRoute().Mode != router.ModeLocal {
		log.Printf("INFO: skipping OIDC app restart - not kernel leader")
		return
	}

	debounceMs := s.oidcRestartDebounceMs
	if debounceMs <= 0 {
		debounceMs = 5000 // default 5 second debounce
	}

	s.remoteStateMu.Lock()
	if s.oidcRestartTimer != nil {
		s.oidcRestartTimer.Stop()
	}
	s.oidcRestartTimer = time.AfterFunc(time.Duration(debounceMs)*time.Millisecond, func() {
		s.restartOIDCApps()
	})
	s.remoteStateMu.Unlock()

	log.Printf("INFO: scheduled OIDC apps restart in %dms due to remote state change", debounceMs)
}

// restartOIDCApps restarts all apps with oidc_client to update OIDC discovery behavior.
func (s *GinServer) restartOIDCApps() {
	if s == nil || s.appManager == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	apps, err := s.appManager.List(ctx)
	if err != nil {
		log.Printf("ERROR: failed to list apps for OIDC restart: %v", err)
		return
	}

	var oidcApps []string
	for _, a := range apps {
		if a.Definition != nil && appDeclaresOIDCClient(a.Definition) {
			// Only restart apps that were running - don't start stopped apps
			if a.Status == "running" || a.Status == "starting" {
				oidcApps = append(oidcApps, a.InstanceID)
			}
		}
	}

	if len(oidcApps) == 0 {
		log.Printf("INFO: no running OIDC apps to restart")
		return
	}

	log.Printf("INFO: restarting %d OIDC apps due to remote state change: %v", len(oidcApps), oidcApps)

	for _, id := range oidcApps {
		// Stop then start to trigger fresh discovery endpoint configuration
		if err := s.appManager.Stop(ctx, id); err != nil {
			log.Printf("WARN: failed to stop OIDC app %s: %v", id, err)
			continue
		}
		if err := s.appManager.Start(ctx, id); err != nil {
			log.Printf("WARN: failed to start OIDC app %s: %v", id, err)
		}
	}
}

func appDeclaresOIDCClient(def *api.AppDefinition) bool {
	if def == nil {
		return false
	}
	for _, svc := range def.Services {
		if svc.OIDCClient != nil {
			return true
		}
	}
	return false
}

func (s *GinServer) observeLockState(bus *events.Bus) {
	if bus == nil || s.healthTracker == nil {
		return
	}
	ch := bus.Subscribe(events.TopicLockStateChanged, 8)
	go func() {
		for evt := range ch {
			payload, ok := evt.Payload.(events.LockStateChanged)
			if !ok {
				continue
			}
			if payload.Locked {
				s.healthTracker.Setf("persistence", health.LevelWarn, "control store locked")
				s.healthTracker.Setf("app-manager", health.LevelWarn, "app manager gated by lock state")
			} else {
				s.healthTracker.Setf("persistence", health.LevelOK, "control store unlocked")
				s.healthTracker.Setf("app-manager", health.LevelOK, "app manager ready")
				s.reloadComponentsAfterUnlock()
			}
		}
	}()
}

func (s *GinServer) observeLeadership(bus *events.Bus) {
	if bus == nil || s.healthTracker == nil {
		return
	}
	ch := bus.Subscribe(events.TopicLeadershipRoleChanged, 8)
	go func() {
		for evt := range ch {
			payload, ok := evt.Payload.(events.LeadershipChanged)
			if !ok {
				continue
			}
			if payload.Resource != cluster.ResourceKernel {
				continue
			}
			// Standby (follower) is not a degraded state for the control plane in single-node context.
			// Reflect role in the message but keep LevelOK.
			if s.routeManager != nil {
				mode := router.ModeLocal
				if payload.Role == cluster.RoleFollower {
					mode = router.ModeTunnel
				}
				s.routeManager.RegisterKernelRoute(mode, "")
			}
			switch payload.Role {
			case cluster.RoleLeader:
				s.healthTracker.Setf("service-manager", health.LevelOK, "service manager role=leader")
			case cluster.RoleFollower:
				s.healthTracker.Setf("service-manager", health.LevelOK, "service manager role=follower (standby)")
			}
		}
	}()
}

func (s *GinServer) observeRemoteConfig(bus *events.Bus) {
	if bus == nil {
		return
	}
	ch := bus.Subscribe(events.TopicRemoteConfigChanged, 8)
	go func() {
		for evt := range ch {
			status, ok := evt.Payload.(remote.Status)
			if !ok {
				continue
			}
			s.applyRemoteRuntimeFromStatus(status)
		}
	}()
}

// observeRemotePortClaims subscribes to endpoint changes and refreshes port
// claim mappings on the remote adapter. This fixes the boot race where
// RestoreServices hasn't populated claims yet when the adapter first starts,
// and ensures runtime app install/start/stop propagates claims to the relay.
func (s *GinServer) observeRemotePortClaims(bus *events.Bus) {
	if bus == nil || s.remoteManager == nil {
		return
	}
	ch := bus.Subscribe(events.TopicServiceEndpointsChanged, 16)
	go func() {
		for range ch {
			s.remoteManager.RefreshPortClaims()
		}
	}()
}

// observeRemoteCertQueuing subscribes to endpoint, remote config, and identity
// changes to queue per-host certificate issuance for HTTP-01 portals. This
// ensures certs are created for endpoints added at startup (RestoreFromPodman),
// after remote is reconfigured, or when identity state changes.
func (s *GinServer) observeRemoteCertQueuing(bus *events.Bus) {
	if bus == nil {
		return
	}
	endpointsCh := bus.Subscribe(events.TopicServiceEndpointsChanged, 16)
	remoteCfgCh := bus.Subscribe(events.TopicRemoteConfigChanged, 16)
	identityCh := bus.Subscribe(events.TopicIdentityChanged, 16)

	// Endpoints added: queue per-host certs for added endpoints only.
	go func() {
		for evt := range endpointsCh {
			payload, ok := evt.Payload.(events.ServiceEndpointsChanged)
			if !ok {
				continue
			}
			rm := s.remoteManager
			if rm == nil {
				continue
			}
			// Queue certs for newly added endpoints (HTTP-01 portals only).
			entries := s.portalCertEntries()
			for _, entry := range entries {
				for _, ep := range payload.Added {
					if ep.DerivedHostLabel == "" || ep.Flow == api.FlowTLS {
						continue // flow:tls apps manage their own certificates (RFC 20260316)
					}
					host := services.RemoteServiceHostname(ep.DerivedHostLabel, entry.Hostname)
					if host == "" {
						continue
					}
					rm.EnqueueCertIssuance(remote.CertIssuanceRequest{
						ID: "host:" + host, Source: entry.Source,
						Solver: entry.Solver, CertDir: entry.CertDir,
						Domains: []string{host}, CommonName: host,
					})
				}
			}

			// Remove certs for permanently removed endpoints.
			// Derive base from PortalHostname directly (not gating on Enabled)
			// so cleanup works even if remote is disabled at removal time.
			if len(payload.Removed) > 0 {
				status := rm.Status()
				base := remotePortalBase(&status)
				if base != "" {
					for _, ep := range payload.Removed {
						if ep.DerivedHostLabel == "" {
							continue
						}
						rm.RemoveHostnameCertificate(ep.DerivedHostLabel + "." + base)
					}
				}
				// Also clean up per-app certs for alias hostnames
				for _, a := range rm.ListAliases() {
					aliasBase := normalizeHostname(a.Hostname)
					if aliasBase == "" {
						continue
					}
					for _, ep := range payload.Removed {
						if ep.DerivedHostLabel == "" {
							continue
						}
						rm.RemoveHostnameCertificate(ep.DerivedHostLabel + "." + aliasBase)
					}
				}
			}
		}
	}()

	// Remote config or identity changed: re-queue all endpoint certs.
	go func() {
		for range remoteCfgCh {
			s.queueAllEndpointCerts()
		}
	}()
	go func() {
		for range identityCh {
			s.queueAllEndpointCerts()
		}
	}()
}

// queueAllEndpointCerts queues per-host certs for all active HTTP-01 portals
// and all registered endpoints.
func (s *GinServer) queueAllEndpointCerts() {
	rm := s.remoteManager
	sm := s.serviceManager
	if rm == nil || sm == nil {
		return
	}
	entries := s.portalCertEntries()
	if len(entries) == 0 {
		return
	}
	endpoints := sm.GetAll()
	for _, entry := range entries {
		for _, ep := range endpoints {
			if ep.DerivedHostLabel == "" || ep.Flow == api.FlowTLS {
				continue // flow:tls apps manage their own certificates (RFC 20260316)
			}
			host := services.RemoteServiceHostname(ep.DerivedHostLabel, entry.Hostname)
			if host == "" {
				continue
			}
			rm.EnqueueCertIssuance(remote.CertIssuanceRequest{
				ID: "host:" + host, Source: entry.Source,
				Solver: entry.Solver, CertDir: entry.CertDir,
				Domains: []string{host}, CommonName: host,
			})
		}
	}
}

// observeProxyOIDCClients auto-registers proxy OIDC clients for apps whose
// listeners require authentication (RFC 20260122 §5.3).
//
// Subscribes to two topics:
//   - TopicAppStatusChanged ("installed"): fires after StoreApp, covers new installs.
//   - TopicServiceEndpointsChanged (added): covers RestoreFromPodman on reboot and
//     any future path that adds endpoints.
//
// Note: cleanup of proxy OIDC clients on uninstall is handled separately in
// handleGinAppUninstall (via DeleteClientsByAppID). Update-listeners uses an
// explicit handler-level call because no post-persist event exists for that path.
func (s *GinServer) observeProxyOIDCClients(bus *events.Bus) {
	if bus == nil {
		return
	}
	appStatusCh := bus.Subscribe(events.TopicAppStatusChanged, 16)
	endpointsCh := bus.Subscribe(events.TopicServiceEndpointsChanged, 16)

	tryRegister := func(appName string) {
		ctx := context.Background()
		appInst, err := s.appManager.Get(ctx, appName)
		if err != nil || appInst.Definition == nil {
			return
		}
		if !s.requiresProxyOIDCClient(appInst.Definition) {
			return
		}
		if err := s.registerProxyOIDCClient(ctx, appName); err != nil {
			// errOIDCManagerUnavailable means control store is still locked (expected during boot);
			// RestoreServices will re-emit endpoint events after unlock.
			if !errors.Is(err, errOIDCManagerUnavailable) {
				log.Printf("WARN: auto-register proxy OIDC client for %s: %v", appName, err)
			}
		}
	}

	go func() {
		for evt := range appStatusCh {
			payload, ok := evt.Payload.(events.AppStatusChangedEvent)
			if !ok || payload.Status != "installed" {
				continue
			}
			tryRegister(payload.App)
		}
	}()
	go func() {
		for evt := range endpointsCh {
			payload, ok := evt.Payload.(events.ServiceEndpointsChanged)
			if !ok || len(payload.Added) == 0 {
				continue
			}
			tryRegister(payload.App)
		}
	}()
}

// requiresProxyOIDCClient checks if an app requires a proxy OIDC client per RFC 20260122 §5.3.
// Proxy clients are needed for apps whose listeners use "headers" or "protected" auth strategies.
// Per RFC 20260122 §4.1.1: auth omitted or empty rules → all paths default to "protected".
func (s *GinServer) requiresProxyOIDCClient(appDef *api.AppDefinition) bool {
	if appDef == nil {
		return false
	}
	for _, listener := range appDef.Listeners {
		if listener.Auth == nil || len(listener.Auth.Rules) == 0 {
			return true
		}
		for _, rule := range listener.Auth.Rules {
			strategy := rule.Strategy
			if strategy == "" {
				strategy = "public"
			}
			if strategy == "headers" || strategy == "protected" {
				return true
			}
		}
	}
	return false
}

// errOIDCManagerUnavailable is returned when the OIDC client manager cannot be
// obtained (e.g. control store still locked during boot).
var errOIDCManagerUnavailable = errors.New("OIDC client manager unavailable")

// deleteProxyOIDCClient removes the proxy OIDC client for an app if one exists.
// Used when listener auth rules change such that a proxy client is no longer needed.
func (s *GinServer) deleteProxyOIDCClient(ctx context.Context, appName string) {
	clientMgr := s.getOIDCClientManager()
	if clientMgr == nil {
		return
	}
	client, err := clientMgr.GetProxyClientByAppName(ctx, appName)
	if err != nil {
		return // no proxy client exists — nothing to clean up
	}
	if err := clientMgr.DeleteClient(ctx, client.ID); err != nil {
		log.Printf("WARN: failed to delete stale proxy OIDC client for %s: %v", appName, err)
	} else {
		log.Printf("INFO: deleted proxy OIDC client for app %s (auth no longer required)", appName)
	}
}

// registerProxyOIDCClient registers a proxy OIDC client for an app per RFC 20260122 §5.3.
func (s *GinServer) registerProxyOIDCClient(ctx context.Context, appName string) error {
	clientMgr := s.getOIDCClientManager()
	if clientMgr == nil {
		return errOIDCManagerUnavailable
	}

	_, err := clientMgr.GetProxyClientByAppName(ctx, appName)
	if err == nil {
		return nil
	}

	_, _, err = clientMgr.RegisterProxyClient(ctx, appName)
	if err != nil {
		return fmt.Errorf("register proxy client: %w", err)
	}

	log.Printf("INFO: registered proxy OIDC client for app %s", appName)
	return nil
}

func (s *GinServer) handleGinReadinessCheck(c *gin.Context) {
	if s.healthTracker == nil {
		c.JSON(http.StatusOK, gin.H{"ready": true, "status": "unknown"})
		return
	}
	required := []string{"persistence", "app-manager", "service-manager"}
	ready, snapshot := s.healthTracker.Ready(required...)
	overall := s.healthTracker.Overall()
	payload := gin.H{
		"ready":      ready,
		"status":     overall.String(),
		"components": flattenHealth(snapshot),
	}
	if overall == health.LevelError {
		c.JSON(http.StatusServiceUnavailable, payload)
		return
	}
	c.JSON(http.StatusOK, payload)
}

func (s *GinServer) handleHealthLive(c *gin.Context) {
	overall := "unknown"
	if s.healthTracker != nil {
		overall = s.healthTracker.Overall().String()
	}
	c.JSON(http.StatusOK, gin.H{"status": overall})
}

func (s *GinServer) handleHealthDetail(c *gin.Context) {
	if s.healthTracker == nil {
		c.JSON(http.StatusOK, gin.H{"overall": "unknown", "components": []gin.H{}})
		return
	}
	snapshot := s.healthTracker.Snapshot()
	c.JSON(http.StatusOK, gin.H{
		"overall":    s.healthTracker.Overall().String(),
		"components": flattenHealth(snapshot),
	})
}

func flattenHealth(snapshot map[string]health.Status) []gin.H {
	components := make([]gin.H, 0, len(snapshot))
	for name, st := range snapshot {
		components = append(components, gin.H{
			"name":       name,
			"level":      st.Level.String(),
			"message":    st.Message,
			"details":    st.Details,
			"updated_at": st.UpdatedAt,
		})
	}
	return components
}

func (s *GinServer) initSecureLoopback() error {
	if s == nil {
		return nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	s.secureListener = ln
	if addr, ok := ln.Addr().(*net.TCPAddr); ok {
		s.securePort = addr.Port
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), secureContextKeyInstance, true)
		s.router.ServeHTTP(w, r.WithContext(ctx))
	})
	s.secureSrv = &http.Server{
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  60 * time.Second,
		ErrorLog:     newFilteredErrorLogger(),
	}
	return nil
}

func (s *GinServer) startSecureLoopback() {
	if s == nil || s.secureSrv == nil || s.secureListener == nil {
		return
	}
	go func() {
		if err := s.secureSrv.Serve(s.secureListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("WARN: secure loopback server stopped: %v", err)
		}
	}()
	log.Printf("INFO: Secure loopback portal listening on 127.0.0.1:%d", s.securePort)
}

func (s *GinServer) stopSecureLoopback(ctx context.Context) {
	if s == nil || s.secureSrv == nil {
		return
	}
	if err := s.secureSrv.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("WARN: secure loopback shutdown failed: %v", err)
	}
	s.secureSrv = nil
	s.secureListener = nil
	s.securePort = 0
}

func (s *GinServer) startInternalHTTPSListener() {
	if s == nil || s.internalCA == nil {
		return
	}

	// Bind to all interfaces (0.0.0.0) instead of localhost only.
	// RFC specifies 127.0.0.1:443, but containers access via host-gateway IP
	// (e.g., 10.88.0.1 via --add-host piccolo.local:host-gateway), not localhost.
	// Security relies on:
	// 1. Internal CA - only piccolo's CA can issue valid certs
	// 2. TLS verification - clients must verify cert is from internal CA
	// 3. OIDC spec endpoints - no sensitive data beyond standard OIDC claims
	// Preload cert into cache before starting listener to fail fast on missing/corrupt certs
	if _, err := s.reloadServerCertFromDisk(); err != nil {
		log.Printf("ERROR: cannot start HTTPS listener: failed to load server cert: %v", err)
		return
	}

	addr := "0.0.0.0:443"
	tlsCfg := &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return s.loadServerCert()
		},
		MinVersion: tls.VersionTLS12,
	}
	s.internalSrv = &http.Server{
		Addr:      addr,
		Handler:   s.router,
		TLSConfig: tlsCfg,
		ErrorLog:  newFilteredErrorLogger(),
	}

	go func() {
		log.Printf("INFO: Starting Internal HTTPS Listener on %s (LAN + back-channel)", addr)
		// Empty cert/key paths: GetCertificate handles loading from disk
		if err := s.internalSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("ERROR: Internal HTTPS Listener failed: %v", err)
		}
	}()
}

func (s *GinServer) loadServerCert() (*tls.Certificate, error) {
	s.cachedCertMu.RLock()
	cert := s.cachedCert
	s.cachedCertMu.RUnlock()
	if cert != nil {
		return cert, nil
	}
	// Fallback: load from disk (initial handshake before cache is populated)
	return s.reloadServerCertFromDisk()
}

func (s *GinServer) reloadServerCertFromDisk() (*tls.Certificate, error) {
	if s.internalCA == nil {
		return nil, fmt.Errorf("internal CA not initialized")
	}
	certPath := s.internalCA.ServerCertPath()
	keyPath := s.internalCA.ServerKeyPath()
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	s.cachedCertMu.Lock()
	s.cachedCert = &cert
	s.cachedCertMu.Unlock()
	return &cert, nil
}

func (s *GinServer) refreshServerCertSANs() {
	if s.internalCA == nil || s.mdnsManager == nil {
		return
	}
	hostnames := s.mdnsManager.TLSHostnames()
	if len(hostnames) == 0 {
		return
	}
	changed, err := s.internalCA.EnsureServerCertificateForHosts(hostnames)
	if err != nil {
		log.Printf("WARN: cert SAN refresh: %v", err)
		return
	}
	if changed {
		log.Printf("INFO: server certificate regenerated with SANs: %v", hostnames)
		// Atomically reload the new cert into memory cache
		if _, err := s.reloadServerCertFromDisk(); err != nil {
			log.Printf("WARN: failed to reload cert into cache: %v", err)
		}
	}
}

func (s *GinServer) stopInternalHTTPSListener(ctx context.Context) {
	if s == nil || s.internalSrv == nil {
		return
	}
	if err := s.internalSrv.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("WARN: Internal HTTPS listener shutdown failed: %v", err)
	}
	s.internalSrv = nil
}

func (s *GinServer) httpsRedirectMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || s.remoteResolver == nil {
			c.Next()
			return
		}
		if strings.HasPrefix(c.Request.URL.Path, "/.well-known/acme-challenge/") {
			c.Next()
			return
		}
		host := canonicalHost(c.Request.Host)
		if host == "" {
			c.Next()
			return
		}
		// Local development and mDNS names (e.g., piccolo.local) should remain HTTP even
		// if the remote resolver is configured with a matching TLD.
		if strings.HasSuffix(host, ".local") || host == "localhost" || host == "127.0.0.1" {
			c.Next()
			return
		}
		if net.ParseIP(host) != nil {
			c.Next()
			return
		}
		if !s.remoteResolver.IsRemoteHostname(host) {
			c.Next()
			return
		}
		if s.isSecureRequest(c.Request) {
			c.Next()
			return
		}
		target := "https://" + host + c.Request.URL.RequestURI()
		c.Redirect(http.StatusTemporaryRedirect, target)
		c.Abort()
	}
}

// isRemoteSecureRequest returns true only for requests arriving through the
// nexus/remote TLS path (secureContextKey). Internal HTTPS (LAN :443) is NOT
// included because those requests should not receive remote-only headers such
// as HSTS.
//
// X-Forwarded-Proto is NOT trusted here — the TLS mux terminates TLS and
// forwards cleartext HTTP via io.Copy without injecting HTTP headers. Remote
// portal traffic goes through the secure loopback which sets
// secureContextKeyInstance. Trusting X-Forwarded-Proto would let any LAN
// client spoof a remote-secure context.
func (s *GinServer) isRemoteSecureRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	return r.Context().Value(secureContextKeyInstance) != nil
}

// isSecureRequest returns true for requests that arrived over a secure channel:
// direct TLS (:443) or the TLS mux secure loopback (secureContextKey).
//
// X-Forwarded-Proto is NOT trusted — the TLS mux terminates TLS and forwards
// cleartext HTTP via io.Copy without injecting HTTP headers. All legitimate
// secure paths are covered by r.TLS (LAN HTTPS) and secureContextKeyInstance
// (TLS mux → secure loopback). Trusting the header would let any client
// bypass the HTTPS redirect.
func (s *GinServer) isSecureRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	return r.Context().Value(secureContextKeyInstance) != nil
}

func canonicalHost(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if i := strings.Index(v, ","); i != -1 {
		v = v[:i]
	}
	if strings.HasPrefix(v, "[") {
		if idx := strings.Index(v, "]"); idx != -1 {
			v = v[1:idx]
		}
	} else {
		if h, _, err := net.SplitHostPort(v); err == nil {
			v = h
		} else if i := strings.Index(v, ":"); i != -1 {
			v = v[:i]
		}
	}
	v = strings.Trim(v, "[]")
	return strings.TrimSuffix(strings.ToLower(v), ".")
}
