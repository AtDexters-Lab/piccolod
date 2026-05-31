package server

import (
	"context"
	"errors"

	"piccolod/internal/api"
	"piccolod/internal/app"
)

var errCatalogManagerUnavailable = errors.New("catalog manager unavailable")

// catalogSyncHost adapts GinServer to the app.SyncHost interface so the app
// manager's catalog manifest sync subsystem can reach host-level concerns
// (catalog template fetch, OIDC client manager, proxy register/delete) without
// taking a circular import on the server package.
type catalogSyncHost struct {
	server *GinServer
}

func (h catalogSyncHost) FetchCatalogTemplate(ctx context.Context, catalogSource string) ([]byte, error) {
	if h.server == nil || h.server.catalogManager == nil {
		return nil, errCatalogManagerUnavailable
	}
	yaml, err := h.server.catalogManager.GetAppTemplate(ctx, catalogSource)
	if err != nil {
		return nil, err
	}
	return []byte(yaml), nil
}

func (h catalogSyncHost) FetchInitScript(ctx context.Context, catalogSource, filePath string) ([]byte, error) {
	if h.server == nil || h.server.catalogManager == nil {
		return nil, errCatalogManagerUnavailable
	}
	return h.server.catalogManager.FetchAppFile(ctx, catalogSource, filePath)
}

func (h catalogSyncHost) CurrentInstallSystemContext() app.InstallSystemContext {
	if h.server == nil {
		return app.InstallSystemContext{}
	}
	return h.server.buildInstallSystemContext()
}

func (h catalogSyncHost) OIDCClientGenerator() app.OIDCClientGenerator {
	if h.server == nil {
		return nil
	}
	mgr := h.server.getOIDCClientManager()
	if mgr == nil {
		// Return a true-nil interface, not a typed nil wrapped in an
		// interface (which would compare != nil at the call site).
		return nil
	}
	var gen app.OIDCClientGenerator = mgr
	return gen
}

func (h catalogSyncHost) PersistOIDCClient(ctx context.Context, clientID, clientSecret, appID string) error {
	if h.server == nil {
		return errCatalogManagerUnavailable
	}
	mgr := h.server.getOIDCClientManager()
	if mgr == nil {
		return errCatalogManagerUnavailable
	}
	return mgr.CreateClient(ctx, clientID, clientSecret, appID)
}

func (h catalogSyncHost) DeleteOIDCClient(ctx context.Context, clientID string) error {
	if h.server == nil {
		return errCatalogManagerUnavailable
	}
	mgr := h.server.getOIDCClientManager()
	if mgr == nil {
		return errCatalogManagerUnavailable
	}
	return mgr.DeleteClient(ctx, clientID)
}

func (h catalogSyncHost) RequiresProxyOIDCClient(def *api.AppDefinition) bool {
	if h.server == nil {
		return false
	}
	return h.server.requiresProxyOIDCClient(def)
}

func (h catalogSyncHost) RegisterProxyOIDCClient(ctx context.Context, instanceID string) error {
	if h.server == nil {
		return errCatalogManagerUnavailable
	}
	return h.server.registerProxyOIDCClient(ctx, instanceID)
}

func (h catalogSyncHost) DeleteProxyOIDCClient(ctx context.Context, instanceID string) {
	if h.server == nil {
		return
	}
	h.server.deleteProxyOIDCClient(ctx, instanceID)
}
