import 'package:flutter/material.dart';
import '../../../core/models/network_models.dart';
import '../../../theme/piccolo_icons.dart';
import '../../../theme/piccolo_theme.dart';

/// A widget that displays a list of discovered Piccolo devices.
///
/// Each device is shown as a card with hostname, model, and IP address.
/// Online devices can be clicked to navigate to them.
/// Offline devices are shown with reduced opacity and disabled.
class DeviceSelector extends StatelessWidget {
  final List<DiscoveredPeer> onlinePeers;
  final List<DiscoveredPeer> offlinePeers;
  final NetworkSelf? self;
  final void Function(DiscoveredPeer) onDeviceSelected;
  final void Function(DiscoveredPeer)? onDeviceSelectedHttps;

  const DeviceSelector({
    super.key,
    required this.onlinePeers,
    required this.offlinePeers,
    required this.self,
    required this.onDeviceSelected,
    this.onDeviceSelectedHttps,
  });

  @override
  Widget build(BuildContext context) {
    final allDevices = [
      // Add self as first device if available
      if (self != null) _SelfDevice(self: self!),
      // Online peers
      ...onlinePeers.map((p) => _PeerDevice(peer: p, online: true)),
      // Offline peers
      ...offlinePeers.map((p) => _PeerDevice(peer: p, online: false)),
    ];

    if (allDevices.isEmpty) {
      return Center(
        child: Text(
          'No devices found',
          style: TextStyle(
            color: Colors.white.withValues(alpha: 0.7),
            fontSize: 16,
          ),
        ),
      );
    }

    return ListView.separated(
      shrinkWrap: true,
      padding: const EdgeInsets.symmetric(vertical: Spacing.sm),
      itemCount: allDevices.length,
      separatorBuilder: (context, index) => const SizedBox(height: Spacing.md),
      itemBuilder: (context, index) {
        final device = allDevices[index];
        if (device is _SelfDevice) {
          // Show self device like peers - use specificHostname for display
          final specificHost = device.self.specificHostname.isNotEmpty
              ? device.self.specificHostname
              : device.self.hostname;
          final selfPeer = DiscoveredPeer(
            hostname: specificHost,
            machineId: device.self.machineId,
            online: true,
          );
          return _DeviceCard(
            displayName: device.self.displayName,
            subtitle: device.self.model ?? '',
            ipAddress: null, // We don't have our own IP in self response
            online: true,
            onTap: () => onDeviceSelected(selfPeer),
            onHttpsTap: onDeviceSelectedHttps != null
                ? () => onDeviceSelectedHttps!(selfPeer)
                : null,
          );
        } else if (device is _PeerDevice) {
          return _DeviceCard(
            displayName: device.peer.displayName,
            subtitle: device.peer.model ?? '',
            ipAddress: device.peer.ipv4 ?? device.peer.ipv6,
            online: device.online,
            onTap: device.online ? () => onDeviceSelected(device.peer) : null,
            onHttpsTap: device.online && onDeviceSelectedHttps != null
                ? () => onDeviceSelectedHttps!(device.peer)
                : null,
          );
        }
        return const SizedBox.shrink();
      },
    );
  }
}

class _SelfDevice {
  final NetworkSelf self;
  _SelfDevice({required this.self});
}

class _PeerDevice {
  final DiscoveredPeer peer;
  final bool online;
  _PeerDevice({required this.peer, required this.online});
}

class _DeviceCard extends StatelessWidget {
  final String displayName;
  final String subtitle;
  final String? ipAddress;
  final bool online;
  final VoidCallback? onTap;
  final VoidCallback? onHttpsTap;

  const _DeviceCard({
    required this.displayName,
    required this.subtitle,
    required this.ipAddress,
    required this.online,
    required this.onTap,
    this.onHttpsTap,
  });

  @override
  Widget build(BuildContext context) {
    return Opacity(
      opacity: online ? 1.0 : 0.5,
      child: Material(
        color: Colors.white.withValues(alpha: 0.1),
        borderRadius: BorderRadius.circular(Radii.md),
        child: InkWell(
          onTap: onTap,
          borderRadius: BorderRadius.circular(Radii.md),
          child: Container(
            padding: const EdgeInsets.all(Spacing.base),
            child: Row(
              children: [
                // Status indicator
                Container(
                  width: 12,
                  height: 12,
                  decoration: BoxDecoration(
                    shape: BoxShape.circle,
                    color: online
                        ? PiccoloTheme.success
                        : PiccoloTheme.inkMuted,
                  ),
                ),
                const SizedBox(width: Spacing.base),
                // Device info
                Expanded(
                  child: Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      Text(
                        displayName,
                        style: const TextStyle(
                          color: Colors.white,
                          fontSize: 16,
                          fontWeight: FontWeight.w600,
                        ),
                      ),
                      if (subtitle.isNotEmpty || ipAddress != null)
                        Text(
                          [
                            if (subtitle.isNotEmpty) subtitle,
                            if (ipAddress != null) ipAddress,
                          ].join(' \u2022 '),
                          style: TextStyle(
                            color: Colors.white.withValues(alpha: 0.7),
                            fontSize: 13,
                          ),
                        ),
                    ],
                  ),
                ),
                // HTTPS button for online devices
                if (online && onHttpsTap != null) ...[
                  IconButton(
                    onPressed: onHttpsTap,
                    icon: const Icon(PiccoloIcons.lock, size: 18),
                    tooltip: 'Open via HTTPS',
                    color: Colors.white.withValues(alpha: 0.7),
                    padding: EdgeInsets.zero,
                    constraints: const BoxConstraints(minWidth: 36, minHeight: 36),
                  ),
                  const SizedBox(width: Spacing.xs),
                ],
                // Arrow indicator for online devices (HTTP)
                if (online)
                  Icon(
                    PiccoloIcons.arrowForwardIos,
                    color: Colors.white.withValues(alpha: 0.5),
                    size: 16,
                  ),
              ],
            ),
          ),
        ),
      ),
    );
  }
}
