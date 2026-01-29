# RFC: mDNS Virtual Interface Filtering

## Problem Statement

The mDNS implementation attempts to use all network interfaces that are "up" and non-loopback. This causes problems with virtual container interfaces (podman0, veth*, docker0, br-*) that:

1. Cannot send multicast traffic to the local network
2. Fail immediately with "no such device" or "network is unreachable"
3. Cause log noise and unnecessary resilience tracking

Example log output before fix:
```
WARN: Failed to send IPv4 announcement on podman0: no such device
RESILIENCE: Interface podman0 failed (attempt 1), backing off for 10s
WARN: Failed to send IPv6 announcement on podman0: network is unreachable
RESILIENCE: Interface podman0 failed (attempt 2), backing off for 20s
... (repeats)
```

## Solution

Add two checks to `setupInterface()` in `internal/mdns/interface.go`:

1. **FlagMulticast check** - Skip interfaces without multicast capability
2. **Virtual interface name filtering** - Skip interfaces matching known virtual/container patterns

### Virtual Interface Prefixes

```go
var virtualInterfacePrefixes = []string{
    // Container runtimes
    "podman", "docker", "cni",
    // Virtual ethernet pairs
    "veth", "vnet",
    // Virtual bridges
    "br-", "virbr",
    // Tunnel interfaces
    "tap", "tun",
    // Dummy/test interfaces
    "dummy",
    // macOS/BSD specific
    "utun", "awdl", "llw", "gif", "stf",
    // Kubernetes CNI plugins
    "flannel", "cali", "weave",
    // LXC/LXD containers
    "lxc", "lxd",
    // Hypervisors
    "vbox", "vmnet", "hyperv",
}
```

### Logging

- Skipped interfaces log at DEBUG level (not WARN) since this is intentional filtering
- Only unexpected setup failures trigger WARN and resilience tracking

## Expected Behavior After Fix

```
DEBUG: Skipping interface podman0: interface podman0 is a virtual interface
DEBUG: Skipping interface veth0: interface veth0 is a virtual interface
INFO: Interface enp0s3 ready - IPv4:192.168.0.127, IPv6:fe80::a00:27ff:fe6b:b051
INFO: Successfully configured 1 network interfaces for mDNS
```

## Implementation Notes & Status

| Change | Status | Location |
|--------|--------|----------|
| Add `virtualInterfacePrefixes` list | Done | `internal/mdns/interface.go` |
| Add `isVirtualInterface()` helper | Done | `internal/mdns/interface.go` |
| Add `FlagMulticast` check | Done | `internal/mdns/interface.go:setupInterface()` |
| Change log level for skipped interfaces | Done | `internal/mdns/interface.go:discoverInterfaces()` |
| Filter virtual interfaces in `checkInterfaceChanges()` | Done | `internal/mdns/interface.go:checkInterfaceChanges()` |
| Unit tests for `isVirtualInterface()` | Done | `internal/mdns/interface_test.go` |
