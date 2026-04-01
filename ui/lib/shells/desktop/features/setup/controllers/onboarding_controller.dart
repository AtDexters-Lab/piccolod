import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/shells/desktop/features/setup/setup_utils.dart';

enum OnboardingPhase { choice, installDisk, installComplete }

class OnboardingController extends ChangeNotifier {
  OnboardingController({
    required this.onComplete,
    this.bootMode,
    this.bootOrderConfigured = false,
    this.installTaskId,
    this.installAbandoned = false,
  }) {
    if (installTaskId != null) {
      _step = OnboardingPhase.installDisk;
    }
    if (installAbandoned) {
      _revertFuture = _revertAbandonedInstall();
    }
  }

  final VoidCallback onComplete;
  final String? bootMode;
  bool bootOrderConfigured;
  String? installTaskId;
  final bool installAbandoned;
  final ApiClient _api = ApiClient();
  bool _disposed = false;
  Future<void>? _revertFuture;

  @override
  void dispose() {
    _disposed = true;
    super.dispose();
  }

  OnboardingPhase _step = OnboardingPhase.choice;
  OnboardingPhase get step => _step;

  String? _error;
  String? get error => _error;

  bool _isLoading = false;
  bool get isLoading => _isLoading;

  List<Map<String, dynamic>> _disks = [];
  List<Map<String, dynamic>> get disks => _disks;

  bool get isFirstSetupFlow => false;

  /// Revert an abandoned USB install back to the onboarding choice.
  Future<void> _revertAbandonedInstall() async {
    try {
      await _api.post('/api/v1/system/onboarding', body: {'choice': 'pending'});
    } on Object catch (e) {
      debugPrint('Revert to onboarding failed: $e');
      if (_disposed) return;
      _error = 'Installation failed. Please reboot to try again.';
      notifyListeners();
    }
  }

  /// User chose "Try Piccolo" — persist choice and trigger disk prep.
  Future<void> chooseTryPiccolo() async {
    try {
      _isLoading = true;
      _error = null;
      notifyListeners();

      if (_revertFuture != null) await _revertFuture;
      await _api.post(
        '/api/v1/system/onboarding',
        body: {'choice': 'try_piccolo'},
      );

      // Disk prep started on backend. Signal router to re-check boot.
      onComplete();
    } on Object catch (e) {
      if (_disposed) return;
      _error = e.toString();
      _isLoading = false;
      notifyListeners();
    }
  }

  /// User chose "Install to Disk" — fetch disks and transition.
  Future<void> chooseInstallDisk() async {
    try {
      _isLoading = true;
      _error = null;
      notifyListeners();

      if (_revertFuture != null) await _revertFuture;
      await fetchDisks();
      if (_disposed) return;
      _step = OnboardingPhase.installDisk;
      _isLoading = false;
      notifyListeners();
    } on Object catch (e) {
      if (_disposed) return;
      _error = e.toString();
      _isLoading = false;
      notifyListeners();
    }
  }

  Future<void> fetchDisks() async {
    final response = await _api.get('/api/v1/storage/disks') as Map<String, dynamic>;
    final rawDisks = response['disks'] as List<dynamic>? ?? <dynamic>[];
    _disks = rawDisks.cast<Map<String, dynamic>>();
    if (!_disposed) notifyListeners();
  }

  Future<bool> startInstall(String targetDisk) async {
    try {
      _error = null;
      final taskId = 'install-${DateTime.now().millisecondsSinceEpoch}';
      installTaskId = taskId;
      notifyListeners();

      await _api.post('/api/v1/system/install-to-disk', body: {
        'target_disk': targetDisk,
        'confirm_data_loss': true,
        'task_id': taskId,
      });
      return true;
    } on ApiException catch (e) {
      installTaskId = null;
      _error = extractServerError(e.message);
      if (!_disposed) notifyListeners();
      return false;
    } on Object catch (e) {
      installTaskId = null;
      _error = e.toString();
      if (!_disposed) notifyListeners();
      return false;
    }
  }

  Future<void> onInstallComplete() async {
    try {
      final onboarding = await _api.get('/api/v1/system/onboarding') as Map<String, dynamic>;
      bootOrderConfigured = onboarding['boot_order_configured'] == true;
    } on Object catch (_) {}
    if (_disposed) return;
    _step = OnboardingPhase.installComplete;
    notifyListeners();
  }

  Future<void> rebootAfterInstall() async {
    try {
      await _api.post('/api/v1/system/reboot');
    } on ApiException {
      rethrow;
    } on Object catch (_) {
      // Connection reset during reboot — expected.
    }
  }

  void backToChoice() {
    _step = OnboardingPhase.choice;
    _error = null;
    installTaskId = null;
    notifyListeners();
  }
}
