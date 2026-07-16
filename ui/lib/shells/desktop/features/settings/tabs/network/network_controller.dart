import 'dart:async';

import 'package:flutter/foundation.dart';
import 'package:piccolo_os/core/models/wifi_models.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/event_stream_client.dart';
import 'package:piccolo_os/core/services/wifi_service.dart';

class NetworkController extends ChangeNotifier {
  NetworkController({
    required ApiClient apiClient,
    EventStreamClient? eventStreamClient,
  }) : _wifiService = WifiService(apiClient),
       _eventStreamClient = eventStreamClient {
    _init();
  }

  final WifiService _wifiService;
  final EventStreamClient? _eventStreamClient;

  StreamSubscription<Map<String, dynamic>>? _eventSub;
  Timer? _refreshDebounce;
  bool _disposed = false;

  // State
  bool isLoading = true;
  String? error;
  NetworkStatus? status;
  WifiAPStatus? apStatus;
  List<WifiNetwork>? scanResults;
  bool isScanning = false;

  void _init() {
    unawaited(refresh());
    _connectEventStream();
  }

  void _connectEventStream() {
    final client = _eventStreamClient;
    if (client == null) return;
    _eventSub = client.networkStatusEvents.listen((_) {
      _scheduleRefresh();
    });
  }

  void _scheduleRefresh() {
    _refreshDebounce?.cancel();
    _refreshDebounce = Timer(const Duration(milliseconds: 500), () {
      if (!_disposed) refresh();
    });
  }

  Future<void> refresh() async {
    try {
      final [statusResult, apResult] = await Future.wait([
        _wifiService.getStatus(),
        _wifiService.getAPStatus(),
      ]);
      if (_disposed) return;
      status = statusResult as NetworkStatus;
      apStatus = apResult as WifiAPStatus;
      error = null;
    } on ApiException catch (e) {
      if (_disposed) return;
      error = e.statusCode == 403 ? 'remote_access' : e.toString();
    } catch (e) {
      if (_disposed) return;
      error = e.toString();
    } finally {
      isLoading = false;
      if (!_disposed) notifyListeners();
    }
  }

  Future<void> scanNetworks() async {
    isScanning = true;
    notifyListeners();

    try {
      scanResults = await _wifiService.scan();
      error = null;
    } catch (e) {
      error = 'Scan failed: $e';
    } finally {
      isScanning = false;
      if (!_disposed) notifyListeners();
    }
  }

  /// Sends a WiFi connect request. Lets exceptions propagate for
  /// network-reset detection by the caller (WiFi connect dialog).
  Future<WifiConnectResult> connectRaw(String ssid, String passphrase) {
    return _wifiService.connect(ssid, passphrase);
  }

  /// Polls getStatus() to verify the device connected to [targetSsid].
  /// Returns true if connected, false after all retries are exhausted.
  Future<bool> verifyConnection(String targetSsid) async {
    for (var i = 0; i < 5; i++) {
      await Future<void>.delayed(const Duration(seconds: 2));
      if (_disposed) return false;
      try {
        debugPrint('WiFi verify poll ${i + 1}/5: checking for $targetSsid');
        final s = await _wifiService.getStatus();
        if (s.isWifiConnected && s.ssid == targetSsid) {
          debugPrint('WiFi verify: connected to $targetSsid');
          if (!_disposed) await refresh();
          return true;
        }
      } catch (e) {
        debugPrint('WiFi verify poll ${i + 1}/5 failed: $e');
      }
    }
    debugPrint('WiFi verify: gave up after 5 polls');
    return false;
  }

  Future<void> disconnectWifi() async {
    try {
      await _wifiService.disconnect();
      await refresh();
    } catch (e) {
      error = 'Disconnect failed: $e';
      notifyListeners();
    }
  }

  @override
  void dispose() {
    _disposed = true;
    _eventSub?.cancel();
    _refreshDebounce?.cancel();
    super.dispose();
  }
}
