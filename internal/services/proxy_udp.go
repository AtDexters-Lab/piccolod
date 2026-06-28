package services

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"piccolod/internal/services/middleware"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const (
	udpMaxFlows         = 4096 // max concurrent UDP flows
	udpMaxFlowsPerIP    = 64   // max flows per source IP
	udpReadBufSize      = 8192 // sufficient for DNS with EDNS0 and other UDP protocols
	udpFlowIdleTimeout  = 60 * time.Second
	udpSweepInterval    = 30 * time.Second
	udpInterfaceAddrTTL = 5 * time.Second
)

// flowKey is a fixed-size byte key for UDP flow map lookups.
// Avoids per-packet string allocation from srcAddr.String().
// Layout: [2]byte source port + [16]byte source IP + [16]byte local IP
// + [4]byte local interface index.
type flowKey [38]byte

type udpPacketInfo struct {
	localIP       net.IP
	sourceIP      net.IP
	sourceIPKnown bool
	ifIndex       int
}

var udpInterfaceAddrsByIndex = func(ifIndex int) ([]net.Addr, error) {
	iface, err := net.InterfaceByIndex(ifIndex)
	if err != nil {
		return nil, err
	}
	return iface.Addrs()
}

type udpInterfaceAddrCacheEntry struct {
	expiresAt time.Time
	ips       []net.IP
}

var udpInterfaceAddrCache struct {
	sync.Mutex
	byIndex map[int]udpInterfaceAddrCacheEntry
}

// makeFlowKey builds a flowKey from a UDP address. Zero-alloc.
// Uses To16() to normalize all IPs to 16 bytes, avoiding collisions
// between short IPv4 and IPv6 addresses that share a byte prefix.
func makeFlowKey(addr *net.UDPAddr, info udpPacketInfo) flowKey {
	var k flowKey
	if addr == nil {
		return k
	}
	binary.BigEndian.PutUint16(k[:2], uint16(addr.Port))
	if ip16 := addr.IP.To16(); ip16 != nil {
		copy(k[2:18], ip16)
	}
	if ip16 := info.localIP.To16(); ip16 != nil {
		copy(k[18:34], ip16)
	}
	if info.ifIndex > 0 {
		binary.BigEndian.PutUint32(k[34:38], uint32(info.ifIndex))
	}
	return k
}

// udpBufPool recycles 8KB buffers across relay goroutine lifetimes.
// Uses pointer-to-slice to avoid interface boxing allocation on Put.
var udpBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, udpReadBufSize)
		return &b
	},
}

// udpFlow tracks a single source-address-to-backend relay.
// lastActiveNano is accessed from multiple goroutines via atomic operations.
type udpFlow struct {
	srcAddr        *net.UDPAddr
	returnInfo     udpPacketInfo
	backendConn    *net.UDPConn
	lastActiveNano atomic.Int64
}

// udpProxyState manages the lifecycle of a single UDP proxy listener.
type udpPacketConn interface {
	ReadFromUDP([]byte) (int, *net.UDPAddr, udpPacketInfo, error)
	WriteToUDP([]byte, *net.UDPAddr, udpPacketInfo) (int, error)
	Close() error
}

type plainUDPPacketConn struct {
	conn *net.UDPConn
}

func (c plainUDPPacketConn) ReadFromUDP(buf []byte) (int, *net.UDPAddr, udpPacketInfo, error) {
	n, addr, err := c.conn.ReadFromUDP(buf)
	return n, addr, udpPacketInfo{}, err
}

func (c plainUDPPacketConn) WriteToUDP(payload []byte, addr *net.UDPAddr, _ udpPacketInfo) (int, error) {
	return c.conn.WriteToUDP(payload, addr)
}

func (c plainUDPPacketConn) Close() error {
	return c.conn.Close()
}

type ipv4UDPPacketConn struct {
	conn *net.UDPConn
	pc   *ipv4.PacketConn
}

func (c ipv4UDPPacketConn) ReadFromUDP(buf []byte) (int, *net.UDPAddr, udpPacketInfo, error) {
	n, cm, src, err := c.pc.ReadFrom(buf)
	if err != nil {
		return n, nil, udpPacketInfo{}, err
	}
	addr, ok := src.(*net.UDPAddr)
	if !ok {
		return n, nil, udpPacketInfo{}, fmt.Errorf("UDP proxy IPv4 source %T is not *net.UDPAddr", src)
	}
	return n, addr, udpPacketInfoFromIPv4(cm), nil
}

func (c ipv4UDPPacketConn) WriteToUDP(payload []byte, addr *net.UDPAddr, info udpPacketInfo) (int, error) {
	return c.pc.WriteTo(payload, info.ipv4ControlMessage(), addr)
}

func (c ipv4UDPPacketConn) Close() error {
	return c.conn.Close()
}

type ipv6UDPPacketConn struct {
	conn *net.UDPConn
	pc   *ipv6.PacketConn
}

func (c ipv6UDPPacketConn) ReadFromUDP(buf []byte) (int, *net.UDPAddr, udpPacketInfo, error) {
	n, cm, src, err := c.pc.ReadFrom(buf)
	if err != nil {
		return n, nil, udpPacketInfo{}, err
	}
	addr, ok := src.(*net.UDPAddr)
	if !ok {
		return n, nil, udpPacketInfo{}, fmt.Errorf("UDP proxy IPv6 source %T is not *net.UDPAddr", src)
	}
	return n, addr, udpPacketInfoFromIPv6(cm), nil
}

func (c ipv6UDPPacketConn) WriteToUDP(payload []byte, addr *net.UDPAddr, info udpPacketInfo) (int, error) {
	return c.pc.WriteTo(payload, info.ipv6ControlMessage(), addr)
}

func (c ipv6UDPPacketConn) Close() error {
	return c.conn.Close()
}

func listenUDPPacketConn(network string, laddr *net.UDPAddr) (udpPacketConn, error) {
	conn, err := net.ListenUDP(network, laddr)
	if err != nil {
		return nil, err
	}
	wrapped, err := wrapUDPPacketConn(conn, network, laddr)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("enable UDP packet info for %s: %w", laddr, err)
	}
	return wrapped, nil
}

func wrapUDPPacketConn(conn *net.UDPConn, network string, laddr *net.UDPAddr) (udpPacketConn, error) {
	if udpListenUsesIPv4(network, laddr) {
		pc := ipv4.NewPacketConn(conn)
		if err := pc.SetControlMessage(ipv4.FlagDst|ipv4.FlagInterface, true); err != nil {
			return nil, err
		}
		return ipv4UDPPacketConn{conn: conn, pc: pc}, nil
	}
	if udpListenUsesIPv6(network, laddr) {
		pc := ipv6.NewPacketConn(conn)
		if err := pc.SetControlMessage(ipv6.FlagDst|ipv6.FlagInterface, true); err != nil {
			return nil, err
		}
		return ipv6UDPPacketConn{conn: conn, pc: pc}, nil
	}
	return plainUDPPacketConn{conn: conn}, nil
}

func udpListenUsesIPv4(network string, laddr *net.UDPAddr) bool {
	return network == "udp4" || (laddr != nil && laddr.IP.To4() != nil)
}

func udpListenUsesIPv6(network string, laddr *net.UDPAddr) bool {
	return network == "udp6" || (laddr != nil && laddr.IP.To16() != nil && laddr.IP.To4() == nil)
}

func udpPacketInfoFromIPv4(cm *ipv4.ControlMessage) udpPacketInfo {
	if cm == nil {
		return udpPacketInfo{}
	}
	return makeUDPPacketInfo(cm.Dst, cm.IfIndex)
}

func udpPacketInfoFromIPv6(cm *ipv6.ControlMessage) udpPacketInfo {
	if cm == nil {
		return udpPacketInfo{}
	}
	return makeUDPPacketInfo(cm.Dst, cm.IfIndex)
}

func makeUDPPacketInfo(localIP net.IP, ifIndex int) udpPacketInfo {
	ip := packetInfoIP(localIP)
	return udpPacketInfo{
		localIP:       ip,
		sourceIP:      udpSourceIP(ip, ifIndex),
		sourceIPKnown: true,
		ifIndex:       ifIndex,
	}
}

func packetInfoIP(ip net.IP) net.IP {
	if ip == nil || ip.IsUnspecified() {
		return nil
	}
	return append(net.IP(nil), ip...)
}

func (info udpPacketInfo) ipv4ControlMessage() *ipv4.ControlMessage {
	if len(info.localIP) == 0 && info.ifIndex == 0 {
		return nil
	}
	cm := &ipv4.ControlMessage{}
	if ip4 := info.usableSourceIP().To4(); ip4 != nil {
		cm.Src = ip4
	}
	if info.ifIndex > 0 {
		cm.IfIndex = info.ifIndex
	}
	if cm.Src == nil && cm.IfIndex == 0 {
		return nil
	}
	return cm
}

func (info udpPacketInfo) ipv6ControlMessage() *ipv6.ControlMessage {
	if len(info.localIP) == 0 && info.ifIndex == 0 {
		return nil
	}
	cm := &ipv6.ControlMessage{}
	if ip16 := info.usableSourceIP().To16(); ip16 != nil && info.localIP.To4() == nil {
		cm.Src = ip16
	}
	if info.ifIndex > 0 {
		cm.IfIndex = info.ifIndex
	}
	if cm.Src == nil && cm.IfIndex == 0 {
		return nil
	}
	return cm
}

func (info udpPacketInfo) usableSourceIP() net.IP {
	if info.sourceIPKnown {
		return info.sourceIP
	}
	return udpSourceIP(info.localIP, info.ifIndex)
}

func udpSourceIP(ip net.IP, ifIndex int) net.IP {
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
		return nil
	}
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 255 && ip4[1] == 255 && ip4[2] == 255 && ip4[3] == 255 {
			return nil
		}
	}
	if ip.IsLoopback() {
		return append(net.IP(nil), ip...)
	}
	if ifIndex > 0 && !interfaceOwnsExactIP(ifIndex, ip) {
		return nil
	}
	return append(net.IP(nil), ip...)
}

func interfaceOwnsExactIP(ifIndex int, ip net.IP) bool {
	ips := interfaceIPsByIndex(ifIndex, time.Now())
	for _, candidate := range ips {
		if candidate.Equal(ip) {
			return true
		}
	}
	return false
}

func interfaceIPsByIndex(ifIndex int, now time.Time) []net.IP {
	if ifIndex <= 0 {
		return nil
	}

	udpInterfaceAddrCache.Lock()
	if entry, ok := udpInterfaceAddrCache.byIndex[ifIndex]; ok && now.Before(entry.expiresAt) {
		ips := entry.ips
		udpInterfaceAddrCache.Unlock()
		return ips
	}
	udpInterfaceAddrCache.Unlock()

	addrs, err := udpInterfaceAddrsByIndex(ifIndex)
	if err != nil {
		return nil
	}

	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		var ip net.IP
		switch a := addr.(type) {
		case *net.IPNet:
			ip = a.IP
		case *net.IPAddr:
			ip = a.IP
		}
		if ip = packetInfoIP(ip); ip != nil {
			ips = append(ips, ip)
		}
	}

	udpInterfaceAddrCache.Lock()
	if udpInterfaceAddrCache.byIndex == nil {
		udpInterfaceAddrCache.byIndex = make(map[int]udpInterfaceAddrCacheEntry)
	}
	udpInterfaceAddrCache.byIndex[ifIndex] = udpInterfaceAddrCacheEntry{
		expiresAt: now.Add(udpInterfaceAddrTTL),
		ips:       ips,
	}
	udpInterfaceAddrCache.Unlock()
	return ips
}

func localUDPAddrForPacket(defaultAddr *net.UDPAddr, info udpPacketInfo) *net.UDPAddr {
	if defaultAddr == nil || len(info.localIP) == 0 {
		return defaultAddr
	}
	return &net.UDPAddr{
		IP:   append(net.IP(nil), info.localIP...),
		Port: defaultAddr.Port,
		Zone: defaultAddr.Zone,
	}
}

func udpPacketInfoFromContext(ctx middleware.UDPContext) udpPacketInfo {
	if ctx.Local == nil {
		return udpPacketInfo{
			sourceIP:      packetInfoIP(ctx.ReplySourceIP),
			sourceIPKnown: ctx.ReplySourceIPKnown,
			ifIndex:       ctx.LocalIfIndex,
		}
	}
	return udpPacketInfo{
		localIP:       packetInfoIP(ctx.Local.IP),
		sourceIP:      packetInfoIP(ctx.ReplySourceIP),
		sourceIPKnown: ctx.ReplySourceIPKnown,
		ifIndex:       ctx.LocalIfIndex,
	}
}

type udpProxyState struct {
	conn        udpPacketConn
	backendAddr *net.UDPAddr // resolved once at startup
	stopCh      chan struct{}
	stopped     chan struct{}
	mu          sync.Mutex
	flows       map[flowKey]*udpFlow
	ipCounts    map[string]int // keyed by srcAddr.IP.String() (cold path only)
}

func (s *udpProxyState) stop() {
	close(s.stopCh)
	_ = s.conn.Close()

	// Close all backend conns to unblock relayReturn goroutines, then clear
	// the map so concurrent sweepIdleFlows (if ticker fires) is a no-op.
	s.mu.Lock()
	for _, flow := range s.flows {
		_ = flow.backendConn.Close()
	}
	s.flows = nil
	s.ipCounts = nil
	s.mu.Unlock()

	<-s.stopped
}

func (p *ProxyManager) startUDPProxy(ep ServiceEndpoint) {
	if err := p.startUDPProxyChecked(ep); err != nil {
		log.Printf("WARN: %v", err)
	}
}

func (p *ProxyManager) startUDPProxyChecked(ep ServiceEndpoint) error {
	addr := net.JoinHostPort("0.0.0.0", strconv.Itoa(ep.PublicPort))

	p.mu.Lock()
	if _, exists := p.udpListeners[ep.PublicPort]; exists {
		p.mu.Unlock()
		return nil
	}
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		p.mu.Unlock()
		return fmt.Errorf("resolve UDP listener %s: %w", addr, err)
	}
	listenUDP := p.listenUDP
	if listenUDP == nil {
		listenUDP = func(network string, laddr *net.UDPAddr) (udpPacketConn, error) {
			return listenUDPPacketConn(network, laddr)
		}
	}
	conn, err := listenUDP("udp", udpAddr)
	if err != nil {
		p.mu.Unlock()
		return fmt.Errorf("bind UDP listener on %s: %w", addr, err)
	}

	// Resolve backend address once.
	backendStr := net.JoinHostPort("127.0.0.1", strconv.Itoa(ep.HostBind))
	backendUDP, err := net.ResolveUDPAddr("udp", backendStr)
	if err != nil {
		_ = conn.Close()
		p.mu.Unlock()
		return fmt.Errorf("resolve UDP backend %s: %w", backendStr, err)
	}

	state := &udpProxyState{
		conn:        conn,
		backendAddr: backendUDP,
		stopCh:      make(chan struct{}),
		stopped:     make(chan struct{}),
		flows:       make(map[flowKey]*udpFlow),
		ipCounts:    make(map[string]int),
	}
	p.udpListeners[ep.PublicPort] = state
	p.mu.Unlock()

	log.Printf("INFO: UDP proxy %s → %s (app=%s listener=%s)", addr, backendStr, ep.App, ep.Name)

	mwEndpoint := ep.AsMiddlewareInfo()
	udpMws, err := p.registry.BuildL4UDP(middleware.BuildSpec{
		Endpoint:          mwEndpoint,
		HasConnectionAuth: ep.ConnectionAuth.HasIPRules(),
		Deps:              p.buildL4Deps(ep),
	})
	if err != nil {
		_ = conn.Close()
		p.mu.Lock()
		delete(p.udpListeners, ep.PublicPort)
		p.mu.Unlock()
		return fmt.Errorf("registry.BuildL4UDP for app=%s listener=%s: %w", ep.App, ep.Name, err)
	}
	udpTerminal := middleware.UDPHandler(func(ctx middleware.UDPContext, payload []byte, _ middleware.UDPSink) {
		flow := state.getOrCreateFlow(ctx.Source, udpPacketInfoFromContext(ctx))
		if flow == nil {
			return // at capacity
		}
		flow.lastActiveNano.Store(time.Now().UnixNano())
		_, _ = flow.backendConn.Write(payload)
	})
	udpChain := middleware.ComposeL4UDPChain(udpMws, udpTerminal)

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer close(state.stopped)

		go state.sweepLoop()

		buf := make([]byte, udpReadBufSize)
		for {
			n, srcAddr, localInfo, err := conn.ReadFromUDP(buf)
			if err != nil {
				select {
				case <-state.stopCh:
				default:
					if !isClosedConnErr(err) {
						log.Printf("WARN: UDP proxy read error: %v", err)
					}
				}
				return
			}

			ctx := middleware.UDPContext{
				Endpoint:           mwEndpoint,
				Source:             srcAddr,
				Local:              localUDPAddrForPacket(udpAddr, localInfo),
				LocalIfIndex:       localInfo.ifIndex,
				ReplySourceIP:      localInfo.sourceIP,
				ReplySourceIPKnown: localInfo.sourceIPKnown,
				AcceptedAt:         time.Now(),
			}
			udpSink := udpListenerSink{conn: conn, returnInfo: localInfo}
			udpChain(ctx, buf[:n], udpSink)
		}
	}()
	return nil
}

// udpListenerSink adapts the UDP listener conn into middleware.UDPSink so
// L4UDP middlewares can write back to the source (e.g., conn_metrics
// rejection responses in step 5+). Step 4: unused — the empty L4UDP chain
// invokes the terminal directly.
type udpListenerSink struct {
	conn       udpPacketConn
	returnInfo udpPacketInfo
}

func (s udpListenerSink) WriteTo(payload []byte, addr *net.UDPAddr) (int, error) {
	return s.conn.WriteToUDP(payload, addr, s.returnInfo)
}

func isClosedConnErr(err error) bool {
	return errors.Is(err, net.ErrClosed)
}

func (s *udpProxyState) getOrCreateFlow(srcAddr *net.UDPAddr, returnInfo udpPacketInfo) *udpFlow {
	key := makeFlowKey(srcAddr, returnInfo)
	s.mu.Lock()
	defer s.mu.Unlock()

	if flow, ok := s.flows[key]; ok {
		return flow
	}

	// Check per-IP limit first to avoid wasting an eviction slot.
	srcIP := srcAddr.IP.String()
	if s.ipCounts[srcIP] >= udpMaxFlowsPerIP {
		return nil
	}
	// Check global capacity — evict oldest if at limit.
	if len(s.flows) >= udpMaxFlows {
		s.evictOldestLocked()
	}

	backendConn, err := net.DialUDP("udp", nil, s.backendAddr)
	if err != nil {
		log.Printf("WARN: UDP proxy backend dial: %v", err)
		return nil
	}

	flow := &udpFlow{
		srcAddr:     srcAddr,
		returnInfo:  returnInfo,
		backendConn: backendConn,
	}
	flow.lastActiveNano.Store(time.Now().UnixNano())
	s.flows[key] = flow
	s.ipCounts[srcIP]++

	go s.relayReturn(flow, key)
	return flow
}

// relayReturn reads responses from the backend and forwards them to the client.
// Goroutine terminates when backendConn is closed (by sweepIdleFlows eviction
// or stop()), which unblocks the Read call with an error.
// TODO: pre-existing race — if sweep evicts this flow and getOrCreateFlow
// immediately creates a new flow with the same key, this goroutine's
// removeFlow call could delete the new flow. A removeFlowIfSame guard
// comparing the flow pointer would prevent this.
func (s *udpProxyState) relayReturn(flow *udpFlow, key flowKey) {
	bufp := udpBufPool.Get().(*[]byte)
	buf := *bufp
	defer udpBufPool.Put(bufp)

	for {
		n, err := flow.backendConn.Read(buf)
		if err != nil {
			break
		}
		flow.lastActiveNano.Store(time.Now().UnixNano())
		_, _ = s.conn.WriteToUDP(buf[:n], flow.srcAddr, flow.returnInfo)
	}
	s.removeFlow(key)
}

// removeFlowLocked removes a flow and cleans up ipCounts. Caller must hold s.mu.
func (s *udpProxyState) removeFlowLocked(key flowKey) {
	flow, ok := s.flows[key]
	if !ok {
		return
	}
	_ = flow.backendConn.Close()
	srcIP := flow.srcAddr.IP.String()
	s.ipCounts[srcIP]--
	if s.ipCounts[srcIP] <= 0 {
		delete(s.ipCounts, srcIP)
	}
	delete(s.flows, key)
}

func (s *udpProxyState) removeFlow(key flowKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeFlowLocked(key)
}

func (s *udpProxyState) evictOldestLocked() {
	var oldestKey flowKey
	var oldestNano int64
	found := false
	for k, f := range s.flows {
		nano := f.lastActiveNano.Load()
		if !found || nano < oldestNano {
			oldestKey = k
			oldestNano = nano
			found = true
		}
	}
	if found {
		s.removeFlowLocked(oldestKey)
	}
}

func (s *udpProxyState) sweepLoop() {
	ticker := time.NewTicker(udpSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.sweepIdleFlows()
		}
	}
}

func (s *udpProxyState) sweepIdleFlows() {
	nowNano := time.Now().UnixNano()
	timeoutNano := udpFlowIdleTimeout.Nanoseconds()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, flow := range s.flows {
		if nowNano-flow.lastActiveNano.Load() > timeoutNano {
			s.removeFlowLocked(key)
		}
	}
}
