import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/wifi_models.dart';

class WifiStatusCard extends StatelessWidget {
  const WifiStatusCard({
    required this.status, required this.onForgetNetwork, required this.onScanNetworks, super.key,
  });

  final WifiStatus status;
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
                        style: Theme.of(context).textTheme.titleMedium,
                      ),
                      Text(
                        _statusDescription,
                        style: Theme.of(context).textTheme.bodySmall?.copyWith(
                              color: Colors.grey[600],
                            ),
                      ),
                    ],
                  ),
                ),
                if (status.signalTier != null) _SignalBars(tier: status.signalTier!),
              ],
            ),
            if (status.isWifiConnected && status.ssid != null) ...[
              const Divider(height: 24),
              _InfoRow('Network', status.ssid!),
              if (status.band != null) _InfoRow('Band', status.band!),
              if (status.ipAddress != null) _InfoRow('IP Address', status.ipAddress!),
              if (status.signalDbm != null)
                _InfoRow('Signal', '${status.signalDbm} dBm (${status.signalTier ?? ""})'),
            ],
            const SizedBox(height: 16),
            Wrap(
              spacing: 8,
              children: [
                FilledButton.tonal(
                  onPressed: onScanNetworks,
                  child: Text(status.hasSavedNetwork ? 'Change Network' : 'Connect to WiFi'),
                ),
                if (status.hasSavedNetwork)
                  OutlinedButton(
                    onPressed: onForgetNetwork,
                    child: const Text('Forget Network'),
                  ),
              ],
            ),
          ],
        ),
      ),
    );
  }

  IconData get _uplinkIcon {
    if (status.isEthernet) return Icons.settings_ethernet;
    if (status.isWifiConnected) return Icons.wifi;
    if (status.isReconnecting) return Icons.wifi_find;
    if (status.isAPMode) return Icons.wifi_tethering;
    return Icons.wifi_off;
  }

  Color get _statusColor {
    if (status.isEthernet || status.isWifiConnected) return Colors.green;
    if (status.isReconnecting) return Colors.orange;
    if (status.isAPMode) return Colors.blue;
    return Colors.red;
  }

  String get _connectionLabel {
    if (status.isEthernet) return 'Connected via Ethernet';
    if (status.isWifiConnected) return 'Connected via WiFi';
    if (status.isReconnecting) return 'Reconnecting…';
    if (status.isAPMode) return 'Setup Mode';
    return 'Disconnected';
  }

  String get _statusDescription {
    if (status.isEthernet) return 'Ethernet is the preferred connection.';
    if (status.isWifiConnected) return status.ssid ?? 'WiFi connected';
    if (status.isReconnecting) return 'Attempting to reconnect to ${status.savedSsid ?? "WiFi"}…';
    if (status.isAPMode) return 'Broadcasting access point for WiFi setup.';
    return 'No network connection available.';
  }
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
            child: Text(label, style: TextStyle(color: Colors.grey[600], fontSize: 13)),
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
      'good' || 'fair' => Colors.green,
      'weak' => Colors.orange,
      _ => Colors.red,
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
            color: i < activeBars ? color : Colors.grey[300],
            borderRadius: BorderRadius.circular(1),
          ),
        );
      }),
    );
  }
}
