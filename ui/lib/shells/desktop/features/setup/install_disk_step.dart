import 'package:flutter/material.dart';
import 'package:piccolo_os/shared/widgets/task_progress_panel.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Multi-phase Install to Disk flow:
/// 1. Disk selection
/// 2. Confirmation dialog
/// 3. Progress (download + write via TaskProgressPanel)
/// 4. Complete → reboot prompt (handled by parent via installComplete state)
class InstallDiskStep extends StatefulWidget {

  const InstallDiskStep({
    required this.disks, required this.onStartInstall, required this.onBack, required this.onInstallComplete, required this.onRefreshDisks, super.key,
    this.taskId,
    this.error,
  });
  final List<Map<String, dynamic>> disks;
  final Future<bool> Function(String targetDisk) onStartInstall;
  final VoidCallback onBack;
  final VoidCallback onInstallComplete;
  final String? taskId;
  final String? error;
  final Future<void> Function() onRefreshDisks;

  @override
  State<InstallDiskStep> createState() => _InstallDiskStepState();
}

class _InstallDiskStepState extends State<InstallDiskStep> {
  String? _selectedDisk;
  bool _isInstalling = false;

  @override
  Widget build(BuildContext context) {
    // If we have an active task, show the progress panel.
    if (widget.taskId != null) {
      return _buildProgressView();
    }
    return _buildDiskSelectionView();
  }

  Widget _buildDiskSelectionView() {
    final disks = widget.disks;

    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          Text(
            'Select a disk',
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
              fontWeight: FontWeight.bold,
            ),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 8),
          const Text(
            'Choose an internal disk to install Piccolo. All data on the selected disk will be erased.',
            style: TextStyle(fontSize: 13, color: PiccoloTheme.inkMuted),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 20),
          if (widget.error != null) ...[
            Container(
              padding: const EdgeInsets.all(12),
              decoration: BoxDecoration(
                color: PiccoloTheme.critical.withValues(alpha: 0.08),
                borderRadius: BorderRadius.circular(Radii.sm),
                border: Border.all(
                  color: PiccoloTheme.critical.withValues(alpha: 0.2),
                ),
              ),
              child: Text(
                widget.error!,
                style: PiccoloTheme.textTheme.labelMedium?.copyWith(
                  color: PiccoloTheme.critical,
                ),
              ),
            ),
            const SizedBox(height: 12),
          ],
          if (disks.isEmpty) ...[
            Container(
              padding: const EdgeInsets.all(24),
              decoration: BoxDecoration(
                color: PiccoloTheme.mist,
                borderRadius: BorderRadius.circular(Radii.sm),
              ),
              child: Column(
                children: [
                  const Icon(PiccoloIcons.storage, size: 48, color: PiccoloTheme.inkMuted),
                  const SizedBox(height: 12),
                  const Text(
                    'No internal disks found',
                    style: TextStyle(
                      fontWeight: FontWeight.w600,
                      color: PiccoloTheme.inkMuted,
                    ),
                  ),
                  const SizedBox(height: 4),
                  const Text(
                    'Connect an internal disk and try again.',
                    style: TextStyle(fontSize: 13, color: PiccoloTheme.inkMuted),
                  ),
                  const SizedBox(height: 16),
                  OutlinedButton.icon(
                    onPressed: () async {
                      await widget.onRefreshDisks();
                    },
                    icon: const Icon(PiccoloIcons.refresh, size: 16),
                    label: const Text('Refresh'),
                  ),
                ],
              ),
            ),
          ] else ...[
            ...disks.map((disk) => _DiskTile(
                  device: disk['device'] as String? ?? '',
                  model: disk['model'] as String? ?? 'Unknown',
                  sizeGb: disk['size_gb'] as int? ?? 0,
                  transport: disk['transport'] as String? ?? '',
                  hasData: disk['has_data'] as bool? ?? false,
                  isSelected: _selectedDisk == disk['device'],
                  onTap: _isInstalling
                      ? null
                      : () {
                          setState(() => _selectedDisk = disk['device'] as String?);
                        },
                )),
          ],
          const SizedBox(height: 24),
          Row(
            children: [
              TextButton(
                onPressed: _isInstalling ? null : widget.onBack,
                child: const Text('Back'),
              ),
              const Spacer(),
              FilledButton(
                onPressed: (_selectedDisk != null && !_isInstalling)
                    ? _confirmAndInstall
                    : null,
                child: _isInstalling
                    ? const SizedBox(
                        width: 20,
                        height: 20,
                        child: CircularProgressIndicator(
                          color: Colors.white,
                          strokeWidth: 2,
                        ),
                      )
                    : const Text('Install'),
              ),
            ],
          ),
        ],
      ),
    );
  }

  Future<void> _confirmAndInstall() async {
    if (_selectedDisk == null) return;

    final selectedDiskInfo = widget.disks.firstWhere(
      (d) => d['device'] == _selectedDisk,
      orElse: () => {},
    );
    final rawModel = selectedDiskInfo['model'] as String? ?? '';
    final model = rawModel.isNotEmpty ? rawModel : _selectedDisk!;
    final hasData = selectedDiskInfo['has_data'] as bool? ?? false;

    // For has_data disks: type-to-confirm with the disk model name.
    // Controller declared in this scope so it survives StatefulBuilder rebuilds.
    final confirmController = TextEditingController();

    final confirmed = await showDialog<bool>(
      context: context,
      barrierDismissible: false,
      builder: (context) => StatefulBuilder(
        builder: (context, setDialogState) {
          final matches = !hasData ||
              confirmController.text.trim().toLowerCase() ==
                  model.trim().toLowerCase();
          return AlertDialog(
            title: const Text('Confirm Installation'),
            content: Column(
              mainAxisSize: MainAxisSize.min,
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                if (hasData) ...[
                  Container(
                    padding: const EdgeInsets.all(12),
                    decoration: BoxDecoration(
                      color: PiccoloTheme.critical.withValues(alpha: 0.08),
                      borderRadius: BorderRadius.circular(Radii.sm),
                      border: Border.all(
                        color: PiccoloTheme.critical.withValues(alpha: 0.2),
                      ),
                    ),
                    child: const Row(
                      children: [
                        Icon(PiccoloIcons.warning,
                            color: PiccoloTheme.critical, size: 20),
                        SizedBox(width: 8),
                        Expanded(
                          child: Text(
                            'This disk contains data that will be permanently destroyed.',
                            style: TextStyle(
                                fontSize: 13, color: PiccoloTheme.critical),
                          ),
                        ),
                      ],
                    ),
                  ),
                  const SizedBox(height: 16),
                ],
                Text(
                  'All data on $model ($_selectedDisk) will be erased. '
                  'This action cannot be undone.',
                ),
                if (hasData) ...[
                  const SizedBox(height: 16),
                  TextField(
                    controller: confirmController,
                    onChanged: (_) => setDialogState(() {}),
                    decoration: InputDecoration(
                      labelText: 'Type "$model" to confirm',
                      border: OutlineInputBorder(
                        borderRadius: BorderRadius.circular(Radii.sm),
                      ),
                    ),
                  ),
                ],
              ],
            ),
            actions: [
              TextButton(
                onPressed: () => Navigator.pop(context, false),
                child: const Text('Cancel'),
              ),
              FilledButton(
                onPressed:
                    matches ? () => Navigator.pop(context, true) : null,
                style: FilledButton.styleFrom(
                    backgroundColor: PiccoloTheme.critical),
                child: const Text('Erase and Install'),
              ),
            ],
          );
        },
      ),
    );

    confirmController.dispose();

    if (confirmed != true || !mounted) return;

    setState(() => _isInstalling = true);
    final success = await widget.onStartInstall(_selectedDisk!);
    if (mounted && !success) {
      setState(() => _isInstalling = false);
    }
  }

  Widget _buildProgressView() {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          Text(
            'Installing Piccolo',
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
              fontWeight: FontWeight.bold,
            ),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 8),
          const Text(
            'Do not power off or disconnect the device.',
            style: TextStyle(fontSize: 13, color: PiccoloTheme.inkMuted),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 20),
          TaskProgressPanel(
            taskId: widget.taskId!,
            taskType: 'Installation',
            urlPath: '/api/v1/system/install-progress/stream',
            onComplete: (evt) {
              if (evt.error == null || evt.error!.isEmpty) {
                widget.onInstallComplete();
              }
            },
          ),
        ],
      ),
    );
  }
}

class _DiskTile extends StatelessWidget {

  const _DiskTile({
    required this.device,
    required this.model,
    required this.sizeGb,
    required this.transport,
    required this.hasData,
    required this.isSelected,
    required this.onTap,
  });
  final String device;
  final String model;
  final int sizeGb;
  final String transport;
  final bool hasData;
  final bool isSelected;
  final VoidCallback? onTap;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.only(bottom: 8),
      child: Material(
        color: isSelected ? PiccoloTheme.cobalt600.withValues(alpha: 0.06) : PiccoloTheme.porcelain,
        borderRadius: BorderRadius.circular(Radii.sm),
        child: InkWell(
          borderRadius: BorderRadius.circular(Radii.sm),
          onTap: onTap,
          child: Container(
            padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 14),
            decoration: BoxDecoration(
              borderRadius: BorderRadius.circular(Radii.sm),
              border: Border.all(
                color: isSelected
                    ? PiccoloTheme.cobalt600
                    : PiccoloTheme.ink.withValues(alpha: 0.1),
                width: isSelected ? 2 : 1,
              ),
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
                Icon(
                  transport == 'nvme' ? PiccoloIcons.lightning : PiccoloIcons.storage,
                  size: 20,
                  color: PiccoloTheme.inkMuted,
                ),
                const SizedBox(width: 12),
                Expanded(
                  child: Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      Text(
                        model.isNotEmpty ? model : device,
                        style: const TextStyle(
                          fontWeight: FontWeight.w500,
                          fontSize: 14,
                        ),
                      ),
                      Text(
                        '${_formatSize(sizeGb)} ${transport.toUpperCase()} $device',
                        style: const TextStyle(
                          fontSize: 12,
                          color: PiccoloTheme.inkMuted,
                        ),
                      ),
                    ],
                  ),
                ),
                if (hasData)
                  Container(
                    padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 3),
                    decoration: BoxDecoration(
                      color: PiccoloTheme.critical.withValues(alpha: 0.08),
                      borderRadius: BorderRadius.circular(6),
                    ),
                    child: const Text(
                      'Has data',
                      style: TextStyle(
                        fontSize: 11,
                        color: PiccoloTheme.critical,
                        fontWeight: FontWeight.w500,
                      ),
                    ),
                  ),
              ],
            ),
          ),
        ),
      ),
    );
  }

  String _formatSize(int gb) {
    if (gb >= 1000) {
      final tb = (gb / 1000).toStringAsFixed(1);
      return '$tb TB';
    }
    return '$gb GB';
  }
}
