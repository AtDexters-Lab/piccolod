import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/wifi_models.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Pure selection list of available WiFi networks.
///
///
/// Tapping a network triggers [onNetworkSelected]. The caller is responsible
/// for showing the connect dialog.
class WifiNetworkList extends StatelessWidget {
  const WifiNetworkList({
    required this.networks,
    required this.isScanning,
    required this.onNetworkSelected,
    required this.onRescan,
    super.key,
  });

  final List<WifiNetwork> networks;
  final bool isScanning;
  final void Function(WifiNetwork network) onNetworkSelected;
  final VoidCallback onRescan;

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
                Text('Available Networks',
                    style: PiccoloTheme.textTheme.titleMedium),
                const Spacer(),
                if (isScanning)
                  const SizedBox(
                    width: 16,
                    height: 16,
                    child: CircularProgressIndicator(strokeWidth: 2),
                  )
                else
                  IconButton(
                    icon: const Icon(PiccoloIcons.refresh, size: 20),
                    onPressed: onRescan,
                    tooltip: 'Rescan',
                  ),
              ],
            ),
            const SizedBox(height: 12),
            if (networks.isEmpty)
              const Padding(
                padding: EdgeInsets.symmetric(vertical: 16),
                child: Text('No networks found. Make sure your router is powered on.'),
              )
            else
              ConstrainedBox(
                constraints: const BoxConstraints(maxHeight: 300),
                child: ListView.builder(
                  shrinkWrap: true,
                  itemCount: networks.length,
                  itemBuilder: (context, index) => _NetworkTile(
                    network: networks[index],
                    onTap: () => onNetworkSelected(networks[index]),
                  ),
                ),
              ),
          ],
        ),
      ),
    );
  }
}

class _NetworkTile extends StatelessWidget {
  const _NetworkTile({
    required this.network,
    required this.onTap,
  });

  final WifiNetwork network;
  final VoidCallback onTap;

  @override
  Widget build(BuildContext context) {
    return ListTile(
      dense: true,
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(8)),
      leading: Icon(
        network.signalIcon,
        color: network.signalColor,
        size: 20,
      ),
      title: Text(network.ssid, style: const TextStyle(fontSize: 14)),
      subtitle: Text(
        '${network.security.toUpperCase()} · ${network.band}',
        style: const TextStyle(fontSize: 12, color: PiccoloTheme.inkMuted),
      ),
      trailing: Text(
        '${network.signalDbm} dBm',
        style: const TextStyle(fontSize: 12, color: PiccoloTheme.inkMuted),
      ),
      onTap: onTap,
    );
  }

}
