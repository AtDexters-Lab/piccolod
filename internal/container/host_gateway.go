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

// HostGatewayEntry returns a HostEntry for piccolo.local pointing to the host.
func HostGatewayEntry() (HostEntry, error) {
	ip, err := GetHostGatewayIP()
	if err != nil {
		return HostEntry{}, err
	}

	return HostEntry{
		Hostname: "piccolo.local",
		IP:       ip,
	}, nil
}
