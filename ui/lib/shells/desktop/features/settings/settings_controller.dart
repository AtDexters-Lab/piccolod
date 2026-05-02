import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/auto_unlock.dart';
import 'package:piccolo_os/core/models/os_update.dart';
import 'package:piccolo_os/core/models/remote_models.dart';
import 'package:piccolo_os/core/models/session.dart';
import 'package:piccolo_os/core/models/system_timezone.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/network_service.dart';
import 'package:piccolo_os/core/utils/downloader/downloader.dart' as downloader;

class SettingsController extends ChangeNotifier {
  bool _disposed = false;

  @override
  void dispose() {
    _disposed = true;
    super.dispose();
  }

  void _setLoading(bool value) {
    if (_disposed) return;
    _isLoading = value;
    notifyListeners();
  }

  // State
  bool _isLoading = false;
  bool get isLoading => _isLoading;

  String? _error;
  String? get error => _error;

  OSUpdate? _osUpdate;
  OSUpdate? get osUpdate => _osUpdate;

  Session? _session;
  Session? get session => _session;

  RemoteStatus? _remoteStatus;
  RemoteStatus? get remoteStatus => _remoteStatus;

  String? _specificHostname;
  String? get specificHostname => _specificHostname;

  AutoUnlockState? _autoUnlock;
  AutoUnlockState? get autoUnlock => _autoUnlock;

  SystemTimezone? _timezone;
  SystemTimezone? get timezone => _timezone;

  bool _autoUnlockTesting = false;
  bool get autoUnlockTesting => _autoUnlockTesting;

  String? _autoUnlockTestResult; // Empty = success; non-empty = failure-token.
  String? get autoUnlockTestResult => _autoUnlockTestResult;
  int? _autoUnlockTestLatencyMs;
  int? get autoUnlockTestLatencyMs => _autoUnlockTestLatencyMs;

  bool _autoUnlockSaving = false;
  bool get autoUnlockSaving => _autoUnlockSaving;

  bool _timezoneSaving = false;
  bool get timezoneSaving => _timezoneSaving;

  // Navigation
  int _selectedIndex = 0;
  int get selectedIndex => _selectedIndex;

  void selectTab(int index) {
    if (_disposed) return;
    _selectedIndex = index;
    notifyListeners();
    unawaited(_refreshCurrentTab());
  }

  Future<void> _refreshCurrentTab() async {
    if (_disposed) return;
    // logic to refresh data based on selected tab
    switch (_selectedIndex) {
      case 0: // Profile
        await fetchSession();
      case 1: // Remote Access
        await fetchRemoteStatus();
      case 2: // Users
        // Users tab has its own controller
        break;
      case 3: // Security
        await fetchSpecificHostname();
        await fetchAutoUnlock();
      case 4: // System
        await fetchOSUpdate();
        await fetchTimezone();
        // Auto-unlock state drives the maintenance-window subtitle on the
        // Update Available card. Fetched here too (not just on Security)
        // so the operator sees the auto-reboot plan when they open System
        // first.
        await fetchAutoUnlock();
        if (!_disposed && _isBackendBusy) {
          unawaited(_pollWhileBusy());
        }
    }
  }

  bool _isPolling = false;

  Future<void> _pollWhileBusy() async {
    if (_isPolling || _disposed) return;
    _isPolling = true;

    try {
      while (_isBackendBusy && !_disposed) {
        await Future<void>.delayed(const Duration(seconds: 5));
        if (_disposed) break;
        await fetchOSUpdate(silent: true);
      }
    } finally {
      _isPolling = false;
    }
  }

  Future<void> fetchSession() async {
    if (_disposed) return;
    _setLoading(true);
    try {
      final response = await ApiClient().get('/api/v1/auth/session');
      if (_disposed) return;
      _session = Session.fromJson(response as Map<String, dynamic>);
      _error = null;
    } on Object catch (e) {
      if (_disposed) return;
      _error = e.toString();
    } finally {
      if (!_disposed) _setLoading(false);
    }
  }

  bool _isBackendBusy = false;
  bool get isBackendBusy => _isBackendBusy;

  Future<void> fetchOSUpdate({bool silent = false}) async {
    if (_disposed) return;
    if (!silent) _setLoading(true);
    try {
      final response = await ApiClient().get('/api/v1/updates/os');
      if (_disposed) return;
      _osUpdate = OSUpdate.fromJson(response as Map<String, dynamic>);
      _error = null;
      _isBackendBusy = false;
    } on Object catch (e) {
      if (_disposed) return;
      if (e.toString().contains('429')) {
        // Backend is busy (Preparing update)
        _isBackendBusy = true;
        _error = null;
        // We keep the old _osUpdate if available, or null
      } else {
        _error = e.toString();
        _isBackendBusy = false;
      }
    } finally {
      if (!_disposed) {
        if (!silent) _setLoading(false);
        notifyListeners(); // Notify changes to busy state
      }
    }
  }

  Future<void> fetchRemoteStatus() async {
    if (_disposed) return;
    _setLoading(true);
    try {
      final response = await ApiClient().get('/api/v1/remote/status');
      if (_disposed) return;
      _remoteStatus = RemoteStatus.fromJson(response as Map<String, dynamic>);
      _error = null;
    } on Object catch (e) {
      if (_disposed) return;
      _error = e.toString();
    } finally {
      if (!_disposed) _setLoading(false);
    }
  }

  Future<void> fetchSpecificHostname() async {
    if (_disposed) return;
    try {
      final peers = await NetworkService(ApiClient()).getPeers();
      if (_disposed) return;
      final h = peers.self?.specificHostname;
      _specificHostname = (h != null && h.isNotEmpty) ? h : null;
      notifyListeners();
    } on Object catch (_) {
      // Non-critical — UI falls back to generic text
    }
  }

  // Actions
  Future<void> logout(VoidCallback onLogoutSuccess) async {
    try {
      await ApiClient().post('/api/v1/auth/logout');
      if (_disposed) return;
      onLogoutSuccess();
    } on Object catch (e) {
      if (!_disposed) {
        _error = e.toString();
        notifyListeners();
      }
    }
  }

  Future<void> changePassword(String oldPassword, String newPassword) async {
    if (_disposed) return;
    _setLoading(true);
    try {
      await ApiClient().post('/api/v1/auth/password', body: {
        'old_password': oldPassword,
        'new_password': newPassword,
      });
      if (_disposed) return;
      _error = null;
      // Success
    } on Object catch (e) {
      if (!_disposed) _error = e.toString();
      rethrow; // Let UI handle error display
    } finally {
      if (!_disposed) _setLoading(false);
    }
  }

  Future<void> disableRemote() async {
     try {
       await ApiClient().post('/api/v1/remote/disable');
       if (_disposed) return;
       await fetchRemoteStatus();
     } on Object catch (e) {
       if (!_disposed) {
         _error = e.toString();
         notifyListeners();
       }
     }
  }

  Future<void> downloadCACertificate() async {
    try {
      final response = await ApiClient().get('/api/v1/system/ca.crt');
      downloader.downloadTextFile(response as String, 'piccolo-ca.crt');
    } on Object catch (_) {
      if (!_disposed) {
        _error = 'Failed to download CA certificate';
        notifyListeners();
      }
    }
  }

  // Callbacks
  VoidCallback? onSessionExpired;

  bool _isUpdateInProgress = false;
  bool get isUpdateInProgress => _isUpdateInProgress;

  bool _isRebooting = false;
  bool get isRebooting => _isRebooting;

  Future<void> rebootOS() async {
    _isRebooting = true;
    if (!_disposed) notifyListeners();

    try {
      await ApiClient().fetchCsrfToken();
      if (_disposed) return;
      try {
        await ApiClient().post('/api/v1/updates/os/reboot');
      } on Object catch (_) {
        // Ignore errors here
      }

      await _waitForSystemRevival();
    } on Object catch (e) {
      if (!_disposed) {
        _isRebooting = false;
        _error = 'Reboot failed: $e';
        notifyListeners();
      }
    }
  }

  Future<void> _waitForSystemRevival() async {
    await Future<void>.delayed(const Duration(seconds: 5));

    while (!_disposed) {
      try {
        final sessionData = await ApiClient().get('/api/v1/auth/session');
        if (_disposed) return;
        final session = Session.fromJson(sessionData as Map<String, dynamic>);

        _isRebooting = false;
        notifyListeners();

        if (!session.authenticated) {
          onSessionExpired?.call();
        } else {
          await fetchOSUpdate();
        }
        return;
      } on Object catch (_) {
        await Future<void>.delayed(const Duration(seconds: 2));
      }
    }
  }

  Future<void> checkForUpdates() async {
    _isUpdateInProgress = true;
    if (!_disposed) notifyListeners();

    final minWait = Future<void>.delayed(const Duration(seconds: 2));

    try {
      await ApiClient().fetchCsrfToken();
      if (_disposed) return;
      try {
        await ApiClient().post('/api/v1/updates/os/apply');
      } on Object catch (e) {
        if (_disposed) return;
        if (!e.toString().contains('429')) {
          _error = e.toString();
          return;
        }
      }

      await minWait;
      if (_disposed) return;
      await _pollForCompletion();

    } on Object catch (e) {
      if (!_disposed) _error = e.toString();
    } finally {
      if (!_disposed) {
        _isUpdateInProgress = false;
        notifyListeners();
      }
    }
  }

  Future<void> rollbackOS() async {
    _isUpdateInProgress = true;
    if (!_disposed) notifyListeners();

    try {
      await ApiClient().fetchCsrfToken();
      if (_disposed) return;
      try {
        await ApiClient().post('/api/v1/updates/os/rollback');
      } on Object catch (e) {
         if (_disposed) return;
         if (!e.toString().contains('429')) {
          _error = e.toString();
          return;
        }
      }

      await _pollForCompletion();
    } on Object catch (e) {
      if (!_disposed) _error = e.toString();
    } finally {
      if (!_disposed) {
        _isUpdateInProgress = false;
        notifyListeners();
      }
    }
  }

  Future<void> _pollForCompletion() async {
      var idleCount = 0;
      const maxIdleChecks = 3;

      while (!_disposed) {
        await fetchOSUpdate(silent: true);
        if (_disposed) break;

        if (_isBackendBusy) {
          idleCount = 0;
          await Future<void>.delayed(const Duration(seconds: 5));
          continue;
        }

        if (_osUpdate?.pending ?? false) {
          break;
        }

        // If we are here, status is 200 OK + Pending=False.
        idleCount++;
        if (idleCount >= maxIdleChecks) {
          break;
        }

        await Future<void>.delayed(const Duration(seconds: 2));
      }
  }

  // ─────────────────────────────────────────────────────────
  // Auto-Unlock (Settings → Security)
  // ─────────────────────────────────────────────────────────

  Future<void> fetchAutoUnlock() async {
    if (_disposed) return;
    try {
      final response = await ApiClient().get('/api/v1/security/auto-unlock');
      if (_disposed) return;
      _autoUnlock = AutoUnlockState.fromJson(response as Map<String, dynamic>);
      _error = null;
      notifyListeners();
    } on Object catch (e) {
      if (_disposed) return;
      _error = e.toString();
      notifyListeners();
    }
  }

  /// Updates auto-unlock state with a partial body. Pass only the fields
  /// being changed; the backend merges with persisted state. Returns the
  /// updated state on success; throws ApiException with the `error` body
  /// on validation failure (e.g. invalid window).
  Future<void> updateAutoUnlock({
    bool? enabled,
    bool? autoRebootEnabled,
    int? windowStartHour,
    int? windowEndHour,
  }) async {
    if (_disposed) return;
    _autoUnlockSaving = true;
    notifyListeners();
    try {
      final body = <String, dynamic>{};
      if (enabled != null) body['enabled'] = enabled;
      final ar = <String, dynamic>{};
      if (autoRebootEnabled != null) ar['enabled'] = autoRebootEnabled;
      if (windowStartHour != null) ar['window_start_hour'] = windowStartHour;
      if (windowEndHour != null) ar['window_end_hour'] = windowEndHour;
      if (ar.isNotEmpty) body['auto_reboot'] = ar;
      final response = await ApiClient().put('/api/v1/security/auto-unlock', body: body);
      if (_disposed) return;
      _autoUnlock = AutoUnlockState.fromJson(response as Map<String, dynamic>);
      _error = null;
    } on Object catch (e) {
      if (!_disposed) _error = e.toString();
      rethrow;
    } finally {
      if (!_disposed) {
        _autoUnlockSaving = false;
        notifyListeners();
      }
    }
  }

  /// Runs the namek round-trip self-test. Captures success + latency or
  /// failure-token on the controller; UI reads via autoUnlockTestResult /
  /// autoUnlockTestLatencyMs. Cleared on next fetchAutoUnlock or another
  /// runAutoUnlockTest invocation.
  Future<void> runAutoUnlockTest() async {
    if (_disposed) return;
    _autoUnlockTesting = true;
    _autoUnlockTestResult = null;
    _autoUnlockTestLatencyMs = null;
    notifyListeners();
    try {
      final response = await ApiClient().post('/api/v1/security/auto-unlock/test');
      if (_disposed) return;
      final data = response as Map<String, dynamic>? ?? <String, dynamic>{};
      final ok = data['success'] == true;
      _autoUnlockTestResult = ok ? '' : ((data['error_kind'] as String?) ?? 'unknown');
      _autoUnlockTestLatencyMs = data['latency_ms'] as int?;
    } on ApiException catch (e) {
      // 429 from the orchestrator's 5s rate-limit. Map to a synthetic token
      // so the UI can render the friendly "Tested too recently" copy via
      // autoUnlockFailureReasonLabel instead of surfacing a raw exception.
      if (!_disposed) _autoUnlockTestResult = e.statusCode == 429 ? 'rate_limited' : e.toString();
    } on Object catch (e) {
      if (!_disposed) _autoUnlockTestResult = e.toString();
    } finally {
      if (!_disposed) {
        _autoUnlockTesting = false;
        notifyListeners();
      }
    }
  }

  // ─────────────────────────────────────────────────────────
  // Timezone (Settings → System)
  // ─────────────────────────────────────────────────────────

  Future<void> fetchTimezone() async {
    if (_disposed) return;
    try {
      final response = await ApiClient().get('/api/v1/system/timezone');
      if (_disposed) return;
      _timezone = SystemTimezone.fromJson(response as Map<String, dynamic>);
      notifyListeners();
    } on Object catch (e) {
      if (_disposed) return;
      _error = e.toString();
      notifyListeners();
    }
  }

  Future<void> updateTimezone(String zone) async {
    if (_disposed) return;
    _timezoneSaving = true;
    notifyListeners();
    try {
      final response = await ApiClient().put(
        '/api/v1/system/timezone',
        body: {'timezone': zone},
      );
      if (_disposed) return;
      _timezone = SystemTimezone.fromJson(
        // PUT returns {timezone}; merge with is_set=true since we just set it.
        {...?(response as Map<String, dynamic>?), 'is_set': true},
      );
      _error = null;
    } on Object catch (e) {
      if (!_disposed) _error = e.toString();
      rethrow;
    } finally {
      if (!_disposed) {
        _timezoneSaving = false;
        notifyListeners();
      }
    }
  }
}
