import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/os_update.dart';
import 'package:piccolo_os/core/services/network_service.dart';
import 'package:piccolo_os/core/models/session.dart';
import 'package:piccolo_os/core/models/remote_models.dart';
import 'package:piccolo_os/core/config/core_config.dart';
import 'package:piccolo_os/core/services/api_client.dart';
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

  // Navigation
  int _selectedIndex = 0;
  int get selectedIndex => _selectedIndex;

  void selectTab(int index) {
    if (_disposed) return;
    _selectedIndex = index;
    notifyListeners();
    _refreshCurrentTab();
  }

  Future<void> _refreshCurrentTab() async {
    if (_disposed) return;
    // logic to refresh data based on selected tab
    switch (_selectedIndex) {
      case 0: // Profile
        await fetchSession();
        break;
      case 1: // Remote Access
        await fetchRemoteStatus();
        break;
      case 2: // Users
        // Users tab has its own controller
        break;
      case 3: // Security
        await fetchSpecificHostname();
        break;
      case 4: // System
        await fetchOSUpdate();
        if (!_disposed && _isBackendBusy) {
          _pollWhileBusy();
        }
        break;
    }
  }

  bool _isPolling = false;

  Future<void> _pollWhileBusy() async {
    if (_isPolling || _disposed) return;
    _isPolling = true;

    try {
      while (_isBackendBusy && !_disposed) {
        await Future.delayed(const Duration(seconds: 5));
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
      _session = Session.fromJson(response);
      _error = null;
    } catch (e) {
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
      _osUpdate = OSUpdate.fromJson(response);
      _error = null;
      _isBackendBusy = false;
    } catch (e) {
      if (_disposed) return;
      if (e.toString().contains("429")) {
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
      _remoteStatus = RemoteStatus.fromJson(response);
      _error = null;
    } catch (e) {
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
    } catch (_) {
      // Non-critical — UI falls back to generic text
    }
  }

  // Actions
  Future<void> logout(VoidCallback onLogoutSuccess) async {
    try {
      await ApiClient().post('/api/v1/auth/logout');
      if (_disposed) return;
      onLogoutSuccess();
    } catch (e) {
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
    } catch (e) {
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
     } catch (e) {
       if (!_disposed) {
         _error = e.toString();
         notifyListeners();
       }
     }
  }
  
  void downloadDiagnosticLog({DateTime? from, DateTime? to}) {
    String formatDate(DateTime d) =>
        '${d.year}-${d.month.toString().padLeft(2, '0')}-${d.day.toString().padLeft(2, '0')}';

    final params = <String, String>{};
    if (from != null) params['from'] = formatDate(from);
    if (to != null) params['to'] = formatDate(to);

    final uri = Uri.parse(
        '${CoreConfig.apiBaseUrl}/api/v1/system/admin/diagnostic-log')
      .replace(queryParameters: params.isNotEmpty ? params : null);

    downloader.downloadFromUrl(uri.toString(), 'piccolod-diagnostic.log');
  }

  Future<void> downloadCACertificate() async {
    try {
      final response = await ApiClient().get('/api/v1/system/ca.crt');
      downloader.downloadTextFile(response as String, 'piccolo-ca.crt');
    } catch (e) {
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
      } catch (_) {
        // Ignore errors here
      }
      
      await _waitForSystemRevival();
    } catch (e) {
      if (!_disposed) {
        _isRebooting = false;
        _error = "Reboot failed: $e";
        notifyListeners();
      }
    }
  }

  Future<void> _waitForSystemRevival() async {
    await Future.delayed(const Duration(seconds: 5));

    while (!_disposed) {
      try {
        final sessionData = await ApiClient().get('/api/v1/auth/session');
        if (_disposed) return;
        final session = Session.fromJson(sessionData);
        
        _isRebooting = false;
        notifyListeners();

        if (!session.authenticated) {
          onSessionExpired?.call();
        } else {
          await fetchOSUpdate();
        }
        return;
      } catch (_) {
        await Future.delayed(const Duration(seconds: 2));
      }
    }
  }

  Future<void> checkForUpdates() async {
    _isUpdateInProgress = true;
    if (!_disposed) notifyListeners();
    
    final minWait = Future.delayed(const Duration(seconds: 2));

    try {
      await ApiClient().fetchCsrfToken();
      if (_disposed) return;
      try {
        await ApiClient().post('/api/v1/updates/os/apply');
      } catch (e) {
        if (_disposed) return;
        if (!e.toString().contains("429")) {
          _error = e.toString();
          return;
        }
      }
      
      await minWait;
      if (_disposed) return;
      await _pollForCompletion();

    } catch (e) {
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
      } catch (e) {
         if (_disposed) return;
         if (!e.toString().contains("429")) {
          _error = e.toString();
          return;
        }
      }
      
      await _pollForCompletion();
    } catch (e) {
      if (!_disposed) _error = e.toString();
    } finally {
      if (!_disposed) {
        _isUpdateInProgress = false;
        notifyListeners();
      }
    }
  }

  Future<void> _pollForCompletion() async {
      int idleCount = 0; 
      const int maxIdleChecks = 3; 

      while (!_disposed) {
        await fetchOSUpdate(silent: true);
        if (_disposed) break;

        if (_isBackendBusy) {
          idleCount = 0; 
          await Future.delayed(const Duration(seconds: 5));
          continue;
        }

        if (_osUpdate?.pending == true) {
          break;
        }

        // If we are here, status is 200 OK + Pending=False.
        idleCount++;
        if (idleCount >= maxIdleChecks) {
          break;
        }
        
        await Future.delayed(const Duration(seconds: 2));
      }
  }
}