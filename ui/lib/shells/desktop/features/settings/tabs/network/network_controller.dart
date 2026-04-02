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
  })  : _wifiService = WifiService(apiClient),
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
  WifiStatus? status;
  WifiAPStatus? apStatus;
  List<WifiNetwork>? scanResults;
  bool isScanning = false;
  bool isConnecting = false;

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
      status = statusResult as WifiStatus;
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

  Future<String?> connectToNetwork(String ssid, String passphrase) async {
    isConnecting = true;
    notifyListeners();

    try {
      final result = await _wifiService.connect(ssid, passphrase);
      if (!result.success) {
        return result.error ?? 'Connection failed';
      }
      // Refresh status after successful connection
      await refresh();
      return null; // success
    } catch (e) {
      return e.toString();
    } finally {
      isConnecting = false;
      if (!_disposed) notifyListeners();
    }
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

  Future<void> toggleAPSuppression(bool suppress) async {
    try {
      await _wifiService.suppressAP(suppress);
      await refresh();
    } catch (e) {
      error = 'Failed to update AP setting: $e';
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
