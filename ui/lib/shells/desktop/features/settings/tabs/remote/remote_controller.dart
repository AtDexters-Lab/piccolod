import 'dart:async';
import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/remote_models.dart';
import 'package:piccolo_os/core/models/service_endpoint.dart';
import 'package:piccolo_os/core/services/remote_service.dart';
import 'package:piccolo_os/core/services/api_client.dart';

class RemoteController extends ChangeNotifier {
  final RemoteService _service = RemoteService();
  Timer? _pollTimer;
  bool _disposed = false; // [P1] Guard
  bool _isPolling = false; // [P2] Guard

  // State
  bool isLoading = true;
  String? error;
  bool isLocked = false; // [P1] Added
  RemoteStatus? status;
  List<RemoteEvent> events = [];
  List<RemoteAlias> aliases = []; // [P1] Added
  List<RemoteCertificate> certificates = []; // [P1] Added
  List<ServiceEndpoint> services = []; // [P2] Added
  List<RemoteDNSProvider> dnsProviders = [];

  // Setup Wizard State
  int wizardStep = 0;
  RemoteGuideInfo? guideInfo;
  List<RemotePreflightCheck> preflightChecks = [];
  bool isRunningPreflight = false;
  bool isSubmittingConfig = false;

  RemoteController() {
    _init();
  }

  void _init() {
    refresh();
    // Poll every 5 seconds for status updates (as per spec)
    _pollTimer = Timer.periodic(const Duration(seconds: 5), (_) => _pollStatus());
  }

  @override
  void dispose() {
    _disposed = true; // [P1] Guard
    _pollTimer?.cancel();
    super.dispose();
  }

  Future<void> refresh() async {
    if (_disposed) return;
    isLoading = true;
    error = null;
    isLocked = false;
    notifyListeners();
    await _pollStatus();
    // [P1] Fetch full lists
    if (!isLocked) {
      await fetchServices(); // [P3] Fetch static data once per refresh
      await _fetchEvents();
    }
    if (_disposed) return;
    isLoading = false;
    notifyListeners();
  }

  Future<void> _pollStatus() async {
    if (_disposed || _isPolling) return; // [P2] Guard
    _isPolling = true;
    
    try {
      status = await _service.getStatus();
      error = null;
      isLocked = false;
      // We also need to refresh lists occasionally or when status changes, but let's do it on poll for simplicity or just on manual refresh?
      // Spec says status drives UI. But lists are separate.
      // Let's poll lists too if active, but maybe less frequently?
      // For now, let's keep it simple: poll status, but maybe only fetch lists on load/refresh/action.
      // Actually, if certificates renew automatically, we want to see it.
      // I'll add _fetchLists() to _pollStatus() but wrapped in try/catch to not block status.
      await _fetchLists(); // [P1] Added await to ensure sequencing, though we ignore error inside
    } catch (e) {
      if (e is ApiException && e.statusCode == 423) {
        isLocked = true;
        error = null;
        status = null;
      } else {
        error = e.toString();
        isLocked = false;
        status = null; // [P2] Clear stale status
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
      // Services removed from poll loop [P3]
    } catch (e) {
      print("Failed to fetch lists: $e");
    }
  }

  Future<void> fetchServices() async {
    if (_disposed) return;
    try {
      services = await _service.getServices(); // [P3] Dedicated fetch
    } catch (e) {
      print("Failed to fetch services: $e");
    }
  }

  Future<void> _fetchEvents() async {
    if (_disposed) return;
    try {
      events = await _service.getEvents();
    } catch (e) {
      print("Failed to fetch remote events: $e");
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
      await _service.verifyNexusGuide({
        'endpoint': endpoint,
        'tld': tld,
        'portal_hostname': portal,
        'jwt_secret': secret,
      });
      if (_disposed) return;
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
      preflightChecks = await _service.runPreflight();
      if (_disposed) return;
      // Check if all passed
      bool allPassed = preflightChecks.every((c) => c.status == 'pass' || c.status == 'warn');
      if (allPassed && preflightChecks.isNotEmpty) {
        // Automatically fetch DNS providers if we might need them
        await fetchDNSProviders();
        // Wait a moment for user to see success, then could advance
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
      print("Failed to fetch DNS providers: $e");
    }
  }

  Future<void> submitConfiguration(Map<String, dynamic> config) async {
    isSubmittingConfig = true;
    notifyListeners();
    try {
      await _service.configure(config);
      if (_disposed) return;
      await refresh(); // Should switch state to 'active' or 'provisioning'
      if (_disposed) return;
      wizardStep = 0; // Reset wizard for next time
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
      wizardStep = 0; // [P2] Reset wizard step
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
