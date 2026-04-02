import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/network_models.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/network_service.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'package:url_launcher/url_launcher.dart';

class OtherDevicesPanel extends StatefulWidget {
  const OtherDevicesPanel({super.key});

  @override
  State<OtherDevicesPanel> createState() => _OtherDevicesPanelState();
}

class _OtherDevicesPanelState extends State<OtherDevicesPanel> {
  final NetworkService _networkService = NetworkService(ApiClient());
  List<DiscoveredPeer> _peers = [];
  bool _isLoading = false;
  DateTime? _lastFetch;

  @override
  void initState() {
    super.initState();
    unawaited(_fetchPeers());
  }

  Future<void> _fetchPeers() async {
    if (_isLoading) return;
    setState(() => _isLoading = true);
    try {
      final response = await _networkService.getPeers();
      if (mounted) {
        setState(() {
          _peers = response.peers;
          _isLoading = false;
          _lastFetch = DateTime.now();
        });
      }
    } on Object catch (e) {
      if (mounted) setState(() => _isLoading = false);
      debugPrint('Peer discovery error: $e');
    }
  }

  void _onExpansionChanged(bool expanded) {
    if (!expanded) return;
    if (_lastFetch != null &&
        DateTime.now().difference(_lastFetch!).inSeconds > 30) {
      unawaited(_fetchPeers());
    }
  }

  Future<void> _openPeerUrl(String url) async {
    final uri = Uri.parse(url);
    if (await canLaunchUrl(uri)) {
      await launchUrl(uri, mode: LaunchMode.externalApplication);
    }
  }

  @override
  Widget build(BuildContext context) {
    if (_peers.isEmpty) return const SizedBox.shrink();

    return Padding(
      padding: const EdgeInsets.fromLTRB(16, 8, 16, 16),
      child: ExpansionTile(
        tilePadding: const EdgeInsets.symmetric(horizontal: 16),
        childrenPadding: const EdgeInsets.fromLTRB(16, 0, 16, 16),
        shape: RoundedRectangleBorder(
          borderRadius: BorderRadius.circular(Radii.md),
          side: const BorderSide(color: PiccoloTheme.hairline),
        ),
        collapsedShape: RoundedRectangleBorder(
          borderRadius: BorderRadius.circular(Radii.md),
          side: const BorderSide(color: PiccoloTheme.hairline),
        ),
        backgroundColor: PiccoloTheme.mist.withValues(alpha: 0.5),
        collapsedBackgroundColor: PiccoloTheme.mist.withValues(alpha: 0.3),
        onExpansionChanged: _onExpansionChanged,
        title: Row(
          children: [
            const Icon(PiccoloIcons.devices, size: 18, color: PiccoloTheme.inkMuted),
            const SizedBox(width: 8),
            const Text(
              'Other Devices on Network',
              style: TextStyle(
                fontSize: 13,
                color: PiccoloTheme.inkMuted,
                fontWeight: FontWeight.w500,
              ),
            ),
            const SizedBox(width: 8),
            Container(
              padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 2),
              decoration: BoxDecoration(
                color: PiccoloTheme.cobalt600.withValues(alpha: 0.1),
                borderRadius: BorderRadius.circular(Radii.sm),
              ),
              child: Text(
                '${_peers.length}',
                style: const TextStyle(
                  fontSize: 11,
                  color: PiccoloTheme.cobalt600,
                  fontWeight: FontWeight.w600,
                ),
              ),
            ),
          ],
        ),
        children: _peers.map(_buildPeerTile).toList(),
      ),
    );
  }

  Widget _buildPeerTile(DiscoveredPeer peer) {
    return InkWell(
      onTap: peer.online ? () => _openPeerUrl(peer.url) : null,
      borderRadius: BorderRadius.circular(Radii.sm),
      child: Padding(
        padding: const EdgeInsets.symmetric(vertical: 10, horizontal: 8),
        child: Row(
          children: [
            Container(
              width: 8,
              height: 8,
              decoration: BoxDecoration(
                color: peer.online ? PiccoloTheme.success : PiccoloTheme.inkMuted,
                shape: BoxShape.circle,
              ),
            ),
            const SizedBox(width: 12),
            Expanded(
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Text(
                    peer.displayName,
                    style: const TextStyle(fontWeight: FontWeight.w500, fontSize: 14),
                  ),
                  Text(
                    peer.online
                        ? [peer.model, peer.ipv4].where((s) => s != null).join(' \u2022 ')
                        : '(offline)',
                    style: const TextStyle(fontSize: 12, color: PiccoloTheme.inkMuted),
                  ),
                ],
              ),
            ),
            if (peer.online)
              const Icon(PiccoloIcons.arrowForwardIos, size: 14, color: PiccoloTheme.inkMuted),
          ],
        ),
      ),
    );
  }
}
