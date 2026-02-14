import 'package:flutter/material.dart';
import '../../../../theme/piccolo_theme.dart';
import '../../../../shared/widgets/task_progress_panel.dart';

/// Multi-phase Install to Disk flow:
/// 1. Disk selection
/// 2. Confirmation dialog
/// 3. Progress (download + write via TaskProgressPanel)
/// 4. Complete → reboot prompt (handled by parent via installComplete state)
class InstallDiskStep extends StatefulWidget {
  final List<Map<String, dynamic>> disks;
  final Future<bool> Function(String targetDisk) onStartInstall;
  final VoidCallback onBack;
  final VoidCallback onInstallComplete;
  final String? taskId;
  final String? error;
  final Future<void> Function() onRefreshDisks;

  const InstallDiskStep({
    super.key,
    required this.disks,
    required this.onStartInstall,
    required this.onBack,
    required this.onInstallComplete,
    required this.onRefreshDisks,
    this.taskId,
    this.error,
  });

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
            "Select a disk",
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
              fontWeight: FontWeight.bold,
            ),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 8),
          Text(
            "Choose an internal disk to install Piccolo. All data on the selected disk will be erased.",
            style: TextStyle(fontSize: 13, color: PiccoloTheme.inkMuted),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 20),
          if (widget.error != null) ...[
            Container(
              padding: const EdgeInsets.all(12),
              decoration: BoxDecoration(
                color: PiccoloTheme.critical.withValues(alpha: 0.08),
                borderRadius: BorderRadius.circular(8),
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
                borderRadius: BorderRadius.circular(12),
              ),
              child: Column(
                children: [
                  const Icon(Icons.storage, size: 48, color: PiccoloTheme.inkMuted),
                  const SizedBox(height: 12),
                  const Text(
                    "No internal disks found",
                    style: TextStyle(
                      fontWeight: FontWeight.w600,
                      color: PiccoloTheme.inkMuted,
                    ),
                  ),
                  const SizedBox(height: 4),
                  const Text(
                    "Connect an internal disk and try again.",
                    style: TextStyle(fontSize: 13, color: PiccoloTheme.inkMuted),
                  ),
                  const SizedBox(height: 16),
                  OutlinedButton.icon(
                    onPressed: () async {
                      await widget.onRefreshDisks();
                    },
                    icon: const Icon(Icons.refresh, size: 16),
                    label: const Text("Refresh"),
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
                child: const Text("Back"),
              ),
              const Spacer(),
              ElevatedButton(
                onPressed: (_selectedDisk != null && !_isInstalling)
                    ? _confirmAndInstall
                    : null,
                style: ElevatedButton.styleFrom(
                  backgroundColor: PiccoloTheme.cobalt600,
                  foregroundColor: Colors.white,
                  disabledBackgroundColor: PiccoloTheme.ink.withValues(alpha: 0.1),
                  disabledForegroundColor: PiccoloTheme.ink.withValues(alpha: 0.3),
                  padding: const EdgeInsets.symmetric(horizontal: 32, vertical: 16),
                  shape: RoundedRectangleBorder(
                    borderRadius: BorderRadius.circular(12),
                  ),
                ),
                child: _isInstalling
                    ? const SizedBox(
                        width: 20,
                        height: 20,
                        child: CircularProgressIndicator(
                          color: Colors.white,
                          strokeWidth: 2,
                        ),
                      )
                    : const Text("Install"),
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
    final model = selectedDiskInfo['model'] as String? ?? _selectedDisk!;
    final hasData = selectedDiskInfo['has_data'] as bool? ?? false;

    final confirmed = await showDialog<bool>(
      context: context,
      barrierDismissible: false,
      builder: (context) => AlertDialog(
        title: const Text("Confirm Installation"),
        content: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            if (hasData) ...[
              Container(
                padding: const EdgeInsets.all(12),
                decoration: BoxDecoration(
                  color: PiccoloTheme.critical.withValues(alpha: 0.08),
                  borderRadius: BorderRadius.circular(8),
                  border: Border.all(
                    color: PiccoloTheme.critical.withValues(alpha: 0.2),
                  ),
                ),
                child: const Row(
                  children: [
                    Icon(Icons.warning_amber_rounded,
                        color: PiccoloTheme.critical, size: 20),
                    SizedBox(width: 8),
                    Expanded(
                      child: Text(
                        "This disk contains data that will be permanently destroyed.",
                        style: TextStyle(fontSize: 13, color: PiccoloTheme.critical),
                      ),
                    ),
                  ],
                ),
              ),
              const SizedBox(height: 16),
            ],
            Text(
              "All data on $model ($_selectedDisk) will be erased. "
              "This action cannot be undone.",
            ),
          ],
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.pop(context, false),
            child: const Text("Cancel"),
          ),
          FilledButton(
            onPressed: () => Navigator.pop(context, true),
            style: FilledButton.styleFrom(backgroundColor: PiccoloTheme.critical),
            child: const Text("Erase and Install"),
          ),
        ],
      ),
    );

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
            "Installing Piccolo",
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
              fontWeight: FontWeight.bold,
            ),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 8),
          Text(
            "Do not power off or disconnect the device.",
            style: TextStyle(fontSize: 13, color: PiccoloTheme.inkMuted),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 20),
          TaskProgressPanel(
            taskId: widget.taskId!,
            taskType: "Installation",
            urlPath: '/api/v1/system/install-progress/stream',
            onComplete: widget.onInstallComplete,
          ),
        ],
      ),
    );
  }
}

class _DiskTile extends StatelessWidget {
  final String device;
  final String model;
  final int sizeGb;
  final String transport;
  final bool hasData;
  final bool isSelected;
  final VoidCallback? onTap;

  const _DiskTile({
    required this.device,
    required this.model,
    required this.sizeGb,
    required this.transport,
    required this.hasData,
    required this.isSelected,
    required this.onTap,
  });

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.only(bottom: 8),
      child: Material(
        color: isSelected ? PiccoloTheme.cobalt600.withValues(alpha: 0.06) : Colors.white,
        borderRadius: BorderRadius.circular(12),
        child: InkWell(
          borderRadius: BorderRadius.circular(12),
          onTap: onTap,
          child: Container(
            padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 14),
            decoration: BoxDecoration(
              borderRadius: BorderRadius.circular(12),
              border: Border.all(
                color: isSelected
                    ? PiccoloTheme.cobalt600
                    : PiccoloTheme.ink.withValues(alpha: 0.1),
                width: isSelected ? 2 : 1,
              ),
            ),
            child: Row(
              children: [
                Radio<bool>(
                  value: true,
                  groupValue: isSelected ? true : null,
                  onChanged: onTap != null ? (_) => onTap!() : null,
                  activeColor: PiccoloTheme.cobalt600,
                ),
                const SizedBox(width: 8),
                Icon(
                  transport == 'nvme' ? Icons.flash_on : Icons.storage,
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
                        style: TextStyle(
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
                      "Has data",
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
