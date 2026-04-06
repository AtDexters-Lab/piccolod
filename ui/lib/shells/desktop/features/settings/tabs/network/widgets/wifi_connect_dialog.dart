import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/wifi_models.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Dialog for connecting to a WiFi network.
///
/// Handles password entry, open networks, connection state, and
/// network-reset retry logic (the device's IP changes when switching WiFi,
/// which kills the HTTP connection — this is expected and handled gracefully).
class WifiConnectDialog extends StatefulWidget {
  const WifiConnectDialog({
    required this.network,
    required this.onConnect,
    required this.onVerify,
    this.isAPMode = false,
    super.key,
  });

  final WifiNetwork network;

  /// Sends the connect request. Throws on network-level errors (expected
  /// when the device switches networks). Returns the backend result on
  /// successful HTTP round-trip.
  final Future<WifiConnectResult> Function(String ssid, String passphrase) onConnect;

  /// Polls the device to check if it connected to the target SSID.
  final Future<bool> Function(String targetSsid) onVerify;

  /// Whether the device is currently in AP mode. Changes guidance text.
  final bool isAPMode;

  @override
  State<WifiConnectDialog> createState() => _WifiConnectDialogState();
}

enum _DialogPhase { input, connecting, reconnecting, success, error }

class _WifiConnectDialogState extends State<WifiConnectDialog> {
  final _passphraseController = TextEditingController();
  _DialogPhase _phase = _DialogPhase.input;
  String? _errorMessage;

  @override
  void dispose() {
    _passphraseController.dispose();
    super.dispose();
  }

  bool get _canConnect => widget.network.isOpen || _passphraseController.text.isNotEmpty;

  Future<void> _doConnect() async {
    if (!_canConnect) return;

    setState(() {
      _phase = _DialogPhase.connecting;
      _errorMessage = null;
    });

    final ssid = widget.network.ssid;
    final passphrase = widget.network.isOpen ? '' : _passphraseController.text;

    try {
      final result = await widget.onConnect(ssid, passphrase);
      if (!mounted) return;

      if (result.success) {
        setState(() => _phase = _DialogPhase.success);
        await Future<void>.delayed(const Duration(milliseconds: 600));
        if (mounted) Navigator.of(context).pop(true);
      } else {
        setState(() {
          _phase = _DialogPhase.error;
          _errorMessage = result.error ?? 'Connection failed';
        });
      }
    } on Object catch (e) {
      // Network-level error — the device is switching networks.
      if (!mounted) return;
      debugPrint('WiFi connect: network error (expected): $e');

      setState(() => _phase = _DialogPhase.reconnecting);

      await Future<void>.delayed(const Duration(seconds: 3));
      if (!mounted) return;

      final connected = await widget.onVerify(ssid);
      if (!mounted) return;

      if (connected) {
        setState(() => _phase = _DialogPhase.success);
        await Future<void>.delayed(const Duration(milliseconds: 600));
        if (mounted) Navigator.of(context).pop(true);
      } else {
        setState(() {
          _phase = _DialogPhase.error;
          _errorMessage = 'Connection may have failed. Please check your WiFi settings.';
        });
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    final canDismiss = _phase != _DialogPhase.connecting && _phase != _DialogPhase.reconnecting;

    return PopScope(
      canPop: canDismiss,
      child: AlertDialog(
        title: Row(
          children: [
            Icon(widget.network.signalIcon, size: 20, color: widget.network.signalColor),
            const SizedBox(width: 10),
            Expanded(
              child: Text(widget.network.ssid, style: PiccoloTheme.textTheme.titleMedium),
            ),
          ],
        ),
        content: SizedBox(
          width: 360,
          child: _buildContent(),
        ),
        actions: _buildActions(),
      ),
    );
  }

  Widget _buildContent() {
    switch (_phase) {
      case _DialogPhase.input:
        return Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(
              '${widget.network.security.toUpperCase()} · ${widget.network.band} · ${widget.network.signalDbm} dBm',
              style: PiccoloTheme.textTheme.bodySmall,
            ),
            const SizedBox(height: 16),
            if (!widget.network.isOpen) ...[
              TextField(
                controller: _passphraseController,
                autofocus: true,
                obscureText: true,
                decoration: const InputDecoration(
                  labelText: 'Password',
                ),
                onSubmitted: (_) => _doConnect(),
              ),
              const SizedBox(height: 16),
            ],
            Container(
              padding: const EdgeInsets.all(10),
              decoration: BoxDecoration(
                color: PiccoloTheme.mist,
                borderRadius: BorderRadius.circular(Radii.xs),
              ),
              child: Row(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  const Icon(PiccoloIcons.info, size: 16, color: PiccoloTheme.inkMuted),
                  const SizedBox(width: 8),
                  Expanded(
                    child: Text(
                      widget.isAPMode
                          ? 'Connecting will shut down the setup hotspot. After connecting, find your Piccolo on your home network.'
                          : "This will switch your Piccolo's WiFi network. Your browser may briefly disconnect.",
                      style: const TextStyle(fontSize: 12, color: PiccoloTheme.inkMuted),
                    ),
                  ),
                ],
              ),
            ),
          ],
        );

      case _DialogPhase.connecting:
        return const Padding(
          padding: EdgeInsets.symmetric(vertical: 16),
          child: Row(
            children: [
              SizedBox(
                width: 20,
                height: 20,
                child: CircularProgressIndicator(strokeWidth: 2, color: PiccoloTheme.cobalt600),
              ),
              SizedBox(width: 12),
              Text('Connecting...'),
            ],
          ),
        );

      case _DialogPhase.reconnecting:
        return Padding(
          padding: const EdgeInsets.symmetric(vertical: 16),
          child: Column(
            mainAxisSize: MainAxisSize.min,
            children: [
              const Text('Network is switching...'),
              const SizedBox(height: 4),
              Text(
                'Reconnecting to your Piccolo.',
                style: PiccoloTheme.textTheme.bodySmall,
              ),
              const SizedBox(height: 16),
              const LinearProgressIndicator(color: PiccoloTheme.cobalt600),
            ],
          ),
        );

      case _DialogPhase.success:
        return const Padding(
          padding: EdgeInsets.symmetric(vertical: 16),
          child: Row(
            children: [
              Icon(PiccoloIcons.success, color: PiccoloTheme.success, size: 20),
              SizedBox(width: 12),
              Text('Connected'),
            ],
          ),
        );

      case _DialogPhase.error:
        return Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            if (_errorMessage != null)
              Text(_errorMessage!, style: const TextStyle(color: PiccoloTheme.critical, fontSize: 13)),
            const SizedBox(height: 12),
            if (!widget.network.isOpen)
              TextField(
                controller: _passphraseController,
                autofocus: true,
                obscureText: true,
                decoration: const InputDecoration(
                  labelText: 'Password',
                ),
                onSubmitted: (_) => _doConnect(),
              ),
          ],
        );
    }
  }

  List<Widget>? _buildActions() {
    switch (_phase) {
      case _DialogPhase.input || _DialogPhase.error:
        return [
          TextButton(
            onPressed: () => Navigator.of(context).pop(false),
            child: const Text('Cancel'),
          ),
          ListenableBuilder(
            listenable: _passphraseController,
            builder: (context, _) => FilledButton(
              onPressed: _canConnect ? _doConnect : null,
              child: Text(_phase == _DialogPhase.error ? 'Retry' : 'Connect'),
            ),
          ),
        ];

      case _DialogPhase.connecting:
      case _DialogPhase.reconnecting:
      case _DialogPhase.success:
        return null;
    }
  }
}
