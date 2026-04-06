import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/wifi_models.dart';

class APModeCard extends StatelessWidget {
  const APModeCard({
    required this.apStatus, required this.onToggleSuppression, super.key,
  });

  final WifiAPStatus apStatus;
  final Future<void> Function(bool suppress) onToggleSuppression;

  @override
  Widget build(BuildContext context) {
    return Card(
      child: Padding(
        padding: const EdgeInsets.all(20),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text('Access Point Mode',
                style: Theme.of(context).textTheme.titleMedium),
            const SizedBox(height: 8),
            if (apStatus.active) ...[
              Row(
                children: [
                  const Icon(Icons.wifi_tethering, color: Colors.blue, size: 20),
                  const SizedBox(width: 8),
                  Text('Broadcasting: ${apStatus.ssid ?? "Piccolo-Setup"}',
                      style: const TextStyle(fontSize: 14)),
                ],
              ),
              const SizedBox(height: 4),
              Text(
                'Connect to this network from your phone to configure WiFi.',
                style: TextStyle(fontSize: 13, color: Colors.grey[600]),
              ),
              if (apStatus.clients > 0) ...[
                const SizedBox(height: 4),
                Text(
                  '${apStatus.clients} client${apStatus.clients > 1 ? "s" : ""} connected',
                  style: TextStyle(fontSize: 13, color: Colors.grey[600]),
                ),
              ],
            ] else
              Text(
                'The access point activates automatically when no other network is available.',
                style: TextStyle(fontSize: 13, color: Colors.grey[600]),
              ),
            const SizedBox(height: 12),
            Row(
              children: [
                const Text('Enable AP fallback', style: TextStyle(fontSize: 14)),
                const Spacer(),
                Switch(
                  value: !apStatus.suppressed,
                  onChanged: (value) => onToggleSuppression(!value),
                ),
              ],
            ),
            Text(
              apStatus.suppressed
                  ? 'AP mode is disabled. The device will not broadcast a setup network when offline.'
                  : 'AP mode will activate when no Ethernet or WiFi is available.',
              style: TextStyle(fontSize: 12, color: Colors.grey[500]),
            ),
          ],
        ),
      ),
    );
  }
}
