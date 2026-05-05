package network

import (
	"log"
	"time"

	"piccolod/internal/network/nmclient"
)

type rawEthObs struct {
	Present  bool
	Iface    string
	NMState  nmclient.NMDeviceState
	NMReason nmclient.NMDeviceStateReason
	Carrier  bool
	HasIP    bool
}

func (p *Prober) probeEth() rawEthObs {
	raw := rawEthObs{}

	devs, err := p.nm.EthernetDevices()
	if err != nil {
		log.Printf("WARN: probe_eth: list devices: %v", err)
		return raw
	}
	if len(devs) == 0 {
		return raw
	}
	dev := devs[0]
	raw.Present = true
	raw.Iface = dev.Interface
	raw.Carrier = dev.Carrier
	raw.NMState = dev.State

	if cached, ok := p.lastReason(dev.Path); ok {
		raw.NMReason = cached
	}
	if info, err := p.nm.ActiveConnectionInfo(dev.Path); err == nil && info != nil {
		raw.HasIP = info.IP4Address != ""
	}

	p.devicePath[DeviceEthernet] = dev.Path
	return raw
}

func (p *Prober) resolveEth(raw rawEthObs, led ActionLedger, sysUptime, quietPeriod, grace time.Duration, now time.Time) DeviceObservation {
	obs := DeviceObservation{
		Kind:     DeviceEthernet,
		Present:  raw.Present,
		Iface:    raw.Iface,
		NMState:  nmStateString(raw.NMState),
		NMReason: nmReasonString(raw.NMReason),
		HasIP:    raw.HasIP,
	}
	if !raw.Present {
		obs.HWHealth = TriInactive
		obs.ConfigHealth = TriInactive
		obs.GwReachable = TriInactive
		return obs
	}
	obs.LinkUp = raw.Carrier && raw.NMState >= nmclient.NMDeviceStateIPConfig

	switch {
	case !raw.Carrier:
		// No cable plugged in — Inactive (catalog B1).
		obs.HWHealth = TriInactive
	case sysUptime < grace:
		// Cold-boot grace.
		obs.HWHealth = TriHealthy
	case raw.NMState <= nmclient.NMDeviceStateUnavailable:
		// Carrier=1 + NM Unavailable past grace = HW wedge (catalog A3).
		obs.HWHealth = p.dampenHW(DeviceEthernet, TriFaulted, led, quietPeriod, now)
	default:
		obs.HWHealth = p.dampenHW(DeviceEthernet, TriHealthy, led, quietPeriod, now)
	}

	switch {
	case obs.HWHealth != TriHealthy:
		obs.ConfigHealth = TriInactive
	case raw.NMState == nmclient.NMDeviceStateActivated && raw.HasIP:
		obs.ConfigHealth = TriHealthy
	default:
		obs.ConfigHealth = TriHealthy // DHCP-in-flight maps to Healthy
	}

	return obs
}
