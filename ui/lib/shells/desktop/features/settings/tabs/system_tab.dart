import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/os_update.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/shared/widgets/info_row.dart';
import 'package:piccolo_os/shared/widgets/log_stream_viewer.dart';
import 'package:piccolo_os/shared/widgets/piccolo_card.dart';
import 'package:piccolo_os/shared/widgets/task_progress_panel.dart';
import 'package:piccolo_os/shells/desktop/features/settings/settings_controller.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class SystemTab extends StatelessWidget {

  const SystemTab({required this.controller, super.key});
  final SettingsController controller;

  @override
  Widget build(BuildContext context) {
    // If rebooting, show the modal-like overlay
    if (controller.isRebooting) {
      return const Center(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            CircularProgressIndicator(strokeWidth: 3),
            SizedBox(height: 32),
            Text(
              'System is restarting...',
              style: TextStyle(fontSize: 20, fontWeight: FontWeight.w600),
            ),
            SizedBox(height: 8),
            Text('You will be redirected to login once the system is back online.'),
          ],
        ),
      );
    }

    final update = controller.osUpdate;
    final isBusy = controller.isUpdateInProgress || controller.isBackendBusy;

    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text('System Update', style: PiccoloTheme.textTheme.headlineLarge),
        const SizedBox(height: 32),

        if (update != null || isBusy) ...[
          // 1. Hero Status Card
          _UpdateStatusCard(
            update: update,
            isChecking: isBusy,
            onCheck: controller.checkForUpdates,
            onReboot: controller.rebootOS,
          ),

          const SizedBox(height: 48),

          if (update != null) ...[
            // 2. Advanced / Danger Zone (Only show if we have data)
            Text('Advanced Options', style: PiccoloTheme.textTheme.titleMedium),
            const SizedBox(height: 16),
            PiccoloCard(
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  InfoRow('Current Version', update.currentVersion),
                  InfoRow('Last Checked', _formatDate(update.lastChecked)),
                  const Divider(height: 32),
                  _DiagnosticLogSection(controller: controller),
                  const Divider(height: 32),
                  Row(
                    children: [
                      Expanded(
                        child: Column(
                          crossAxisAlignment: CrossAxisAlignment.start,
                          children: [
                            Text('Rollback System', style: PiccoloTheme.textTheme.bodyMedium?.copyWith(fontWeight: FontWeight.bold)),
                            const SizedBox(height: 4),
                            Text(
                              'Revert to the previous system snapshot. Useful if an update caused issues.',
                              style: PiccoloTheme.textTheme.labelSmall,
                            ),
                          ],
                        ),
                      ),
                      OutlinedButton(
                        onPressed: (controller.isLoading || isBusy) ? null : () => _showRollbackConfirmation(context),
                        style: OutlinedButton.styleFrom(
                          foregroundColor: PiccoloTheme.critical,
                          side: const BorderSide(color: PiccoloTheme.critical),
                        ),
                        child: const Text('Rollback'),
                      ),
                    ],
                  ),
                ],
              ),
            ),
          ],

          const SizedBox(height: 48),
          const _InstallToDiskCard(),
          const SizedBox(height: 48),
          Text('Update Logs', style: PiccoloTheme.textTheme.titleMedium),
          const SizedBox(height: 16),
          LogStreamViewer(
            systemUnit: 'transactional-update',
            height: 320,
            autoConnect: isBusy,
          ),
        ] else
           const Text('System information unavailable.'),

      ],
    );
  }

  void _showRollbackConfirmation(BuildContext context) {
    unawaited(showDialog<void>(
      context: context,
      builder: (context) => AlertDialog(
        title: const Text('Confirm Rollback'),
        content: const Text(
          'Are you sure you want to rollback the system to the previous snapshot?\n\n'
          'This will discard any system changes made since the last update. User data (files) will be preserved.',
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.pop(context),
            child: const Text('Cancel'),
          ),
          FilledButton(
            onPressed: () {
              Navigator.pop(context);
              unawaited(controller.rollbackOS());
            },
            style: FilledButton.styleFrom(backgroundColor: PiccoloTheme.critical),
            child: const Text('Rollback'),
          ),
        ],
      ),
    ));
  }

  String _formatDate(DateTime dt) {
    // Simple formatter, can be replaced with intl package later
    return "${dt.year}-${dt.month.toString().padLeft(2,'0')}-${dt.day.toString().padLeft(2,'0')} ${dt.hour.toString().padLeft(2,'0')}:${dt.minute.toString().padLeft(2,'0')}";
  }
}

class _UpdateStatusCard extends StatelessWidget {

  const _UpdateStatusCard({
    required this.update,
    required this.isChecking,
    required this.onCheck,
    required this.onReboot,
  });
  final OSUpdate? update; // Make nullable
  final bool isChecking;
  final VoidCallback onCheck;
  final VoidCallback onReboot;

  @override
  Widget build(BuildContext context) {
    // Determine State
    final pendingReboot = update?.pending ?? false;

    var accentColor = PiccoloTheme.success;
    var icon = PiccoloIcons.success;
    var title = 'System is up to date';
    var subtitle = update != null ? 'Version ${update!.currentVersion}' : 'Checking version...';
    Widget? action;

    if (isChecking) {
      accentColor = PiccoloTheme.cobalt600;
      icon = PiccoloIcons.sync;
      title = 'Checking for updates...';
      subtitle = 'Please wait.';
      action = const SizedBox(
        height: 24,
        width: 24,
        child: CircularProgressIndicator(strokeWidth: 2)
      );
    } else if (pendingReboot) {
      accentColor = PiccoloTheme.cobalt600; // or Info color
      icon = PiccoloIcons.systemUpdate;
      title = 'Update Available';
      subtitle = 'Version ${update!.availableVersion} is ready to install.';
      action = FilledButton.icon(
        onPressed: onReboot,
        icon: const Icon(PiccoloIcons.restart),
        label: const Text('Restart Now'),
        style: FilledButton.styleFrom(
          padding: const EdgeInsets.symmetric(horizontal: 24, vertical: 12),
        ),
      );
    } else if (update != null) {
      // Idle / Up to date
      action = OutlinedButton(
        onPressed: onCheck,
        child: const Text('Check for Updates'),
      );
    } else {
      // Update is null and not checking? Fallback
      title = 'Unknown Status';
      subtitle = '';
    }

    return Container(
      padding: const EdgeInsets.all(32),
      decoration: BoxDecoration(
        color: PiccoloTheme.porcelain,
        borderRadius: BorderRadius.circular(Radii.lg),
        boxShadow: Elevation.elev3,
      ),
      child: Row(
        children: [
          Container(
            padding: const EdgeInsets.all(16),
            decoration: BoxDecoration(
              color: accentColor.withValues(alpha: 0.1),
              shape: BoxShape.circle,
            ),
            child: isChecking
              ? SizedBox(width: 32, height: 32, child: CircularProgressIndicator(color: accentColor))
              : Icon(icon, color: accentColor, size: 32),
          ),
          const SizedBox(width: 24),
          Expanded(
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Text(title, style: PiccoloTheme.textTheme.headlineSmall),
                const SizedBox(height: 4),
                Text(subtitle, style: PiccoloTheme.textTheme.bodyMedium?.copyWith(color: PiccoloTheme.inkMuted)),
              ],
            ),
          ),
          if (action != null) ...[
            const SizedBox(width: 24),
            action,
          ]
        ],
      ),
    );
  }
}

/// Install to Disk card shown only when running from USB.
/// Checks onboarding endpoint to determine visibility.
class _InstallToDiskCard extends StatefulWidget {
  const _InstallToDiskCard();

  @override
  State<_InstallToDiskCard> createState() => _InstallToDiskCardState();
}

class _InstallToDiskCardState extends State<_InstallToDiskCard> {
  final ApiClient _api = ApiClient();
  bool _isUSBBoot = false;
  bool _loaded = false;
  bool _showInstallFlow = false;

  // Install flow state
  List<Map<String, dynamic>> _disks = [];
  String? _selectedDisk;
  String? _taskId;
  String? _error;
  bool _isInstalling = false;
  bool _installComplete = false;

  @override
  void initState() {
    super.initState();
    unawaited(_checkBootMode());
  }

  Future<void> _checkBootMode() async {
    try {
      final onboarding = await _api.get('/api/v1/system/onboarding');
      final bootMode = (onboarding as Map<String, dynamic>)['boot_mode'] as String?;
      final state = onboarding['state'] as String?;
      if (mounted) {
        setState(() {
          _isUSBBoot = bootMode == 'usb' &&
              (state == 'try_piccolo' || state == 'complete');
          _loaded = true;
        });
      }
    } on Object catch (_) {
      if (mounted) setState(() => _loaded = true);
    }
  }

  Future<void> _fetchDisks() async {
    try {
      final response = await _api.get('/api/v1/storage/disks');
      final rawDisks = (response as Map<String, dynamic>)['disks'] as List<dynamic>? ?? <dynamic>[];
      setState(() {
        _disks = rawDisks.cast<Map<String, dynamic>>();
        _showInstallFlow = true;
        _error = null;
      });
    } on Object catch (e) {
      setState(() => _error = e.toString());
    }
  }

  Future<void> _startInstall() async {
    if (_selectedDisk == null) return;

    final confirmed = await showDialog<bool>(
      context: context,
      barrierDismissible: false,
      builder: (context) => AlertDialog(
        title: const Text('Confirm Installation'),
        content: Text(
          'All data on $_selectedDisk will be erased. '
          'Running apps will be stopped. App configurations will be preserved '
          'if you restore from backup after install.\n\n'
          'This action cannot be undone.',
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.pop(context, false),
            child: const Text('Cancel'),
          ),
          FilledButton(
            onPressed: () => Navigator.pop(context, true),
            style: FilledButton.styleFrom(backgroundColor: PiccoloTheme.critical),
            child: const Text('Erase and Install'),
          ),
        ],
      ),
    );

    if (confirmed != true || !mounted) return;

    setState(() {
      _isInstalling = true;
      _error = null;
    });

    try {
      await _api.fetchCsrfToken();
      final taskId = 'install-${DateTime.now().millisecondsSinceEpoch}';
      await _api.post('/api/v1/system/install-to-disk', body: {
        'target_disk': _selectedDisk,
        'confirm_data_loss': true,
        'task_id': taskId,
      });
      setState(() => _taskId = taskId);
    } on Object catch (e) {
      setState(() {
        _isInstalling = false;
        _error = e.toString();
      });
    }
  }

  @override
  Widget build(BuildContext context) {
    if (!_loaded || !_isUSBBoot) return const SizedBox.shrink();

    if (_installComplete) {
      return _buildCompleteView();
    }
    if (_taskId != null) {
      return _buildProgressView();
    }
    if (_showInstallFlow) {
      return _buildDiskSelectionView();
    }
    return _buildPromptView();
  }

  Widget _buildPromptView() {
    return Container(
      padding: const EdgeInsets.all(24),
      decoration: BoxDecoration(
        color: PiccoloTheme.porcelain,
        borderRadius: BorderRadius.circular(Radii.md),
        border: Border.all(color: PiccoloTheme.hairline),
      ),
      child: Row(
        children: [
          Container(
            padding: const EdgeInsets.all(12),
            decoration: BoxDecoration(
              color: PiccoloTheme.cobalt600.withValues(alpha: 0.1),
              borderRadius: BorderRadius.circular(Radii.sm),
            ),
            child: const Icon(PiccoloIcons.saveToDisk, color: PiccoloTheme.cobalt600, size: 24),
          ),
          const SizedBox(width: 16),
          Expanded(
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Text('Install to Disk',
                    style: PiccoloTheme.textTheme.bodyMedium?.copyWith(fontWeight: FontWeight.bold)),
                const SizedBox(height: 4),
                Text(
                  'Running from USB. Install Piccolo to an internal disk for permanent use.',
                  style: PiccoloTheme.textTheme.labelSmall,
                ),
              ],
            ),
          ),
          const SizedBox(width: 16),
          FilledButton(
            onPressed: _fetchDisks,
            child: const Text('Install'),
          ),
        ],
      ),
    );
  }

  Widget _buildDiskSelectionView() {
    return Container(
      padding: const EdgeInsets.all(24),
      decoration: BoxDecoration(
        color: PiccoloTheme.porcelain,
        borderRadius: BorderRadius.circular(Radii.md),
        border: Border.all(color: PiccoloTheme.hairline),
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text('Install to Disk',
              style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
          const SizedBox(height: 8),
          const Text(
            'Select an internal disk. All data on the selected disk will be erased.',
            style: TextStyle(fontSize: 13, color: PiccoloTheme.inkMuted),
          ),
          const SizedBox(height: 16),
          if (_error != null) ...[
            Container(
              padding: const EdgeInsets.all(12),
              margin: const EdgeInsets.only(bottom: 12),
              decoration: BoxDecoration(
                color: PiccoloTheme.critical.withValues(alpha: 0.08),
                borderRadius: BorderRadius.circular(Radii.sm),
              ),
              child: Text(_error!,
                  style: const TextStyle(fontSize: 13, color: PiccoloTheme.critical)),
            ),
          ],
          if (_disks.isEmpty)
            const Center(
              child: Padding(
                padding: EdgeInsets.all(24),
                child: Text('No internal disks found.',
                    style: TextStyle(color: PiccoloTheme.inkMuted)),
              ),
            )
          else
            for (final disk in _disks)
              _SettingsDiskTile(
                device: disk['device'] as String? ?? '',
                model: disk['model'] as String? ?? 'Unknown',
                sizeGb: disk['size_gb'] as int? ?? 0,
                transport: disk['transport'] as String? ?? '',
                isSelected: _selectedDisk == disk['device'],
                onTap: _isInstalling
                    ? null
                    : () => setState(() => _selectedDisk = disk['device'] as String?),
              ),
          const SizedBox(height: 16),
          Row(
            mainAxisAlignment: MainAxisAlignment.end,
            children: [
              TextButton(
                onPressed: () => setState(() => _showInstallFlow = false),
                child: const Text('Cancel'),
              ),
              const SizedBox(width: 12),
              FilledButton(
                onPressed: (_selectedDisk != null && !_isInstalling) ? _startInstall : null,
                child: _isInstalling
                    ? const SizedBox(
                        width: 20,
                        height: 20,
                        child: CircularProgressIndicator(color: Colors.white, strokeWidth: 2),
                      )
                    : const Text('Install'),
              ),
            ],
          ),
        ],
      ),
    );
  }

  Widget _buildProgressView() {
    return Container(
      padding: const EdgeInsets.all(24),
      decoration: BoxDecoration(
        color: PiccoloTheme.porcelain,
        borderRadius: BorderRadius.circular(Radii.md),
        border: Border.all(color: PiccoloTheme.hairline),
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text('Installing Piccolo',
              style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
          const SizedBox(height: 8),
          const Text(
            'Do not power off or disconnect the device.',
            style: TextStyle(fontSize: 13, color: PiccoloTheme.inkMuted),
          ),
          const SizedBox(height: 16),
          TaskProgressPanel(
            taskId: _taskId!,
            taskType: 'Installation',
            urlPath: '/api/v1/system/install-progress/stream',
            onComplete: (evt) {
              if (evt.error == null || evt.error!.isEmpty) {
                setState(() => _installComplete = true);
              }
            },
          ),
        ],
      ),
    );
  }

  Widget _buildCompleteView() {
    return Container(
      padding: const EdgeInsets.all(24),
      decoration: BoxDecoration(
        color: PiccoloTheme.porcelain,
        borderRadius: BorderRadius.circular(Radii.md),
        border: Border.all(color: PiccoloTheme.hairline),
      ),
      child: Column(
        children: [
          const Icon(PiccoloIcons.success, color: PiccoloTheme.success, size: 48),
          const SizedBox(height: 16),
          Text('Piccolo has been installed',
              style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.w600)),
          const SizedBox(height: 8),
          const Text(
            'Remove the USB drive and reboot to start using Piccolo from the internal disk.',
            style: TextStyle(fontSize: 13, color: PiccoloTheme.inkMuted),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 24),
          FilledButton.icon(
            onPressed: () async {
              try {
                await _api.fetchCsrfToken();
                await _api.post('/api/v1/system/reboot');
              } on Object catch (_) {}
            },
            icon: const Icon(PiccoloIcons.restart),
            label: const Text('Reboot Now'),
            style: FilledButton.styleFrom(
              padding: const EdgeInsets.symmetric(horizontal: 32, vertical: 16),
            ),
          ),
        ],
      ),
    );
  }
}

class _DiagnosticLogSection extends StatefulWidget {

  const _DiagnosticLogSection({required this.controller});
  final SettingsController controller;

  @override
  State<_DiagnosticLogSection> createState() => _DiagnosticLogSectionState();
}

class _DiagnosticLogSectionState extends State<_DiagnosticLogSection> {
  late DateTime _from;
  late DateTime _to;
  // null = collapsed, true = from picker, false = to picker
  bool? _activePicker;

  @override
  void initState() {
    super.initState();
    final now = DateTime.now();
    _to = DateTime(now.year, now.month, now.day);
    // 2-day difference = 3 inclusive calendar dates, matching backend's diagnosticDefaultDays=3
    _from = _to.subtract(const Duration(days: 2));
  }

  void _togglePicker(bool isFrom) {
    setState(() {
      _activePicker = (_activePicker == isFrom) ? null : isFrom;
    });
  }

  void _onDateSelected(DateTime picked) {
    final isFrom = _activePicker ?? false;
    setState(() {
      if (isFrom) {
        _from = picked;
        if (_from.isAfter(_to)) _to = _from;
        // Clamp: max 7 inclusive calendar days = 6-day difference
        if (_to.difference(_from).inDays > 6) {
          _to = _from.add(const Duration(days: 6));
        }
      } else {
        _to = picked;
        if (_to.isBefore(_from)) _from = _to;
        // Clamp: max 7 inclusive calendar days = 6-day difference
        if (_to.difference(_from).inDays > 6) {
          _from = _to.subtract(const Duration(days: 6));
        }
      }
      _activePicker = null;
    });
  }

  String _formatDate(DateTime d) =>
      '${d.year}-${d.month.toString().padLeft(2, '0')}-${d.day.toString().padLeft(2, '0')}';

  /// Clamp [d] into [earliest..latest] so CalendarDatePicker never asserts.
  DateTime _clampDate(DateTime d, DateTime earliest, DateTime latest) {
    if (d.isBefore(earliest)) return earliest;
    if (d.isAfter(latest)) return latest;
    return d;
  }

  @override
  Widget build(BuildContext context) {
    final now = DateTime.now();
    final today = DateTime(now.year, now.month, now.day);
    final firstDate = today.subtract(const Duration(days: 90));

    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Row(
          children: [
            Expanded(
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Text('Diagnostic Log',
                      style: PiccoloTheme.textTheme.bodyMedium
                          ?.copyWith(fontWeight: FontWeight.bold)),
                  const SizedBox(height: 4),
                  Text(
                    'Download a redacted system log for bug reporting.',
                    style: PiccoloTheme.textTheme.labelSmall,
                  ),
                ],
              ),
            ),
            OutlinedButton.icon(
              onPressed: () => widget.controller
                  .downloadDiagnosticLog(from: _from, to: _to),
              icon: const Icon(PiccoloIcons.download, size: 18),
              label: const Text('Download'),
            ),
          ],
        ),
        const SizedBox(height: 12),
        Row(
          children: [
            _DateChip(
              label: 'From',
              value: _formatDate(_from),
              isActive: _activePicker ?? false,
              onTap: () => _togglePicker(true),
            ),
            const Padding(
              padding: EdgeInsets.symmetric(horizontal: 8),
              child: Text('\u2014', style: TextStyle(color: PiccoloTheme.inkMuted)),
            ),
            _DateChip(
              label: 'To',
              value: _formatDate(_to),
              isActive: _activePicker == false,
              onTap: () => _togglePicker(false),
            ),
            const SizedBox(width: 8),
            Text('(max 7 days)', style: PiccoloTheme.textTheme.labelSmall),
          ],
        ),
        AnimatedSize(
          duration: Motion.medium,
          curve: Curves.easeInOut,
          alignment: Alignment.topLeft,
          child: _activePicker != null
              ? Align(
                  alignment: Alignment.centerLeft,
                  child: ConstrainedBox(
                    constraints: const BoxConstraints(maxWidth: 320),
                    child: CalendarDatePicker(
                      key: ValueKey(_activePicker),
                      initialDate: _clampDate(
                        _activePicker ?? false ? _from : _to,
                        firstDate,
                        today,
                      ),
                      firstDate: firstDate,
                      lastDate: today,
                      onDateChanged: _onDateSelected,
                    ),
                  ),
                )
              : const SizedBox.shrink(),
        ),
      ],
    );
  }
}

class _DateChip extends StatelessWidget {

  const _DateChip({
    required this.label,
    required this.value,
    required this.isActive,
    required this.onTap,
  });
  final String label;
  final String value;
  final bool isActive;
  final VoidCallback onTap;

  @override
  Widget build(BuildContext context) {
    return InkWell(
      borderRadius: BorderRadius.circular(Radii.sm),
      onTap: onTap,
      child: Container(
        padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 8),
        decoration: BoxDecoration(
          borderRadius: BorderRadius.circular(Radii.sm),
          border: Border.all(
            color: isActive
                ? PiccoloTheme.cobalt600
                : PiccoloTheme.ink.withValues(alpha: 0.1),
          ),
          color: isActive
              ? PiccoloTheme.cobalt600.withValues(alpha: 0.05)
              : null,
        ),
        child: Row(
          mainAxisSize: MainAxisSize.min,
          children: [
            Text('$label: ',
                style: PiccoloTheme.textTheme.labelSmall),
            Text(value,
                style: PiccoloTheme.textTheme.bodyMedium
                    ?.copyWith(fontWeight: FontWeight.w500)),
            const SizedBox(width: 4),
            Icon(
              isActive ? PiccoloIcons.expandLess : PiccoloIcons.calendar,
              size: 14,
              color: isActive ? PiccoloTheme.cobalt600 : PiccoloTheme.inkMuted,
            ),
          ],
        ),
      ),
    );
  }
}

class _SettingsDiskTile extends StatelessWidget {

  const _SettingsDiskTile({
    required this.device,
    required this.model,
    required this.sizeGb,
    required this.transport,
    required this.isSelected,
    required this.onTap,
  });
  final String device;
  final String model;
  final int sizeGb;
  final String transport;
  final bool isSelected;
  final VoidCallback? onTap;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.only(bottom: 8),
      child: InkWell(
        borderRadius: BorderRadius.circular(Radii.sm),
        onTap: onTap,
        child: Container(
          padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 10),
          decoration: BoxDecoration(
            borderRadius: BorderRadius.circular(Radii.sm),
            border: Border.all(
              color: isSelected
                  ? PiccoloTheme.cobalt600
                  : PiccoloTheme.ink.withValues(alpha: 0.1),
              width: isSelected ? 2 : 1,
            ),
            color: isSelected ? PiccoloTheme.cobalt600.withValues(alpha: 0.04) : null,
          ),
          child: Row(
            children: [
              RadioGroup<bool>(
                groupValue: isSelected ? true : null,
                onChanged: (_) => onTap?.call(),
                child: Radio<bool>(
                  value: true,
                  activeColor: PiccoloTheme.cobalt600,
                  enabled: onTap != null,
                ),
              ),
              const SizedBox(width: 8),
              Expanded(
                child: Text(
                  '${model.isNotEmpty ? model : device} (${sizeGb >= 1000 ? "${(sizeGb / 1000).toStringAsFixed(1)} TB" : "$sizeGb GB"} ${transport.toUpperCase()})',
                  style: const TextStyle(fontSize: 14),
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }
}
