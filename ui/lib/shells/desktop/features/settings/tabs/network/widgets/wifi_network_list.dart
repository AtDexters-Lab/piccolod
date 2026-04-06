import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/wifi_models.dart';

class WifiNetworkList extends StatefulWidget {
  const WifiNetworkList({
    required this.networks, required this.isScanning, required this.isConnecting, required this.onConnect, required this.onRescan, super.key,
  });

  final List<WifiNetwork> networks;
  final bool isScanning;
  final bool isConnecting;
  final Future<String?> Function(String ssid, String passphrase) onConnect;
  final VoidCallback onRescan;

  @override
  State<WifiNetworkList> createState() => _WifiNetworkListState();
}

class _WifiNetworkListState extends State<WifiNetworkList> {
  String? _selectedSSID;
  final _passphraseController = TextEditingController();
  String? _connectError;

  @override
  void dispose() {
    _passphraseController.dispose();
    super.dispose();
  }

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
                    style: Theme.of(context).textTheme.titleMedium),
                const Spacer(),
                if (widget.isScanning)
                  const SizedBox(
                    width: 16,
                    height: 16,
                    child: CircularProgressIndicator(strokeWidth: 2),
                  )
                else
                  IconButton(
                    icon: const Icon(Icons.refresh, size: 20),
                    onPressed: widget.onRescan,
                    tooltip: 'Rescan',
                  ),
              ],
            ),
            const SizedBox(height: 12),
            if (widget.networks.isEmpty)
              const Padding(
                padding: EdgeInsets.symmetric(vertical: 16),
                child: Text('No networks found. Make sure your router is powered on.'),
              )
            else
              ...widget.networks.map((network) => _NetworkTile(
                    network: network,
                    isSelected: _selectedSSID == network.ssid,
                    onTap: () => setState(() {
                      _selectedSSID = network.ssid;
                      _passphraseController.clear();
                      _connectError = null;
                    }),
                  )),
            if (_selectedSSID != null) ...[
              const SizedBox(height: 16),
              if (_connectError != null)
                Padding(
                  padding: const EdgeInsets.only(bottom: 8),
                  child: Text(
                    _connectError!,
                    style: TextStyle(color: Theme.of(context).colorScheme.error, fontSize: 13),
                  ),
                ),
              Row(
                children: [
                  Expanded(
                    child: TextField(
                      controller: _passphraseController,
                      obscureText: true,
                      decoration: const InputDecoration(
                        labelText: 'Password',
                        border: OutlineInputBorder(),
                        isDense: true,
                      ),
                      onSubmitted: (_) => _doConnect(),
                    ),
                  ),
                  const SizedBox(width: 8),
                  FilledButton(
                    onPressed: widget.isConnecting ? null : _doConnect,
                    child: widget.isConnecting
                        ? const SizedBox(
                            width: 16,
                            height: 16,
                            child: CircularProgressIndicator(strokeWidth: 2, color: Colors.white),
                          )
                        : const Text('Connect'),
                  ),
                ],
              ),
            ],
          ],
        ),
      ),
    );
  }

  Future<void> _doConnect() async {
    if (_selectedSSID == null || _passphraseController.text.isEmpty) return;
    final error = await widget.onConnect(_selectedSSID!, _passphraseController.text);
    if (mounted) {
      setState(() {
        _connectError = error;
        if (error == null) {
          _selectedSSID = null;
          _passphraseController.clear();
        }
      });
    }
  }
}

class _NetworkTile extends StatelessWidget {
  const _NetworkTile({
    required this.network,
    required this.isSelected,
    required this.onTap,
  });

  final WifiNetwork network;
  final bool isSelected;
  final VoidCallback onTap;

  @override
  Widget build(BuildContext context) {
    return ListTile(
      dense: true,
      selected: isSelected,
      selectedTileColor: Theme.of(context).colorScheme.primaryContainer.withValues(alpha: 0.3),
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(8)),
      leading: Icon(
        Icons.wifi,
        color: _signalColor,
        size: 20,
      ),
      title: Text(network.ssid, style: const TextStyle(fontSize: 14)),
      subtitle: Text(
        '${network.security.toUpperCase()} · ${network.band}',
        style: TextStyle(fontSize: 12, color: Colors.grey[600]),
      ),
      trailing: Text(
        '${network.signalDbm} dBm',
        style: TextStyle(fontSize: 12, color: Colors.grey[500]),
      ),
      onTap: onTap,
    );
  }

  Color get _signalColor {
    return switch (network.signalTier) {
      'good' || 'fair' => Colors.green,
      'weak' => Colors.orange,
      _ => Colors.red,
    };
  }
}
