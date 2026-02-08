package app

import (
	"context"
	"log"
	"os/exec"
	"strings"
)

// flushAndReloadNetavarkRules clears stale nftables DNAT rules left by netavark
// after container removal. Called once at startup before the first reconcile tick.
//
// Netavark creates DNAT rules on container create but doesn't fully clean them up
// on container remove. When a new container reuses a host-bind port that has a stale
// rule, the stale rule sits higher in the chain and matches first, causing 502s.
//
// Strategy:
//  1. Delete the entire netavark nftables table (removes all chains and rules).
//     We must use "delete table" rather than "flush table" because flush preserves
//     empty chain structures. Netavark's idempotency checks see the existing chains
//     and skip recreating per-network masquerade rules, breaking outbound container
//     connectivity.
//  2. Reload network configuration for every running network anchor so their
//     rules are recreated fresh (netavark recreates the table and all chains from
//     scratch since they no longer exist).
func (m *AppManager) flushAndReloadNetavarkRules(ctx context.Context) {
	// Step 1: Delete the netavark nftables table.
	// All failures are logged internally and treated as non-fatal.
	deleteNetavarkTable(ctx)

	// Step 2: Reload network for all running anchors.
	// Use ensureStateManager to initialize state if needed (first call at startup).
	// If state is unavailable (locked, unmounted), skip reload — ReconcileOnce will
	// handle it once the state becomes available.
	state, err := m.ensureStateManager()
	if err != nil {
		log.Printf("INFO: netavark repair: state unavailable, skipping reload: %v", err)
		return
	}

	apps := state.ListApps()
	for _, app := range apps {
		if app == nil || !app.Enabled {
			continue
		}
		anchorCID := strings.TrimSpace(app.PublishContainerID())
		if anchorCID == "" {
			continue
		}

		layout, err := m.ensureAppVolumeLayout(ctx, app.InstanceID)
		if err != nil {
			log.Printf("WARN: netavark repair: skip %s: volume layout: %v", app.InstanceID, err)
			continue
		}

		mode := app.Mode()
		runtime, err := m.podmanRuntimeForApp(app.InstanceID, layout, mode)
		if err != nil {
			log.Printf("WARN: netavark repair: skip %s: podman runtime: %v", app.InstanceID, err)
			continue
		}

		anchorName := networkAnchorContainerName(app.InstanceID)
		if err := m.containerManager.NetworkReload(ctx, runtime, anchorName); err != nil {
			log.Printf("WARN: netavark repair: reload %s (%s): %v", app.InstanceID, anchorName, err)
		} else {
			log.Printf("INFO: netavark repair: reloaded network for %s", app.InstanceID)
		}
	}
}

// deleteNetavarkTable deletes all netavark nftables tables across all table families.
// Netavark may use "inet" (newer) or "ip"/"ip6" (older) table families depending
// on version. We try all three; missing tables are treated as benign.
// All failures are non-fatal since reloading running containers still provides value.
//
// We use "delete table" (not "flush table") because flush preserves empty chain
// structures that poison netavark's idempotency checks, preventing it from
// recreating per-network masquerade rules on subsequent container operations.
func deleteNetavarkTable(ctx context.Context) {
	nftPath, err := exec.LookPath("nft")
	if err != nil {
		log.Printf("INFO: nft binary not found, skipping netavark cleanup (no nftables = no stale rules)")
		return
	}

	// Try all table families — netavark version determines which exists.
	for _, family := range []string{"inet", "ip", "ip6"} {
		deleteNftTable(ctx, nftPath, family, "netavark")
	}
}

// deleteNftTable deletes a single nftables table. Missing tables are logged at INFO
// level; other failures are logged as warnings.
func deleteNftTable(ctx context.Context, nftPath, family, table string) {
	cmd := exec.CommandContext(ctx, nftPath, "delete", "table", family, table)
	output, err := cmd.CombinedOutput()
	if err != nil {
		outStr := strings.ToLower(string(output))
		if strings.Contains(outStr, "no such file or directory") ||
			(strings.Contains(outStr, "table") && strings.Contains(outStr, "does not exist")) {
			return
		}
		log.Printf("WARN: nft delete table %s %s failed: %s", family, table, strings.TrimSpace(string(output)))
		return
	}
	log.Printf("INFO: deleted netavark nftables table %s %s", family, table)
}
