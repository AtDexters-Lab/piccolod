import 'dart:async';
import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/remote_models.dart';
import 'package:piccolo_os/core/models/service_endpoint.dart';
import 'package:piccolo_os/core/services/remote_service.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/event_stream_client.dart';

class RemoteController extends ChangeNotifier {
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
  List<RemoteDNSProvider> dnsProviders = [];

  // Setup Wizard State
  int wizardStep = 0;
  RemoteGuideInfo? guideInfo;
  List<RemotePreflightCheck> preflightChecks = [];
  bool isRunningPreflight = false;
  bool isSubmittingConfig = false;

  // Ephemeral configuration state for the wizard (not yet persisted to backend)
  final Map<String, dynamic> _pendingConfig = {};

  RemoteController({EventStreamClient? eventStreamClient})
      : _sharedEventStream = eventStreamClient {
    _init();
  }

  void _init() {
    refresh();
    _connectEventStream();
  }

  void _connectEventStream() {
    // Use shared client if provided, otherwise create our own
    EventStreamClient client;
    if (_sharedEventStream != null) {
      client = _sharedEventStream;
    } else {
      _ownedEventStream = EventStreamClient();
      client = _ownedEventStream!;
      client.connect();
    }

    // Subscribe to remote config changes
    _remoteConfigSub = client.remoteConfigEvents.listen((_) {
      if (!_disposed) _pollStatus();
    });

    // Subscribe to certificate status changes
    _certificateSub = client.certificateEvents.listen((_) {
      if (!_disposed) _pollStatus();
    });
  }

  @override
  void dispose() {
    _disposed = true;
    _remoteConfigSub?.cancel();
    _certificateSub?.cancel();
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
    } catch (e) {
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
    } catch (e) {
      debugPrint("Failed to fetch lists: $e");
    }
  }

  Future<void> fetchServices() async {
    if (_disposed) return;
    try {
      services = await _service.getServices();
    } catch (e) {
      debugPrint("Failed to fetch services: $e");
    }
  }

  Future<void> _fetchEvents() async {
    if (_disposed) return;
    try {
      events = await _service.getEvents();
    } catch (e) {
      debugPrint("Failed to fetch remote events: $e");
    }
  }

  // --- Setup Wizard ---

  Future<void> loadNexusGuide() async {
    try {
      guideInfo = await _service.getNexusGuide();
      if (_disposed) return;
      notifyListeners();
    } catch (e) {
      if (_disposed) return;
      error = "Failed to load Nexus guide: $e";
      notifyListeners();
    }
  }

  Future<void> verifyNexusGuide(String endpoint, String tld, String portal, String secret) async {
    try {
      // Validate with backend (stateless)
      await _service.verifyNexusGuide({
        'endpoint': endpoint,
        'tld': tld,
        'portal_hostname': portal,
        'jwt_secret': secret,
      });
      if (_disposed) return;
      
      // Store in memory for subsequent steps
      _pendingConfig['endpoint'] = endpoint;
      _pendingConfig['tld'] = tld;
      _pendingConfig['portal_hostname'] = portal;
      _pendingConfig['device_secret'] = secret;
      
      wizardStep = 1; // Move to preflight
      notifyListeners();
    } catch (e) {
      if (_disposed) return;
      error = "Failed to verify guide: $e";
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
        configPayload = Map.from(_pendingConfig);
        // Ensure solver is set for validation if not yet selected (default to http-01 for check)
        configPayload.putIfAbsent('solver', () => 'http-01');
      }

      preflightChecks = await _service.runPreflight(configPayload);
      if (_disposed) return;
      bool allPassed = preflightChecks.every((c) => c.status == 'pass' || c.status == 'warn');
      if (allPassed && preflightChecks.isNotEmpty) {
        await fetchDNSProviders();
      }
    } catch (e) {
      if (_disposed) return;
      error = "Preflight failed: $e";
    } finally {
      if (!_disposed) {
        isRunningPreflight = false;
        notifyListeners();
      }
    }
  }

  Future<void> fetchDNSProviders() async {
    try {
      dnsProviders = await _service.getDNSProviders();
      if (_disposed) return;
      notifyListeners();
    } catch (e) {
      debugPrint("Failed to fetch DNS providers: $e");
    }
  }

  Future<void> submitConfiguration(Map<String, dynamic> partialConfig) async {
    isSubmittingConfig = true;
    notifyListeners();
    try {
      // Merge partial config (solver, dns_creds) with pending config (endpoint, secret)
      final finalConfig = Map<String, dynamic>.from(_pendingConfig);
      finalConfig.addAll(partialConfig);
      
      await _service.configure(finalConfig);
      if (_disposed) return;
      
      // Clear pending state on success
      _pendingConfig.clear();
      
      await refresh();
      if (_disposed) return;
      wizardStep = 0;
    } catch (e) {
      if (_disposed) return;
      error = "Configuration failed: $e";
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
    } catch (e) {
      if (_disposed) return;
      error = "Failed to disable remote access: $e";
      notifyListeners();
    }
  }

  Future<String?> rotateCredentials() async {
    try {
      final secret = await _service.rotateCredentials();
      if (_disposed) return null;
      return secret;
    } catch (e) {
      if (_disposed) return null;
      error = "Failed to rotate credentials: $e";
      notifyListeners();
      return null;
    }
  }

  Future<void> renewCertificate(String id) async {
    try {
      await _service.renewCertificate(id);
      if (_disposed) return;
      await refresh();
    } catch (e) {
      if (_disposed) return;
      error = "Failed to renew certificate: $e";
      notifyListeners();
    }
  }

  Future<void> addAlias(String hostname, String listener) async {
    try {
      await _service.addAlias(hostname, listener);
      if (_disposed) return;
      await refresh();
    } catch (e) {
      if (_disposed) return;
      error = "Failed to add alias: $e";
      notifyListeners();
    }
  }

  Future<void> deleteAlias(String id) async {
    try {
      await _service.removeAlias(id);
      if (_disposed) return;
      await refresh();
    } catch (e) {
      if (_disposed) return;
      error = "Failed to delete alias: $e";
      notifyListeners();
    }
  }
}