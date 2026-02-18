package container

// GetHostGatewayIP returns "host-gateway" which Podman resolves to the host machine.
// For older versions or runtimes that don't support it, this might need adjustment,
// but "host-gateway" is the standard way to reference the host from a container.
func GetHostGatewayIP() (string, error) {
	// Use the special value "host-gateway".
	// Podman/Docker resolves this to the correct gateway IP (e.g. 10.0.2.2 or 192.168.65.2)
	// when used in --add-host.
	return "host-gateway", nil
}

// HostGatewayEntries returns a HostEntry for the given OIDC hostname pointing
// to the host machine, enabling container OIDC back-channel communication.
func HostGatewayEntries(oidcHostname string) ([]HostEntry, error) {
	ip, err := GetHostGatewayIP()
	if err != nil {
		return nil, err
	}
	return []HostEntry{{Hostname: oidcHostname, IP: ip}}, nil
}
