package server

import (
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type networkPeerResponse struct {
	Hostname  string `json:"hostname"`
	MachineID string `json:"machine_id"`
	IPv4      string `json:"ipv4,omitempty"`
	IPv6      string `json:"ipv6,omitempty"`
	Model     string `json:"model,omitempty"`
	Version   string `json:"version,omitempty"`
	Online    bool   `json:"online"`
}

type networkPeersResponse struct {
	Self  *networkSelfResponse  `json:"self,omitempty"`
	Peers []networkPeerResponse `json:"peers"`
}

type networkSelfResponse struct {
	Hostname         string `json:"hostname"`
	SpecificHostname string `json:"specific_hostname"` // Always piccolo-<machineId>.local
	MachineID        string `json:"machine_id"`
	Model            string `json:"model,omitempty"`
	Version          string `json:"version,omitempty"`
	IsGatewayLeader  bool   `json:"is_gateway_leader"`
}

// handleNetworkPeers returns discovered Piccolo devices on the LAN.
// For remote requests (via loopback proxy), returns empty peers for graceful UI degradation.
func (s *GinServer) handleNetworkPeers(c *gin.Context) {
	// LAN-only check: return empty if accessed via loopback (Nexus proxy)
	// Fail-closed: if we can't parse RemoteAddr, assume remote and return empty
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil {
		c.JSON(http.StatusOK, networkPeersResponse{Peers: []networkPeerResponse{}})
		return
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		// Remote access via Nexus - return empty peers
		c.JSON(http.StatusOK, networkPeersResponse{Peers: []networkPeerResponse{}})
		return
	}

	// No mDNS manager - return empty
	if s.mdnsManager == nil {
		c.JSON(http.StatusOK, networkPeersResponse{Peers: []networkPeerResponse{}})
		return
	}

	// Build self info
	var self *networkSelfResponse
	hostname := s.mdnsManager.Hostname()
	machineID := s.mdnsManager.MachineID()
	specificHostname := s.mdnsManager.SpecificHostname()
	isGatewayLeader := s.mdnsManager.IsGatewayLeader()
	model := s.mdnsManager.DeviceModel()
	version := s.mdnsManager.Version()
	if hostname != "" {
		self = &networkSelfResponse{
			Hostname:         hostname,
			SpecificHostname: specificHostname,
			MachineID:        machineID,
			Model:            model,
			Version:          version,
			IsGatewayLeader:  isGatewayLeader,
		}
	}

	// Build peer list
	// Peers not seen for 3 minutes (3x 60s discovery interval) are considered offline
	discoveredPeers := s.mdnsManager.Peers()
	peers := make([]networkPeerResponse, 0, len(discoveredPeers))
	staleThreshold := time.Now().Add(-180 * time.Second)

	for _, p := range discoveredPeers {
		// Ensure hostname ends with .local (handle both with and without suffix)
		hostname := p.Hostname
		if !strings.HasSuffix(hostname, ".local") {
			hostname += ".local"
		}
		peer := networkPeerResponse{
			Hostname:  hostname,
			MachineID: p.MachineID,
			Model:     p.Model,
			Version:   p.Version,
			Online:    p.LastSeen.After(staleThreshold),
		}
		if p.IPv4 != nil {
			peer.IPv4 = p.IPv4.String()
		}
		if p.IPv6 != nil {
			peer.IPv6 = p.IPv6.String()
		}
		peers = append(peers, peer)
	}

	c.JSON(http.StatusOK, networkPeersResponse{
		Self:  self,
		Peers: peers,
	})
}
