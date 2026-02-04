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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	"piccolod/internal/health"
	hostnamepkg "piccolod/internal/hostname"
	"piccolod/internal/mdns"
	"piccolod/internal/oidc"
	"piccolod/internal/persistence"
	"piccolod/internal/remote"
	"piccolod/internal/remote/nexusclient"
	"piccolod/internal/router"
	"piccolod/internal/runtime/commands"
	"piccolod/internal/runtime/supervisor"
	"piccolod/internal/services"
	"piccolod/internal/state/paths"
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

	secureSrv      *http.Server
	secureListener net.Listener
	securePort     int

	// Optional OpenAPI request validation (Phase 0)
	apiValidator *openAPIValidator

	// Auth & sessions (Phase 1)
	authManager *authpkg.Manager
	sessions    *authpkg.SessionStore
	userManager *authpkg.UserManager
	// simple rate-limit counters for login failures
	loginFailures int
	resetFailures int

	// Crypto manager for lock/unlock of app data volumes
	cryptoManager  *crypt.Manager
	healthTracker  *health.Tracker
	updateManager  osUpdateManager
	catalogManager *catalog.Manager

	// Remote state tracking for OIDC app cache busting
	remoteStateMu         sync.Mutex
	remoteStateEnabled    bool
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
}

type secureContextKey struct{}

var secureContextKeyInstance = secureContextKey{}

// portUnpublisherFunc adapts a function into services.PortUnpublisher.
type portUnpublisherFunc func(int)

func (f portUnpublisherFunc) Unpublish(p int) { f(p) }

// portPublisherFunc adapts a function into services.PortPublisher.
type portPublisherFunc func(int)

func (f portPublisherFunc) Publish(p int) { f(p) }

type serviceRemoteResolver struct {
	services   *services.ServiceManager
	mu         sync.RWMutex
	domain     string
	portal     string
	port       int
	tlsMuxPort int
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

func (r *serviceRemoteResolver) UpdateConfig(cfg nexusclient.Config) {
	r.mu.Lock()
	portal := strings.TrimSuffix(strings.ToLower(cfg.PortalHostname), ".")
	// RFC 20260114: remote base is the portal hostname apex itself.
	// App hostnames are <label>.<portal>.
	r.portal = portal
	r.domain = portal
	r.mu.Unlock()
}

func (r *serviceRemoteResolver) IsRemoteHostname(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	r.mu.RLock()
	portal := r.portal
	domain := r.domain
	r.mu.RUnlock()
	if host == "" {
		return false
	}
	if portal != "" && host == portal {
		return true
	}
	if domain != "" {
		if host == domain {
			return true
		}
		if strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func (r *serviceRemoteResolver) SetTlsMuxPort(p int) { r.mu.Lock(); r.tlsMuxPort = p; r.mu.Unlock() }

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
	portal := r.portal
	domain := r.domain
	portalPort := r.port
	tlsMuxPort := r.tlsMuxPort
	r.mu.RUnlock()

	// Strict RFC 20260114: remote resolution is only valid when the portal hostname (remote base)
	// is configured. Without it, we do not attempt any hostname/port fallbacks.
	if portal == "" || domain == "" {
		return 0, false
	}

	normPort := remotePort
	if normPort == acmeHTTPFallbackPort {
		normPort = 80
	}

	// Portal host (apex): treat as flow=tcp (device-terminated TLS when not 80)
	// Per RFC 20260114 Section 5.1 step 1: <base> always routes to portal
	if portal != "" && h == portal {
		if normPort == 80 {
			return portalPort, true
		}
		if isTLS && tlsMuxPort > 0 {
			return tlsMuxPort, true
		}
		return 0, false
	}

	// Extract host label from hostname (RFC 20260114)
	// Format: <app>.<base> (primary) or <listener>-<app>.<base> (non-primary)
	hostLabel := ""
	if domain != "" {
		suffix := "." + domain
		if strings.HasSuffix(h, suffix) {
			label := h[:len(h)-len(suffix)]
			// Only take the first label (no nested subdomains)
			if idx := strings.Index(label, "."); idx != -1 {
				label = label[:idx]
			}
			hostLabel = label
		}
	} else if idx := strings.Index(h, "."); idx != -1 {
		hostLabel = h[:idx]
	}

	// Resolve by host label (RFC 20260114)
	// This handles both <app>.<base> (primary) and <listener>-<app>.<base> (non-primary)
	if hostLabel != "" {
		if ep, ok := r.services.ResolveByHostLabel(hostLabel, normPort); ok {
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
	stateDir := paths.Root()
	cmgr, err := crypt.NewManager(stateDir)
	if err != nil {
		return nil, fmt.Errorf("crypto manager init: %w", err)
	}
	healthTracker := health.NewTracker()

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
		mdnsMgr.ObserveServiceEndpoints(eventsBus)
	}

	catalogMgr := catalog.NewManager(os.Getenv("PICCOLO_APP_STORE_URL"), filepath.Join(stateDir, "tmp", "catalog"))

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
		healthTracker:  healthTracker,
		catalogManager: catalogMgr,
	}
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

	s.supervisor.Register(supervisor.NewComponent("app-manager", func(ctx context.Context) error {
		s.appManager.StartBackground()
		return nil
	}, func(ctx context.Context) error {
		s.appManager.StopBackground()
		return nil
	}))

	s.supervisor.Register(supervisor.NewComponent("consensus", consensusMgr.Start, consensusMgr.Stop))
	s.supervisor.Register(newLeadershipObserver(eventsBus))
	s.observeLockState(eventsBus)
	s.observeLeadership(eventsBus)
	s.observeRemoteConfig(eventsBus)
	s.observeProxyOIDCClients(eventsBus)

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
		svcMgr.ProxyManager().SetAliasChecker(func(host, listener string) bool {
			if s == nil || s.remoteManager == nil {
				return false
			}
			h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
			if h == "" || listener == "" {
				return false
			}
			for _, a := range s.remoteManager.ListAliases() {
				if strings.TrimSpace(a.Hostname) == "" {
					continue
				}
				if a.Listener != listener {
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
			// RFC 20260122 §6.2: Trust X-Forwarded-Proto because Piccolo's TLS mux
			// terminates TLS and sets this header for downstream handlers.
			TrustForwardedProto: true,
		})
	}

	// Remote manager
	bootstrapDir := persist.BootstrapVolume().MountDir
	if strings.TrimSpace(bootstrapDir) == "" {
		return nil, fmt.Errorf("bootstrap volume mount unavailable")
	}
	remoteStorage := newBootstrapRemoteStorage(persist.Control().Remote(), bootstrapDir)
	var rm *remote.Manager
	if remoteStorage != nil {
		rm, err = remote.NewManagerWithStorage(remoteStorage, bootstrapDir)
	} else {
		rm, err = remote.NewManager(bootstrapDir)
	}
	if err != nil {
		return nil, fmt.Errorf("remote manager init: %w", err)
	}
	s.remoteManager = rm
	s.registerUnlockReloader(rm)
	rm.SetEventsBus(eventsBus)

	// Wire remote status provider for health aggregation (RFC 20260125)
	if rm != nil && svcMgr != nil {
		svcMgr.SetRemoteStatusProvider(&remoteStatusAdapter{rm: rm})
	}

	// Internal CA is initialized at Start() from network-bootstrap dir (pre-unlock).

	// Now that remote manager exists, wire ACME challenge handler and cert provider
	if rm != nil && svcMgr != nil {
		svcMgr.ProxyManager().SetAcmeHandler(rm.HTTPChallengeHandler())
		certProv := remote.NewFileCertProvider(rm.CertDirectory())
		certProv.SetMissingHandler(func(host string) {
			if rm == nil {
				return
			}
			st := rm.Status()
			if !st.Enabled || !strings.EqualFold(st.Solver, "http-01") {
				return
			}
			base := remoteBaseHostname(&st)
			if base == "" {
				return
			}
			h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
			if h == "" {
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
			rm.QueueHostnameCertificate(h)
		})
		tlsMux.SetCertProvider(certProv)
	}
	var nexusAdapter nexusclient.Adapter
	if os.Getenv("PICCOLO_NEXUS_USE_STUB") == "1" {
		nexusAdapter = nexusclient.NewStub()
	} else {
		nexusAdapter = nexusclient.NewBackendAdapter(routeMgr, remoteResolver)
	}
	rm.SetNexusAdapter(nexusAdapter)
	remote.RegisterHandlers(dispatch, rm)
	s.healthTracker.Setf("remote", health.LevelOK, "remote manager ready")
	s.refreshRemoteRuntime()

	// Update manager (MicroOS transactional-update)
	if s.updateManager == nil {
		um, err := update.NewManager(update.WithCurrentVersion(s.version))
		if err != nil {
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

	// (Simplified) No dynamic port publish/unpublish wiring; allow dial to fail gracefully.

	// Rehydrate proxies for containers that survived restarts
	appMgr.RestoreServices(context.Background())

	s.setupGinRoutes()
	if err := s.initSecureLoopback(); err != nil {
		return nil, fmt.Errorf("secure loopback init: %w", err)
	}
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

	return s.router.Run(":" + port)
}

// Stop gracefully shuts down the server and all its components.
// The context is used for timeout control during shutdown.
func (s *GinServer) Stop(ctx context.Context) error {
	log.Printf("INFO: Beginning graceful shutdown...")

	// Notify systemd that we're stopping
	if sent, err := daemon.SdNotify(false, daemon.SdNotifyStopping); err != nil {
		log.Printf("WARN: Failed to notify systemd of stopping: %v", err)
	} else if sent {
		log.Printf("INFO: Notified systemd that service is stopping")
	}

	// 1. Stop accepting new requests (handled by caller stopping the listener)

	// 2. Stop app manager background tasks and all running apps
	if s.appManager != nil {
		s.appManager.StopRuntimeEvents()
		if err := s.appManager.StopAllApps(ctx); err != nil {
			log.Printf("WARN: Failed to stop all apps cleanly: %v", err)
		}
	}

	// 3. Stop cert refresh subscription and internal listeners
	if s.certRefreshUnsub != nil {
		s.certRefreshUnsub()
		s.certRefreshUnsub = nil
	}
	s.stopSecureLoopback()
	s.stopInternalHTTPSListener()

	// 4. Stop OIDC provider's background goroutines
	s.oidcProviderMu.Lock()
	if s.oidcProvider != nil {
		s.oidcProvider.Storage().Close()
	}
	s.oidcProviderMu.Unlock()

	// 5. Stop supervisor-managed components
	if err := s.supervisor.Stop(ctx); err != nil {
		log.Printf("WARN: Failed to stop components cleanly: %v", err)
	}

	// 6. Shutdown persistence (detach control and bootstrap volumes) - AFTER apps stopped
	if s.persistence != nil {
		if err := s.persistence.Shutdown(ctx); err != nil {
			log.Printf("WARN: Failed to shutdown persistence cleanly: %v", err)
		}
	}

	log.Printf("INFO: Graceful shutdown completed")
	return nil
}

func (s *GinServer) portalOriginForRequest(r *http.Request) string {
	if s == nil || r == nil {
		return ""
	}

	// WAN via Nexus proxy: RemoteAddr is loopback.
	remoteHost, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(remoteHost); ip != nil && ip.IsLoopback() {
		if s.remoteManager != nil {
			st := s.remoteManager.Status()
			if st.Enabled && strings.TrimSpace(st.PortalHostname) != "" {
				return "https://" + strings.TrimSuffix(strings.TrimSpace(st.PortalHostname), ".")
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
		// In a common "https reverse proxy → piccolod:80" setup, X-Forwarded-Proto indicates https
		// while piccolod listens on 80; do not append :80 to the external origin.
		if envPort != 80 {
			portalPort = envPort
		}
	}

	host := ""
	if s.mdnsManager != nil {
		host = strings.TrimSpace(s.mdnsManager.Hostname())
	}
	if host == "" {
		host = canonicalHost(r.Host)
	}
	if host == "" {
		host = getPreferredOutboundIP()
	}
	if host == "" {
		return ""
	}
	if portalPort != 0 && portalPort != defaultPort {
		host = net.JoinHostPort(host, strconv.Itoa(portalPort))
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

		// Selected read-only status endpoints remain public
		v1.GET("/remote/status", s.handleRemoteStatus)
		v1.GET("/storage/disks", s.handleStorageDisks)
		v1.GET("/health/live", s.handleHealthLive)
		v1.GET("/health/ready", s.handleGinReadinessCheck)
		v1.GET("/health/detail", s.handleHealthDetail)

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
			apps.DELETE("/:name", s.requireUnlocked(), s.requireAdmin(), s.handleGinAppUninstall)
			apps.PATCH("/:name/listeners", s.requireUnlocked(), s.requireAdmin(), s.handleGinAppUpdateListeners)
			apps.POST("/:name/start", s.requireUnlocked(), s.requireAdmin(), s.handleGinAppStart)
			apps.POST("/:name/stop", s.requireUnlocked(), s.requireAdmin(), s.handleGinAppStop)
			apps.GET("/:name/terminal", s.requireAdmin(), s.handleWorkspaceTerminal)
		}

		// Image search (Admin only)
		admin.GET("/images/search", s.handleImageSearch)

		// System logs (Admin only)
		admin.GET("/system/logs/stream", s.handleGinSystemLogStream)

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
			remote.GET("/certificates", s.handleRemoteCertificatesList)
			remote.POST("/certificates/:id/renew", s.handleRemoteCertificateRenew)
			remote.GET("/events", s.handleRemoteEvents)
			remote.GET("/nexus-guide", s.handleRemoteGuideInfo)
			remote.POST("/nexus-guide/verify", s.handleRemoteGuideVerify)
		}

		// Persistence exports (Admin only)
		admin.POST("/exports/control", s.requireUnlocked(), s.handlePersistenceControlExport)
		admin.POST("/exports/full", s.requireUnlocked(), s.handlePersistenceFullExport)

		// Auth-only endpoints (Accessible to all logged-in users)
		authed.POST("/auth/logout", s.handleAuthLogout)
		authed.POST("/auth/password", s.handleAuthPassword)
		authed.POST("/auth/staleness/ack", s.handleAuthStalenessAck)
		authed.GET("/auth/csrf", s.handleAuthCSRF)
		authed.POST("/oauth/resume", s.handleOIDCResume)

		// Debug terminal (Admin only)
		admin.GET("/terminal", s.handleTerminal)

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

		// Services list (Needs filtering)
		authed.GET("/services", s.handleGinServicesAll)
	}

	// Admin routes
	r.GET("/version", s.handleGinVersion)

	// Static file serving for web UI and fallback
	r.NoRoute(func(c *gin.Context) {
		if c.Request.Method == http.MethodGet {
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
	var remoteStatus *remote.Status
	if s.remoteManager != nil {
		st := s.remoteManager.Status()
		remoteStatus = &st
	}

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
		out = append(out, s.formatServiceEndpoint(c, ep, remoteStatus))
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
	var remoteStatus *remote.Status
	if s.remoteManager != nil {
		st := s.remoteManager.Status()
		remoteStatus = &st
	}
	for _, ep := range eps {
		formatted := s.formatServiceEndpoint(c, ep, remoteStatus)
		// Add listener health status (RFC 20260125)
		formatted["health"] = s.computeListenerHealth(ep)
		out = append(out, formatted)
	}
	c.JSON(http.StatusOK, gin.H{"services": out})
}

func (s *GinServer) formatServiceEndpoint(c *gin.Context, ep services.ServiceEndpoint, remoteStatus *remote.Status) gin.H {
	remoteHost := s.remoteServiceHostname(remoteStatus, ep)
	var remoteHostValue interface{}
	if remoteHost != "" {
		remoteHostValue = remoteHost
	}
	scheme := determineScheme(ep.Flow, ep.Protocol)
	lanPortURL := s.determineLocalURL(c, ep, scheme)

	result := gin.H{
		"app":          ep.App,
		"name":         ep.Name,
		"guest_port":   ep.GuestPort,
		"host_port":    ep.HostBind,
		"public_port":  ep.PublicPort,
		"remote_ports": ep.RemotePorts,
		"remote_host":  remoteHostValue,
		"flow":         ep.Flow,
		"protocol":     ep.Protocol,
		"primary":      ep.Primary,
		"middleware":   ep.Middleware,
		"scheme":       scheme,
		"local_url":    lanPortURL, // Keep for backward compatibility
		"lan_port_url": lanPortURL, // New explicit name
	}

	// Add host-based URLs only for HTTP/WS listeners (per RFC 20260114)
	if ep.DerivedHostLabel != "" {
		// LAN host URL: only if mDNS is enabled (mdnsManager is nil when disabled)
		if s.mdnsManager != nil {
			lanBase := s.mdnsManager.Hostname()
			// RFC 20260122 §4.4: Use 2-level mDNS format with hyphen separator
			lanHostname := hostnamepkg.DeriveLANHostname(ep.DerivedHostLabel, lanBase)
			lanHostURL := fmt.Sprintf("%s://%s", scheme, lanHostname)
			result["lan_host_url"] = lanHostURL
		}

		// Remote URL: only if remote is enabled
		if remoteHost != "" {
			remoteURL := "https://" + remoteHost
			result["remote_url"] = remoteURL
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
	// These are identified by the secureContextKey set in the secure loopback handler,
	// or by the X-Forwarded-Proto header set by the TLS mux.
	// Internal HTTPS requests (r.TLS != nil from :443 listener) are still LAN requests
	// and should receive local URLs so the UI can offer new-tab fallback.
	if r.Context().Value(secureContextKeyInstance) != nil {
		return nil
	}
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return nil
	}

	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	url := fmt.Sprintf("%s://%s:%d", scheme, host, ep.PublicPort)
	return &url
}

// remoteBaseHostname returns the RFC 20260114 remote base hostname (portal hostname apex).
// It is used as the suffix for all derived remote app hostnames: <label>.<base>.
func remoteBaseHostname(status *remote.Status) string {
	if status == nil || !status.Enabled {
		return ""
	}
	base := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(status.PortalHostname)), ".")
	if base == "" {
		return ""
	}
	// Defensive: remote config should not include ports, but normalize if it does.
	if h, _, err := net.SplitHostPort(base); err == nil {
		base = h
	}
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(base)), ".")
}

func (s *GinServer) remoteServiceHostname(status *remote.Status, ep services.ServiceEndpoint) string {
	// Use remoteBaseHostname for enhanced validation (port handling, etc.)
	base := remoteBaseHostname(status)
	if s == nil || base == "" {
		return ""
	}
	// Delegate to shared implementation (RFC 20260125: avoid logic drift)
	return services.RemoteServiceHostname(ep.DerivedHostLabel, base)
}

func (s *GinServer) handlePersistenceControlExport(c *gin.Context) {
	if s.dispatcher == nil {
		writeGinError(c, http.StatusInternalServerError, "command dispatcher not available")
		return
	}
	resp, err := s.dispatcher.Dispatch(c.Request.Context(), persistence.RunControlExportCommand{})
	if err != nil {
		if errors.Is(err, persistence.ErrNotImplemented) {
			writeGinError(c, http.StatusNotImplemented, "control-plane export not implemented yet")
		} else {
			writeGinError(c, http.StatusInternalServerError, "failed to start control export: "+err.Error())
		}
		return
	}
	artifact, ok := resp.(persistence.ExportArtifact)
	if !ok {
		writeGinError(c, http.StatusInternalServerError, "unexpected response from persistence")
		return
	}
	writeGinSuccess(c, gin.H{"artifact": artifact}, "control-plane export started")
}

func (s *GinServer) handlePersistenceFullExport(c *gin.Context) {
	if s.dispatcher == nil {
		writeGinError(c, http.StatusInternalServerError, "command dispatcher not available")
		return
	}
	resp, err := s.dispatcher.Dispatch(c.Request.Context(), persistence.RunFullExportCommand{})
	if err != nil {
		if errors.Is(err, persistence.ErrNotImplemented) {
			writeGinError(c, http.StatusNotImplemented, "full export not implemented yet")
		} else {
			writeGinError(c, http.StatusInternalServerError, "failed to start full export: "+err.Error())
		}
		return
	}
	artifact, ok := resp.(persistence.ExportArtifact)
	if !ok {
		writeGinError(c, http.StatusInternalServerError, "unexpected response from persistence")
		return
	}
	writeGinSuccess(c, gin.H{"artifact": artifact}, "full export started")
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
// TopicServiceEndpointsChanged: app endpoints change mDNS aliases
// TopicLeadershipRoleChanged: gateway leader may add/remove piccolo.local
// Updates are serialized through a single channel to prevent concurrent
// cert/key writes and reduce redundant I/O during event bursts.
func (s *GinServer) subscribeCertRefresh() {
	if s.certRefreshUnsub != nil {
		s.certRefreshUnsub()
	}
	if s.events == nil {
		return
	}

	ch1, unsub1 := s.events.SubscribeWithCancel(events.TopicServiceEndpointsChanged, 16)
	ch2, unsub2 := s.events.SubscribeWithCancel(events.TopicLeadershipRoleChanged, 4)
	refreshCh := make(chan struct{}, 1)
	done := make(chan struct{})
	s.certRefreshUnsub = func() {
		unsub1() // closes ch1
		unsub2() // closes ch2
		close(done)
	}
	trigger := func() {
		select {
		case refreshCh <- struct{}{}:
		default: // already pending
		}
	}
	go func() {
		for range ch1 {
			trigger()
		}
	}()
	go func() {
		for range ch2 {
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

	caDir := paths.NetworkBootstrapDir()
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
	if s.remoteResolver != nil {
		s.remoteResolver.UpdateConfig(nexusclient.Config{
			PortalHostname: status.PortalHostname,
		})
	}

	// Update allowed frame ancestors for app proxies
	if s.serviceManager != nil {
		var ancestors []string
		if h := strings.TrimSpace(status.PortalHostname); h != "" {
			ancestors = append(ancestors, h)
		}
		s.serviceManager.ProxyManager().SetAllowedAncestors(ancestors)
	}

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
		s.tlsMux.Stop()
		if s.remoteResolver != nil {
			s.remoteResolver.SetTlsMuxPort(0)
		}
	}

	// Detect remote state transition and schedule OIDC app restart
	s.remoteStateMu.Lock()
	wasEnabled := s.remoteStateEnabled
	nowEnabled := status.Enabled && strings.TrimSpace(status.PortalHostname) != ""
	s.remoteStateEnabled = nowEnabled
	s.remoteStateMu.Unlock()

	if wasEnabled != nowEnabled {
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
	appStatusCh := bus.Subscribe(events.TopicAppStatusChanged, 8)
	endpointsCh := bus.Subscribe(events.TopicServiceEndpointsChanged, 8)

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
	payload := gin.H{
		"ready":      ready,
		"status":     s.healthTracker.Overall().String(),
		"components": flattenHealth(snapshot),
	}
	// TODO(ballast): once the health tracker distinguishes fatal states (e.g. control
	// store cannot unlock due to corruption), emit 503 here so MicroOS can roll
	// back automatically. For now we always return 200 to stay compatible with
	// piccolod-health-check-prod.sh which only inspects the status code.
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

func (s *GinServer) stopSecureLoopback() {
	if s == nil || s.secureSrv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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
	hostnames := s.mdnsManager.Hostnames()
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

func (s *GinServer) stopInternalHTTPSListener() {
	if s == nil || s.internalSrv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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
		c.Redirect(http.StatusMovedPermanently, target)
		c.Abort()
	}
}

// isRemoteSecureRequest returns true only for requests arriving through the
// nexus/remote TLS path (secureContextKey) or an HTTPS reverse proxy
// (X-Forwarded-Proto). Internal HTTPS (LAN :443) is NOT included because
// those requests should not receive remote-only headers such as HSTS.
func (s *GinServer) isRemoteSecureRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.Context().Value(secureContextKeyInstance) != nil {
		return true
	}
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return true
	}
	return false
}

func (s *GinServer) isSecureRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	if v := r.Context().Value(secureContextKeyInstance); v != nil {
		return true
	}
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return true
	}
	return false
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
