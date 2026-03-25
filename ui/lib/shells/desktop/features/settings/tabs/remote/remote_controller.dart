import 'dart:async';

import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/identity_models.dart';
import 'package:piccolo_os/core/models/remote_models.dart';
import 'package:piccolo_os/core/models/service_endpoint.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/event_stream_client.dart';
import 'package:piccolo_os/core/services/identity_service.dart';
import 'package:piccolo_os/core/services/remote_service.dart';

class RemoteController extends ChangeNotifier {

  RemoteController({EventStreamClient? eventStreamClient})
      : _sharedEventStream = eventStreamClient {
    _init();
  }
  final RemoteService _remoteService = RemoteService();
  final IdentityService _identityService = IdentityService();
  bool _disposed = false;
  bool _isPolling = false;

  // Event stream for real-time updates (shared or owned)
  final EventStreamClient? _sharedEventStream;
  EventStreamClient? _ownedEventStream;
  StreamSubscription<Map<String, dynamic>>? _remoteConfigSub;
  StreamSubscription<Map<String, dynamic>>? _certificateSub;
  StreamSubscription<Map<String, dynamic>>? _identitySub;

  // Debounced refresh
  bool _refreshScheduled = false;

  // State
  bool isLoading = true;
  String? error;
  bool isLocked = false;
  RemoteStatus? status;
  IdentityStatus? identityStatus;
  List<Portal> portals = [];
  List<RemoteEvent> events = [];
  List<RemoteAlias> aliases = [];
  List<RemoteCertificate> certificates = [];
  List<ServiceEndpoint> services = [];

  // Computed properties
  bool get hasAnyRemoteActive => portals.any((p) => p.state == 'active');
  bool get isNamekEnrolled => identityStatus?.enrolled ?? false;
  bool get isSelfHostedActive {
    final st = status;
    return st != null && st.enabled && st.portalHostname != null && st.portalHostname!.isNotEmpty;
  }

  void _init() {
    unawaited(refresh());
    _connectEventStream();
  }

  void _connectEventStream() {
    EventStreamClient client;
    if (_sharedEventStream != null) {
      client = _sharedEventStream;
    } else {
      _ownedEventStream = EventStreamClient();
      client = _ownedEventStream!
        ..connect();
    }

    _remoteConfigSub = client.remoteConfigEvents.listen((_) {
      _scheduleRefresh();
    });

    _certificateSub = client.certificateEvents.listen((_) {
      _scheduleRefresh();
    });

    _identitySub = client.identityEvents.listen((_) {
      _scheduleRefresh();
    });
  }

  void _scheduleRefresh() {
    if (_refreshScheduled || _disposed) return;
    _refreshScheduled = true;
    unawaited(Future.microtask(() async {
      _refreshScheduled = false;
      if (!_disposed) await _pollStatus();
    }));
  }

  @override
  void dispose() {
    _disposed = true;
    unawaited(_remoteConfigSub?.cancel());
    unawaited(_certificateSub?.cancel());
    unawaited(_identitySub?.cancel());
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
      await Future.wait([fetchServices(), _fetchEvents()]);
    }
    if (_disposed) return;
    isLoading = false;
    notifyListeners();
  }

  Future<void> _pollStatus() async {
    if (_disposed || _isPolling) return;
    _isPolling = true;

    try {
      // Fetch remote status and identity status in parallel
      late final RemoteStatus remoteResult;
      await Future.wait([
        _remoteService.getStatus().then((v) => remoteResult = v),
        _fetchIdentityStatus(),
      ]);
      status = remoteResult;
      error = null;
      isLocked = false;
      await _fetchLists();
      _computePortals();
    } on Object catch (e) {
      if (e is ApiException && e.statusCode == 423) {
        isLocked = true;
        error = null;
        status = null;
        portals = [];
      } else {
        error = e.toString();
        isLocked = false;
        status = null;
        portals = [];
      }
    } finally {
      _isPolling = false;
    }
    if (_disposed) return;
    notifyListeners();
  }

  Future<IdentityStatus?> _fetchIdentityStatus() async {
    try {
      final ids = await _identityService.getStatus();
      identityStatus = ids;
      return ids;
    } on Object catch (e) {
      // 503 = identity service unavailable (no TPM)
      if (e is ApiException && e.statusCode == 503) {
        identityStatus = null;
        return null;
      }
      debugPrint('Failed to fetch identity status: $e');
      return identityStatus; // keep previous value
    }
  }

  void _computePortals() {
    final result = <Portal>[];

    // Namek portals — both custom and slug hostnames when both are active.
    final ids = identityStatus;
    if (ids != null && ids.state == 'active' && ids.nexusEndpoints.isNotEmpty) {
      final customFqdn = ids.resolvedHostname;
      final slug = ids.hostname;
      final hasCustom = customFqdn != null && customFqdn != slug;

      // Custom hostname portal (listed first when set)
      if (hasCustom) {
        result.add(Portal(
          source: PortalSource.namek,
          hostname: customFqdn,
          state: _namekCertState(isCustom: true),
          endpoints: ids.nexusEndpoints,
        ));
      }
      // Slug hostname portal (always present)
      if (slug != null && slug.isNotEmpty) {
        result.add(Portal(
          source: PortalSource.namek,
          hostname: slug,
          state: _namekCertState(isCustom: false),
          endpoints: ids.nexusEndpoints,
        ));
      }
    }

    // Self-hosted portal — preflight_required mapped to active for legacy configs.
    final st = status;
    if (st != null && st.enabled && st.portalHostname != null && st.portalHostname!.isNotEmpty) {
      final portalState = st.state == 'preflight_required' ? 'active' : st.state;
      result.add(Portal(
        source: PortalSource.selfHosted,
        hostname: st.portalHostname!,
        state: portalState,
        endpoints: st.endpoint != null ? [st.endpoint!] : [],
      ));
    }

    portals = result;
  }

  /// Derives managed portal state from namek certificate health.
  String _namekCertState({required bool isCustom}) {
    final certs = status?.certificates;
    if (certs == null || certs.isEmpty) return 'active';
    final prefix = isCustom ? 'namek-custom-' : 'namek-';
    final relevant = certs.where((c) =>
        c.id.startsWith(prefix) &&
        (isCustom || !c.id.startsWith('namek-custom-')));
    final hasError = relevant.any((c) => c.status == 'error');
    final now = DateTime.now();
    final hasRetry = relevant.any((c) => c.status == 'error' && c.retryAt != null && c.retryAt!.isAfter(now));
    if (hasRetry) return 'warning';
    if (hasError) return 'error';
    return 'active';
  }

  Future<void> _fetchLists() async {
    if (_disposed) return;
    try {
      await Future.wait([
        _remoteService.getAliases().then((v) => aliases = v),
        _remoteService.getCertificates().then((v) => certificates = v),
      ]);
    } on Object catch (e) {
      debugPrint('Failed to fetch lists: $e');
    }
  }

  Future<void> fetchServices() async {
    if (_disposed) return;
    try {
      services = await _remoteService.getServices();
    } on Object catch (e) {
      debugPrint('Failed to fetch services: $e');
    }
  }

  Future<void> _fetchEvents() async {
    if (_disposed) return;
    try {
      events = await _remoteService.getEvents();
    } on Object catch (e) {
      debugPrint('Failed to fetch remote events: $e');
    }
  }

  // --- Self-hosted ---

  /// Loads the Nexus relay setup guide info.
  Future<RemoteGuideInfo> fetchGuideInfo() => _remoteService.getNexusGuide();

  /// Verifies and configures a self-hosted relay in one step.
  /// Returns null on success, or an error message on failure.
  Future<String?> connectSelfHosted(String endpoint, String portal, String secret) async {
    try {
      await _remoteService.verifyNexusGuide({
        'endpoint': endpoint,
        'portal_hostname': portal,
        'jwt_secret': secret,
      });
    } on Object catch (e) {
      return 'Verification failed: $e';
    }
    if (_disposed) return null;

    try {
      await _remoteService.configure({
        'endpoint': endpoint,
        'portal_hostname': portal,
        'device_secret': secret,
      });
    } on Object catch (e) {
      return 'Configuration failed: $e';
    }
    if (_disposed) return null;

    try {
      await refresh();
    } on Object catch (_) {
      // refresh() handles errors internally; swallow if it leaks.
    }
    return null;
  }

  Future<void> disableRemote() async {
    try {
      await _remoteService.disable();
      if (_disposed) return;
      await refresh();
    } on Object catch (e) {
      if (_disposed) return;
      error = 'Failed to disable remote access: $e';
      notifyListeners();
    }
  }

  Future<String?> rotateCredentials() async {
    try {
      final secret = await _remoteService.rotateCredentials();
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
      await _remoteService.renewCertificate(id);
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
      await _remoteService.addAlias(hostname, listener);
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
      await _remoteService.removeAlias(id);
      if (_disposed) return;
      await refresh();
    } on Object catch (e) {
      if (_disposed) return;
      error = 'Failed to delete alias: $e';
      notifyListeners();
    }
  }

  // --- Namek Management ---

  Future<void> enrollNamek() async {
    await _runIdentityAction('enroll', _identityService.enroll);
  }

  Future<void> enableNamek() async {
    await _runIdentityAction('enable namek', _identityService.enable);
  }

  Future<void> disableNamek() async {
    await _runIdentityAction('disable namek', _identityService.disable);
  }

  Future<void> setNamekHostname(String hostname) async {
    await _runIdentityAction('set hostname', () => _identityService.setHostname(hostname));
  }

  Future<void> setNamekUrl(String url) async {
    await _runIdentityAction('set namek URL', () => _identityService.setNamekUrl(url));
  }

  Future<void> _runIdentityAction(String label, Future<void> Function() action) async {
    try {
      await action();
      if (_disposed) return;
      await refresh();
    } on Object catch (e) {
      if (_disposed) return;
      error = 'Failed to $label: $e';
      notifyListeners();
    }
  }
}
