import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/event_stream_client.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/network/network_controller.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/network/widgets/ap_mode_card.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/network/widgets/wifi_network_list.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/network/widgets/wifi_status_card.dart';

class NetworkTab extends StatefulWidget {
  const NetworkTab({super.key, this.eventStreamClient});

  final EventStreamClient? eventStreamClient;

  @override
  State<NetworkTab> createState() => _NetworkTabState();
}

class _NetworkTabState extends State<NetworkTab> {
  late final NetworkController _controller;

  @override
  void initState() {
    super.initState();
    _controller = NetworkController(
      apiClient: ApiClient(),
      eventStreamClient: widget.eventStreamClient,
    );
  }

  @override
  void dispose() {
    _controller.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return ListenableBuilder(
      listenable: _controller,
      builder: (context, _) {
        if (_controller.isLoading) {
          return const Center(child: CircularProgressIndicator());
        }

        if (_controller.error != null && _controller.status == null) {
          if (_controller.error == 'remote_access') {
            return Center(
              child: Padding(
                padding: const EdgeInsets.all(32),
                child: Column(
                  mainAxisSize: MainAxisSize.min,
                  children: [
                    const Icon(PiccoloIcons.lock, size: 48, color: PiccoloTheme.inkMuted),
                    const SizedBox(height: Spacing.base),
                    Text('Local Access Required',
                        style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
                    const SizedBox(height: Spacing.sm),
                    Text(
                      'WiFi settings can only be managed when you are\nconnected to the same local network as your Piccolo.',
                      textAlign: TextAlign.center,
                      style: PiccoloTheme.textTheme.bodyMedium,
                    ),
                    const SizedBox(height: Spacing.base),
                    FilledButton.tonal(
                      onPressed: _controller.refresh,
                      child: const Text('Check Again'),
                    ),
                  ],
                ),
              ),
            );
          }
          return Center(
            child: Column(
              mainAxisSize: MainAxisSize.min,
              children: [
                Text('Error: ${_controller.error}'),
                const SizedBox(height: 16),
                FilledButton(
                  onPressed: _controller.refresh,
                  child: const Text('Retry'),
                ),
              ],
            ),
          );
        }

        final status = _controller.status;
        if (status == null || !status.available) {
          return const Center(
            child: Text('No WiFi hardware detected on this device.'),
          );
        }

        return SingleChildScrollView(
          padding: const EdgeInsets.all(24),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              WifiStatusCard(
                status: status,
                onForgetNetwork: _controller.disconnectWifi,
                onScanNetworks: () async {
                  await _controller.scanNetworks();
                },
              ),
              const SizedBox(height: 16),
              if (_controller.scanResults != null)
                WifiNetworkList(
                  networks: _controller.scanResults!,
                  isScanning: _controller.isScanning,
                  isConnecting: _controller.isConnecting,
                  onConnect: _controller.connectToNetwork,
                  onRescan: _controller.scanNetworks,
                ),
              if (_controller.apStatus != null) ...[
                const SizedBox(height: 16),
                APModeCard(
                  apStatus: _controller.apStatus!,
                  onToggleSuppression: _controller.toggleAPSuppression,
                ),
              ],
            ],
          ),
        );
      },
    );
  }
}
