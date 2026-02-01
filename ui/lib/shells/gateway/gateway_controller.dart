import 'package:flutter/foundation.dart';
import 'package:web/web.dart' as web;
import '../../core/models/network_models.dart';
import '../../core/services/api_client.dart';
import '../../core/services/network_service.dart';

/// Controller for the gateway shell that handles device discovery and navigation.
///
/// The gateway shell is shown when accessing piccolo.local and the device
/// is the gateway leader. It displays a device selector for multi-device LANs
/// or auto-redirects for single-device LANs.
class GatewayController extends ChangeNotifier {
  final NetworkService _networkService;

  List<DiscoveredPeer> _peers = [];
  NetworkSelf? _self;
  bool _isLoading = true;
  String? _error;
  bool _redirecting = false;

  GatewayController({NetworkService? networkService})
      : _networkService = networkService ?? NetworkService(ApiClient());

  List<DiscoveredPeer> get peers => _peers;
  NetworkSelf? get self => _self;
  bool get isLoading => _isLoading;
  String? get error => _error;
  bool get redirecting => _redirecting;

  /// Returns only online peers for display.
  List<DiscoveredPeer> get onlinePeers =>
      _peers.where((p) => p.online).toList();

  /// Returns offline peers (shown but disabled).
  List<DiscoveredPeer> get offlinePeers =>
      _peers.where((p) => !p.online).toList();

  /// Initialize the controller by fetching peers.
  Future<void> initialize() async {
    _isLoading = true;
    _error = null;
    notifyListeners();

    try {
      final response = await _networkService.getPeers();
      _self = response.self;
      _peers = response.peers;

      // Auto-redirect if single device (no peers and we have self info)
      if (_peers.isEmpty && _self != null) {
        _redirectToSelf();
        return;
      }

      _isLoading = false;
      notifyListeners();
    } catch (e) {
      debugPrint('GatewayController.initialize error: $e');
      _error = 'Could not discover devices';
      _isLoading = false;
      notifyListeners();
    }
  }

  /// Redirect to this device's specific hostname.
  void _redirectToSelf() {
    if (_self == null) return;

    // Use specific hostname (piccolo-<machineId>.local) for redirect
    var hostname = _self!.specificHostname;

    // Guard against redirect loop: if specificHostname is empty and
    // hostname is piccolo.local, we'd redirect back to ourselves
    if (hostname.isEmpty) {
      final fallback = _self!.hostname.toLowerCase();
      if (fallback == 'piccolo.local' || fallback == 'piccolo.local.') {
        // Cannot redirect - would cause infinite loop
        _error = 'Device hostname unavailable';
        _isLoading = false;
        notifyListeners();
        return;
      }
      hostname = _self!.hostname;
    }

    _redirecting = true;
    notifyListeners();

    final url = 'http://$hostname';

    // Small delay to show loading state before redirect
    Future.delayed(const Duration(milliseconds: 500), () {
      web.window.location.replace(url);
    });
  }

  /// Navigate to a specific device.
  void navigateToDevice(DiscoveredPeer peer) {
    if (!peer.online) return;
    web.window.location.href = peer.url;
  }

  /// Refresh the peer list.
  Future<void> refresh() async {
    await initialize();
  }
}
