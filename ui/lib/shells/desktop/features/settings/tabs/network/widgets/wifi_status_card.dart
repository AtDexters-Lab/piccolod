import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/wifi_models.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class WifiStatusCard extends StatelessWidget {
  const WifiStatusCard({
    required this.status,
    required this.onForgetNetwork,
    required this.onScanNetworks,
    super.key,
  });

  final NetworkStatus status;
  final VoidCallback onForgetNetwork;
  final VoidCallback onScanNetworks;

  @override
  Widget build(BuildContext context) {
    return Card(
      child: Padding(
        padding: const EdgeInsets.all(20),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Row(
              children: [
                Icon(_uplinkIcon, size: 24, color: _statusColor),
                const SizedBox(width: 12),
                Expanded(
                  child: Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      Text(
                        _connectionLabel,
                        style: PiccoloTheme.textTheme.titleMedium,
                      ),
                      Text(
                        _statusDescription,
                        style: PiccoloTheme.textTheme.bodySmall,
                      ),
                    ],
                  ),
                ),
                if (status.isWifiConnected && status.signalTier != null)
                  _SignalBars(tier: status.signalTier!),
              ],
            ),
            if (_hasConnectionDetails) ...[
              const Divider(height: 24),
              if (status.activeUplinkIface != null)
                _InfoRow('Interface', status.activeUplinkIface!),
              if (status.isWifiConnected && status.ssid != null)
                _InfoRow('Network', status.ssid!),
              if (status.band != null) _InfoRow('Band', status.band!),
              if (status.ipAddress != null)
                _InfoRow('IP Address', status.ipAddress!),
              if (status.signalDbm != null)
                _InfoRow(
                  'Signal',
                  '${status.signalDbm} dBm (${status.signalTier ?? ""})',
                ),
            ],
            if (status.available) ...[
              const SizedBox(height: 16),
              Wrap(
                spacing: 8,
                children: [
                  FilledButton.tonal(
                    onPressed: onScanNetworks,
                    child: Text(
                      status.hasSavedNetwork
                          ? 'Change Network'
                          : 'Connect to WiFi',
                    ),
                  ),
                  if (status.hasSavedNetwork)
                    OutlinedButton(
                      onPressed: onForgetNetwork,
                      child: const Text('Forget Network'),
                    ),
                ],
              ),
            ],
          ],
        ),
      ),
    );
  }

  IconData get _uplinkIcon {
    if (status.isAPMode) return PiccoloIcons.accessPoint;
    if (status.isPortal) return Icons.login;
    if (status.isUnknown) return Icons.sync;
    if (status.isLimitedConnectivity) {
      return Icons.signal_wifi_statusbar_connected_no_internet_4;
    }
    if (status.isEthernet) return PiccoloIcons.ethernet;
    if (status.isWifiConnected) return PiccoloIcons.wifi;
    if (status.isReconnecting) return PiccoloIcons.wifiNone;
    return PiccoloIcons.wifiOff;
  }

  Color get _statusColor {
    if (status.isAPMode) return PiccoloTheme.info;
    if (status.isUnknown) return PiccoloTheme.inkMuted;
    if (status.isLimitedConnectivity ||
        status.isPortal ||
        status.isReconnecting) {
      return PiccoloTheme.warning;
    }
    if (status.isEthernet || status.isWifiConnected) {
      return PiccoloTheme.success;
    }
    return PiccoloTheme.critical;
  }

  String get _connectionLabel {
    if (status.isAPMode) return 'Setup Mode';
    if (status.isPortal) return 'Sign-in required';
    if (status.isUnknown) return 'Checking network';
    if (status.isLimitedConnectivity) return 'Connected (limited)';
    if (status.isEthernet) return 'Connected via Ethernet';
    if (status.isWifiConnected) return 'Connected via WiFi';
    if (status.isReconnecting) return 'Reconnecting…';
    return 'Disconnected';
  }

  String get _statusDescription {
    if (status.isAPMode) return 'Broadcasting access point for WiFi setup.';
    if (status.isPortal) {
      return 'This network requires browser sign-in, which Piccolo cannot complete. Choose another network.';
    }
    if (status.isUnknown) {
      return 'Network status is temporarily unavailable. Piccolo will retry.';
    }
    if (status.isLimitedConnectivity) {
      final transport = status.activeUplink == 'ethernet' ? 'Ethernet' : 'WiFi';
      return 'Connected via $transport, but internet access is limited.';
    }
    if (status.isEthernet) return 'Ethernet is the preferred connection.';
    if (status.isWifiConnected) return status.ssid ?? 'WiFi connected';
    if (status.isReconnecting) {
      return 'Attempting to reconnect to ${status.savedSsid ?? "WiFi"}…';
    }
    return 'No network connection available.';
  }

  bool get _hasConnectionDetails =>
      status.activeUplinkIface != null ||
      (status.isWifiConnected && status.ssid != null);
}

class _InfoRow extends StatelessWidget {
  const _InfoRow(this.label, this.value);
  final String label;
  final String value;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.only(bottom: 4),
      child: Row(
        children: [
          SizedBox(
            width: 100,
            child: Text(
              label,
              style: const TextStyle(
                color: PiccoloTheme.inkMuted,
                fontSize: 13,
              ),
            ),
          ),
          Text(value, style: const TextStyle(fontSize: 13)),
        ],
      ),
    );
  }
}

class _SignalBars extends StatelessWidget {
  const _SignalBars({required this.tier});
  final String tier;

  @override
  Widget build(BuildContext context) {
    final activeBars = switch (tier) {
      'good' => 4,
      'fair' => 3,
      'weak' => 2,
      _ => 1,
    };
    final color = switch (tier) {
      'good' || 'fair' => PiccoloTheme.success,
      'weak' => PiccoloTheme.warning,
      _ => PiccoloTheme.critical,
    };

    return Row(
      mainAxisSize: MainAxisSize.min,
      crossAxisAlignment: CrossAxisAlignment.end,
      children: List.generate(4, (i) {
        final height = 6.0 + i * 4.0;
        return Container(
          width: 4,
          height: height,
          margin: const EdgeInsets.only(left: 2),
          decoration: BoxDecoration(
            color: i < activeBars ? color : PiccoloTheme.hairline,
            borderRadius: BorderRadius.circular(1),
          ),
        );
      }),
    );
  }
}
