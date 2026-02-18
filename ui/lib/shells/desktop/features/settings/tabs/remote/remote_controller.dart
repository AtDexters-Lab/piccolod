import 'dart:async';

import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/remote_models.dart';
import 'package:piccolo_os/core/models/service_endpoint.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/event_stream_client.dart';
import 'package:piccolo_os/core/services/remote_service.dart';

class RemoteController extends ChangeNotifier {

  RemoteController({EventStreamClient? eventStreamClient})
      : _sharedEventStream = eventStreamClient {
    _init();
  }
  final RemoteService _service = RemoteService();
  bool _disposed = false;
  bool _isPolling = false;

  // Event stream for real-time updates (shared or owned)
  final EventStreamClient? _sharedEventStream;
  EventStreamClient? _ownedEventStream;
  StreamSubscription<Map<String, dynamic>>? _remoteConfigSub;
  StreamSubscription<Map<String, dynamic>>? _certificateSub;

  // State
  bool isLoading = true;
  String? error;
  bool isLocked = false;
  RemoteStatus? status;
  List<RemoteEvent> events = [];
  List<RemoteAlias> aliases = [];
  List<RemoteCertificate> certificates = [];
  List<ServiceEndpoint> services = [];

  // Setup Wizard State
  int wizardStep = 0;
  RemoteGuideInfo? guideInfo;
  List<RemotePreflightCheck> preflightChecks = [];
  bool isRunningPreflight = false;
  bool isSubmittingConfig = false;

  // Ephemeral configuration state for the wizard (not yet persisted to backend)
  final Map<String, dynamic> _pendingConfig = {};

  void _init() {
    unawaited(refresh());
    _connectEventStream();
  }

  void _connectEventStream() {
    // Use shared client if provided, otherwise create our own
    EventStreamClient client;
    if (_sharedEventStream != null) {
      client = _sharedEventStream;
    } else {
      _ownedEventStream = EventStreamClient();
      client = _ownedEventStream!
        ..connect();
    }

    // Subscribe to remote config changes
    _remoteConfigSub = client.remoteConfigEvents.listen((_) {
      if (!_disposed) unawaited(_pollStatus());
    });

    // Subscribe to certificate status changes
    _certificateSub = client.certificateEvents.listen((_) {
      if (!_disposed) unawaited(_pollStatus());
    });
  }

  @override
  void dispose() {
    _disposed = true;
    unawaited(_remoteConfigSub?.cancel());
    unawaited(_certificateSub?.cancel());
    // Only dispose the event stream if we own it
    _ownedEventStream?.dispose();
    super.dispose();
  }

  Future<void> refresh() async {
    if (_disposed) return;
    isLoading = true;
    error = null;
    isLocked = false;
    notifyListeners();
    await _pollStatus();
    if (!isLocked) {
      await fetchServices();
      await _fetchEvents();
    }
    if (_disposed) return;
    isLoading = false;
    notifyListeners();
  }

  Future<void> _pollStatus() async {
    if (_disposed || _isPolling) return;
    _isPolling = true;

    try {
      status = await _service.getStatus();
      error = null;
      isLocked = false;
      await _fetchLists();
    } on Object catch (e) {
      if (e is ApiException && e.statusCode == 423) {
        isLocked = true;
        error = null;
        status = null;
      } else {
        error = e.toString();
        isLocked = false;
        status = null;
      }
    } finally {
      _isPolling = false;
    }
    if (_disposed) return;
    notifyListeners();
  }

  Future<void> _fetchLists() async {
    if (_disposed) return;
    try {
      aliases = await _service.getAliases();
      certificates = await _service.getCertificates();
    } on Object catch (e) {
      debugPrint('Failed to fetch lists: $e');
    }
  }

  Future<void> fetchServices() async {
    if (_disposed) return;
    try {
      services = await _service.getServices();
    } on Object catch (e) {
      debugPrint('Failed to fetch services: $e');
    }
  }

  Future<void> _fetchEvents() async {
    if (_disposed) return;
    try {
      events = await _service.getEvents();
    } on Object catch (e) {
      debugPrint('Failed to fetch remote events: $e');
    }
  }

  // --- Setup Wizard ---

  /// Seeds pending config from current status (used when resuming wizard)
  void seedPendingConfigFromStatus() {
    if (status == null) return;
    if (status!.endpoint != null) _pendingConfig['endpoint'] = status!.endpoint;
    if (status!.portalHostname != null) _pendingConfig['portal_hostname'] = status!.portalHostname;
    // Note: device_secret is not returned in status for security reasons
    // When resuming, the backend will use the existing stored secret
  }

  Future<void> loadNexusGuide() async {
    try {
      guideInfo = await _service.getNexusGuide();
      if (_disposed) return;
      notifyListeners();
    } on Object catch (e) {
      if (_disposed) return;
      error = 'Failed to load Nexus guide: $e';
      notifyListeners();
    }
  }

  Future<void> verifyNexusGuide(String endpoint, String portal, String secret) async {
    try {
      // Validate with backend (stateless)
      await _service.verifyNexusGuide({
        'endpoint': endpoint,
        'portal_hostname': portal,
        'jwt_secret': secret,
      });
      if (_disposed) return;

      // Store in memory for subsequent steps
      _pendingConfig['endpoint'] = endpoint;
      _pendingConfig['portal_hostname'] = portal;
      _pendingConfig['device_secret'] = secret;

      wizardStep = 1; // Move to preflight
      notifyListeners();
    } on Object catch (e) {
      if (_disposed) return;
      error = 'Failed to verify guide: $e';
      notifyListeners();
    }
  }

  Future<void> runPreflight() async {
    isRunningPreflight = true;
    notifyListeners();
    try {
      // Pass pending config if we are in the wizard flow (step 1)
      // Otherwise (re-running on active) pass null/empty
      Map<String, dynamic>? configPayload;
      if (_pendingConfig.isNotEmpty && wizardStep > 0) {
        configPayload = Map<String, dynamic>.from(_pendingConfig);
      }

      preflightChecks = await _service.runPreflight(configPayload);
      if (_disposed) return;
    } on Object catch (e) {
      if (_disposed) return;
      error = 'Preflight failed: $e';
    } finally {
      if (!_disposed) {
        isRunningPreflight = false;
        notifyListeners();
      }
    }
  }

  Future<void> submitConfiguration() async {
    isSubmittingConfig = true;
    notifyListeners();
    try {
      // Submit the pending config (endpoint, device_secret, portal_hostname)
      // HTTP-01 solver is implicit on the backend for user-managed mode
      await _service.configure(_pendingConfig);
      if (_disposed) return;

      // Clear pending state on success
      _pendingConfig.clear();

      await refresh();
      if (_disposed) return;
      wizardStep = 0;
    } on Object catch (e) {
      if (_disposed) return;
      error = 'Configuration failed: $e';
    } finally {
      if (!_disposed) {
        isSubmittingConfig = false;
        notifyListeners();
      }
    }
  }

  // --- Management ---

  Future<void> disableRemote() async {
    try {
      await _service.disable();
      if (_disposed) return;
      wizardStep = 0;
      await refresh();
    } on Object catch (e) {
      if (_disposed) return;
      error = 'Failed to disable remote access: $e';
      notifyListeners();
    }
  }

  Future<String?> rotateCredentials() async {
    try {
      final secret = await _service.rotateCredentials();
      if (_disposed) return null;
      return secret;
    } on Object catch (e) {
      if (_disposed) return null;
      error = 'Failed to rotate credentials: $e';
      notifyListeners();
      return null;
    }
  }

  Future<void> renewCertificate(String id) async {
    try {
      await _service.renewCertificate(id);
      if (_disposed) return;
      await refresh();
    } on Object catch (e) {
      if (_disposed) return;
      error = 'Failed to renew certificate: $e';
      notifyListeners();
    }
  }

  Future<void> addAlias(String hostname, String listener) async {
    try {
      await _service.addAlias(hostname, listener);
      if (_disposed) return;
      await refresh();
    } on Object catch (e) {
      if (_disposed) return;
      error = 'Failed to add alias: $e';
      notifyListeners();
    }
  }

  Future<void> deleteAlias(String id) async {
    try {
      await _service.removeAlias(id);
      if (_disposed) return;
      await refresh();
    } on Object catch (e) {
      if (_disposed) return;
      error = 'Failed to delete alias: $e';
      notifyListeners();
    }
  }
}
