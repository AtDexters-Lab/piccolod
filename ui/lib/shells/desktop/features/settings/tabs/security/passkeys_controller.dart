import 'dart:async';

import 'package:flutter/foundation.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/webauthn_service.dart';
import 'package:piccolo_os/shells/desktop/features/setup/setup_utils.dart';

/// A registered passkey returned by the API.
class Passkey {
  Passkey({
    required this.id,
    required this.friendlyName,
    required this.rpId,
    required this.createdAt,
    this.lastUsedAt,
  });

  factory Passkey.fromJson(Map<String, dynamic> json) {
    return Passkey(
      id: (json['id'] as String?) ?? '',
      friendlyName: ((json['friendly_name'] as String?) ?? '').isEmpty
          ? 'Passkey'
          : json['friendly_name'] as String,
      rpId: (json['rp_id'] as String?) ?? '',
      createdAt: json['created_at'] != null
          ? DateTime.parse(json['created_at'] as String)
          : DateTime.now(),
      lastUsedAt: json['last_used_at'] != null
          ? DateTime.parse(json['last_used_at'] as String)
          : null,
    );
  }

  final String id;
  final String friendlyName;
  final String rpId;
  final DateTime createdAt;
  final DateTime? lastUsedAt;

  /// Human-readable RP context label.
  String get rpContext {
    if (rpId.isEmpty) return 'LAN';
    if (rpId.contains('.') && !rpId.endsWith('.local')) return 'Remote';
    return 'LAN';
  }
}

/// Result of a delete operation, surfacing the post-signal state to the UI
/// so the SnackBar can render the right copy variant (E6).
enum SignalDeliveryState {
  /// Signal API not present — we removed it server-side; user's password
  /// manager may keep it visible until next sync.
  unsupported,
  /// Signal API present and the call returned successfully.
  delivered,
  /// Signal API present but the call returned false / threw.
  failed,
}

/// Controller for managing passkeys in the Security settings tab.
class PasskeysController extends ChangeNotifier {
  PasskeysController({this.api});

  final ApiClient? api;
  ApiClient get _api => api ?? ApiClient();

  bool _disposed = false;

  /// Buffer of credential IDs the user recently deleted on this device. Used
  /// (D-13) to re-fire signalUnknownCredential at list-load time when a fire-
  /// and-forget signal may have been dropped (backgrounded tab, iOS Safari
  /// throttle). Entries expire after 5 minutes — recovery is bounded, not
  /// permanent.
  final Map<String, _RecentlyDeleted> _recentlyDeleted = {};
  static const Duration _recentlyDeletedTtl = Duration(minutes: 5);

  @override
  void dispose() {
    _disposed = true;
    super.dispose();
  }

  void _safeNotify() {
    if (!_disposed) notifyListeners();
  }

  // State
  bool _isLoading = false;
  bool get isLoading => _isLoading;

  String? _error;
  String? get error => _error;

  List<Passkey> _passkeys = [];
  List<Passkey> get passkeys => _passkeys;

  /// Load all passkeys from the API.
  Future<void> loadPasskeys() async {
    if (_disposed) return;
    _isLoading = true;
    _error = null;
    _safeNotify();

    try {
      final list = await _api.listPasskeys();
      if (_disposed) return;
      _passkeys = list
          .map((json) => Passkey.fromJson(json as Map<String, dynamic>))
          .toList();
      _error = null;
      _reconcileRecentlyDeleted();
    } on Object catch (e) {
      if (_disposed) return;
      _error = e.toString();
    } finally {
      if (!_disposed) {
        _isLoading = false;
        _safeNotify();
      }
    }
  }

  /// D-13: for any credential the user recently deleted that is genuinely
  /// absent from the latest server list, re-fire signalUnknownCredential.
  /// signalUnknownCredential is idempotent — safe to call multiple times.
  /// Scope is strictly "recently deleted on this device": never a blanket
  /// "prune anything not in server list" sweep (that would reproduce RF-2).
  ///
  /// Entries are only evicted by TTL expiry — not by server-still-has-it. A
  /// transient stale list (consensus lag right after a delete) must not erase
  /// a buffered entry; otherwise the next refresh would see the credential
  /// genuinely gone but have nothing left in the buffer to re-signal.
  void _reconcileRecentlyDeleted() {
    final now = DateTime.now();
    final serverIds = _passkeys.map((p) => p.id).toSet();
    final toRemove = <String>[];
    _recentlyDeleted.forEach((id, entry) {
      if (now.difference(entry.deletedAt) > _recentlyDeletedTtl) {
        toRemove.add(id);
        return;
      }
      // Skip entries whose initial signal hasn't settled yet — the delete-
      // time signalFuture is still in flight, and re-firing here would
      // double-deliver for a single user-initiated delete.
      if (!entry.initialSignalSettled) {
        return;
      }
      if (!serverIds.contains(id)) {
        unawaited(WebAuthnService.signalUnknownCredential(entry.rpId, id));
      }
      // else: leave in buffer; next reconcile after TTL or after server list
      // catches up will resolve.
    });
    for (final id in toRemove) {
      _recentlyDeleted.remove(id);
    }
  }

  /// Register a new passkey via the WebAuthn ceremony.
  Future<void> registerPasskey() async {
    if (_disposed) return;
    _isLoading = true;
    _error = null;
    _safeNotify();

    try {
      final beginResult = await _api.beginPasskeyRegistration();
      if (_disposed) return;

      final sessionId = beginResult['session_id'] as String;
      final options = beginResult['publicKey'] as Map<String, dynamic>;

      final credential = await WebAuthnService.createCredential(options);
      if (_disposed) return;

      await _api.finishPasskeyRegistration(sessionId, credential);
      if (_disposed) return;

      await loadPasskeys();
    } on ApiException catch (e) {
      if (_disposed) return;
      // friendlyApiError covers the 409 passkey_already_registered case with
      // a non-destructive message; no special-case branch needed here.
      _error = friendlyApiError(e);
      _isLoading = false;
      _safeNotify();
      rethrow;
    } on Object catch (e) {
      if (_disposed) return;
      _error = friendlyPasskeyError(e);
      _isLoading = false;
      _safeNotify();
      rethrow;
    }
  }

  /// Delete a passkey by ID. Returns the SignalDeliveryState so the UI can
  /// render the right SnackBar copy variant (E6).
  Future<SignalDeliveryState> deletePasskey(String id) async {
    if (_disposed) return SignalDeliveryState.unsupported;
    _isLoading = true;
    _error = null;
    _safeNotify();

    try {
      final response = await _api.deletePasskey(id);
      if (_disposed) return SignalDeliveryState.unsupported;

      final rpId = (response['rp_id'] as String?) ?? '';
      final credentialId = (response['credential_id'] as String?) ?? id;

      // D-9: fire the Signal API AFTER server success, in parallel with the
      // list-reload. We need the signal's delivery state to pick the right
      // SnackBar copy, but the network round-trips are independent.
      Future<bool>? signalFuture;
      if (WebAuthnService.signalApiSupported()) {
        // Buffer for D-13 reconciliation if delivery fails or is dropped.
        // initialSignalSettled stays false until the signalFuture resolves —
        // the reconciler (run by loadPasskeys) must not re-fire while the
        // delete-time signal is still in flight.
        _recordRecentlyDeleted(credentialId, rpId);
        signalFuture =
            WebAuthnService.signalUnknownCredential(rpId, credentialId);
      }

      final listFuture = loadPasskeys();
      var deliveryState = SignalDeliveryState.unsupported;
      if (signalFuture != null) {
        final ok = await signalFuture;
        final entry = _recentlyDeleted[credentialId];
        if (entry != null) {
          entry.initialSignalSettled = true;
          if (ok) {
            // Signal delivered: no further reconciliation needed. Keep the
            // TTL-bounded record strictly as a deletion breadcrumb, not a
            // retry queue.
            _recentlyDeleted.remove(credentialId);
          }
        }
        deliveryState = ok
            ? SignalDeliveryState.delivered
            : SignalDeliveryState.failed;
      }
      await listFuture;
      return deliveryState;
    } on Object catch (e) {
      if (_disposed) return SignalDeliveryState.unsupported;
      _error = e.toString();
      _isLoading = false;
      _safeNotify();
      rethrow;
    }
  }

  /// Rename a passkey.
  Future<void> renamePasskey(String id, String name) async {
    if (_disposed) return;
    _isLoading = true;
    _error = null;
    _safeNotify();

    try {
      await _api.renamePasskey(id, name);
      if (_disposed) return;
      // No Signal API call on rename: the "friendly name" is a piccolod-local
      // label, not the WebAuthn account identity (user.name / displayName).
      // The account-level identity signal is intentionally omitted — firing
      // it here would clobber the OS passkey picker's account display with a
      // per-credential nickname.
      await loadPasskeys();
    } on Object catch (e) {
      if (_disposed) return;
      _error = e.toString();
      _isLoading = false;
      _safeNotify();
      rethrow;
    }
  }

  void _recordRecentlyDeleted(String id, String rpId) {
    _recentlyDeleted[id] = _RecentlyDeleted(rpId: rpId, deletedAt: DateTime.now());
    // Cap memory: keep at most 50 recent entries.
    if (_recentlyDeleted.length > 50) {
      final oldestKey = _recentlyDeleted.entries
          .reduce((a, b) =>
              a.value.deletedAt.isBefore(b.value.deletedAt) ? a : b)
          .key;
      _recentlyDeleted.remove(oldestKey);
    }
  }

}

class _RecentlyDeleted {
  _RecentlyDeleted({required this.rpId, required this.deletedAt});
  final String rpId;
  final DateTime deletedAt;
  /// Set true once the delete-time signal has settled (success or failure).
  /// The reconciler only re-fires when this is true — otherwise the initial
  /// signal kicked off by deletePasskey is still in flight and re-firing
  /// would double-deliver.
  bool initialSignalSettled = false;
}
