package services

import (
	"fmt"
	"net"
	"strconv"
)

// PortAllocator allocates ephemeral ports within configured ranges (in-memory)
type PortAllocator struct {
	hostBindRange PortRange
	publicRange   PortRange
	nextHostBind  int
	nextPublic    int
	usedHost      map[int]struct{}
	usedPublic    map[string]struct{} // keyed by "port/proto" (e.g., "35001/tcp", "53/udp")
	portAvailable func(host string, port int, network string) bool
}

func NewPortAllocator(hostBind, public PortRange) *PortAllocator {
	return &PortAllocator{
		hostBindRange: hostBind,
		publicRange:   public,
		nextHostBind:  hostBind.Start,
		nextPublic:    public.Start,
		usedHost:      make(map[int]struct{}),
		usedPublic:    make(map[string]struct{}),
	}
}

func (a *PortAllocator) nextInRange(current int, r PortRange) int {
	if current > r.End {
		return r.Start
	}
	return current
}

func publicKey(port int, proto string) string {
	return strconv.Itoa(port) + "/" + proto
}

func (a *PortAllocator) AllocatePair() (int, int, error) {
	return a.allocatePair(false)
}

func (a *PortAllocator) allocatePair(udp bool) (int, int, error) {
	hb, err := a.allocateHost()
	if err != nil {
		return 0, 0, err
	}
	pp, err := a.allocatePublicForFlow(udp)
	if err != nil {
		a.freeHost(hb)
		return 0, 0, err
	}
	return hb, pp, nil
}

func (a *PortAllocator) allocateHost() (int, error) {
	hb := a.nextInRange(a.nextHostBind, a.hostBindRange)
	startHB := hb
	for {
		if _, ok := a.usedHost[hb]; !ok {
			// Probe OS availability
			if a.isPortAvailable("127.0.0.1", hb, "tcp") {
				a.usedHost[hb] = struct{}{}
				if hb >= a.nextHostBind {
					a.nextHostBind = hb + 1
				}
				return hb, nil
			}
		}
		hb++
		hb = a.nextInRange(hb, a.hostBindRange)
		if hb == startHB {
			return 0, fmt.Errorf("no available host-bind ports in range %d-%d", a.hostBindRange.Start, a.hostBindRange.End)
		}
	}
}

// allocatePublic auto-allocates a TCP public port from the range.
func (a *PortAllocator) allocatePublic() (int, error) {
	return a.allocatePublicForFlow(false)
}

func (a *PortAllocator) allocatePublicForFlow(udp bool) (int, error) {
	pp := a.nextInRange(a.nextPublic, a.publicRange)
	startPP := pp
	network := "tcp"
	if udp {
		network = "udp"
	}
	for {
		key := publicKey(pp, "tcp")
		if _, ok := a.usedPublic[key]; !ok {
			_, udpClaimed := a.usedPublic[publicKey(pp, "udp")]
			if (!udp || !udpClaimed) && a.isPortAvailable("", pp, network) {
				a.usedPublic[key] = struct{}{}
				if pp >= a.nextPublic {
					a.nextPublic = pp + 1
				}
				return pp, nil
			}
		}
		pp++
		pp = a.nextInRange(pp, a.publicRange)
		if pp == startPP {
			return 0, fmt.Errorf("no available public ports in range %d-%d", a.publicRange.Start, a.publicRange.End)
		}
	}
}

func isPortFree(host string, port int) bool {
	return isPortAvailable(host, port, "tcp")
}

func (a *PortAllocator) isPortAvailable(host string, port int, network string) bool {
	if a.portAvailable != nil {
		return a.portAvailable(host, port, network)
	}
	return isPortAvailable(host, port, network)
}

func isPortAvailable(host string, port int, network string) bool {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	if network == "udp" {
		pc, err := net.ListenPacket("udp", addr)
		if err != nil {
			return false
		}
		pc.Close()
		return true
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// ReserveHost reserves an existing host-bind port so it won't be reused.
func (a *PortAllocator) ReserveHost(port int) error {
	if port < a.hostBindRange.Start || port > a.hostBindRange.End {
		return fmt.Errorf("host port %d outside allocator range %d-%d", port, a.hostBindRange.Start, a.hostBindRange.End)
	}
	if _, exists := a.usedHost[port]; exists {
		return fmt.Errorf("host port %d already reserved", port)
	}
	a.usedHost[port] = struct{}{}
	if port >= a.nextHostBind {
		a.nextHostBind = port + 1
	}
	return nil
}

// AllocatePublic allocates only a public proxy port.
func (a *PortAllocator) AllocatePublic() (int, error) {
	return a.allocatePublic()
}

func (a *PortAllocator) freeHost(port int) {
	delete(a.usedHost, port)
}

func (a *PortAllocator) freePublic(port int) {
	// Auto-allocated ports are always TCP.
	delete(a.usedPublic, publicKey(port, "tcp"))
}

// FreePublicProto releases a specific port+protocol combination.
// Use this for port claims where TCP and UDP are tracked independently.
func (a *PortAllocator) FreePublicProto(port int, proto string) {
	delete(a.usedPublic, publicKey(port, proto))
}

func (a *PortAllocator) Release(host, public int) {
	if host > 0 {
		a.freeHost(host)
	}
	if public > 0 {
		a.freePublic(public)
	}
}

func (a *PortAllocator) ReleaseHost(port int) {
	if port > 0 {
		a.freeHost(port)
	}
}

func (a *PortAllocator) ReleasePublic(port int) {
	if port > 0 {
		a.freePublic(port)
	}
}

// ClaimPublicPort reserves a specific public port for a port claim.
// Unlike AllocatePublic, this accepts ports outside the auto-allocate range.
// Protocol-aware: TCP 53 and UDP 53 are tracked independently, allowing
// different apps to claim the same port number on different protocols.
func (a *PortAllocator) ClaimPublicPort(port int, udp bool) error {
	network := "tcp"
	if udp {
		network = "udp"
	}
	key := publicKey(port, network)
	if _, exists := a.usedPublic[key]; exists {
		return fmt.Errorf("%s port %d already in use", network, port)
	}
	if !a.isPortAvailable("", port, network) {
		return fmt.Errorf("%s port %d not available on host", network, port)
	}
	a.usedPublic[key] = struct{}{}
	return nil
}

// AllocateWithClaim allocates a host-bind port from the normal range and claims
// a specific public port. On claim failure the host-bind port is released.
func (a *PortAllocator) AllocateWithClaim(claimPort int, udp bool) (int, error) {
	hb, err := a.allocateHost()
	if err != nil {
		return 0, err
	}
	if err := a.ClaimPublicPort(claimPort, udp); err != nil {
		a.freeHost(hb)
		return 0, fmt.Errorf("claim port %d: %w", claimPort, err)
	}
	return hb, nil
}

// AllocateForClaim allocates ports for a listener, using port claim logic when
// PortClaim is set, otherwise auto-allocating from the ranges. Returns (hostBind, publicPort, error).
func (a *PortAllocator) AllocateForClaim(portClaim *int, udp bool) (int, int, error) {
	if portClaim != nil {
		hb, err := a.AllocateWithClaim(*portClaim, udp)
		if err != nil {
			return 0, 0, err
		}
		return hb, *portClaim, nil
	}
	return a.allocatePair(udp)
}
