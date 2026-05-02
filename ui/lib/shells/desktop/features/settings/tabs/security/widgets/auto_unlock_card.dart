import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/auto_unlock.dart';
import 'package:piccolo_os/shells/desktop/features/settings/settings_controller.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class AutoUnlockCard extends StatefulWidget {
  const AutoUnlockCard({required this.controller, super.key});
  final SettingsController controller;

  @override
  State<AutoUnlockCard> createState() => _AutoUnlockCardState();
}

class _AutoUnlockCardState extends State<AutoUnlockCard> {
  @override
  void initState() {
    super.initState();
    widget.controller.addListener(_onControllerChanged);
    if (widget.controller.autoUnlock == null) {
      unawaited(widget.controller.fetchAutoUnlock());
    }
  }

  @override
  void dispose() {
    widget.controller.removeListener(_onControllerChanged);
    super.dispose();
  }

  void _onControllerChanged() {
    if (mounted) setState(() {});
  }

  Future<void> _toggleEnabled(bool value) async {
    try {
      await widget.controller.updateAutoUnlock(enabled: value);
    } on Object catch (e) {
      if (!mounted) return;
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(content: Text('Could not update auto-unlock: $e')),
      );
    }
  }

  Future<void> _toggleAutoReboot(bool value) async {
    try {
      await widget.controller.updateAutoUnlock(autoRebootEnabled: value);
    } on Object catch (e) {
      if (!mounted) return;
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(content: Text('Could not update auto-reboot: $e')),
      );
    }
  }

  Future<void> _setWindow(int start, int end) async {
    try {
      await widget.controller.updateAutoUnlock(
        windowStartHour: start,
        windowEndHour: end,
      );
    } on Object catch (e) {
      if (!mounted) return;
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(content: Text('Could not update window: $e')),
      );
    }
  }

  @override
  Widget build(BuildContext context) {
    final state = widget.controller.autoUnlock;
    final saving = widget.controller.autoUnlockSaving;

    return Container(
      padding: const EdgeInsets.all(Spacing.lg),
      decoration: BoxDecoration(
        color: PiccoloTheme.porcelain,
        borderRadius: BorderRadius.circular(Radii.md),
        border: Border.all(color: PiccoloTheme.hairline),
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Row(
            children: [
              const Icon(PiccoloIcons.lockKey,
                  color: PiccoloTheme.cobalt600, size: 20),
              const SizedBox(width: Spacing.sm),
              Expanded(
                child: Text('Auto-unlock after reboot',
                    style: PiccoloTheme.textTheme.bodyMedium
                        ?.copyWith(fontWeight: FontWeight.bold)),
              ),
              if (state != null)
                Switch(
                  value: state.enabled,
                  onChanged: saving ? null : _toggleEnabled,
                ),
            ],
          ),
          const SizedBox(height: Spacing.sm),
          Text(
            'Restore your encrypted disk automatically when the device reboots, '
            'so apps come back online without you entering the password. The '
            'unlock key is held encrypted by piccolospace; this means the '
            'device boots without a password if it is powered on by anyone '
            'with physical access.',
            style: PiccoloTheme.textTheme.labelSmall,
          ),
          if (state == null) ...[
            const SizedBox(height: Spacing.lg),
            const Center(child: CircularProgressIndicator()),
          ] else if (state.enabled) ...[
            const SizedBox(height: Spacing.lg),
            _StatusRow(state: state),
            const SizedBox(height: Spacing.lg),
            const Divider(height: 1),
            const SizedBox(height: Spacing.lg),
            _AutoRebootSection(
              state: state,
              saving: saving,
              onToggle: _toggleAutoReboot,
              onWindowChanged: _setWindow,
            ),
            const SizedBox(height: Spacing.lg),
            _TestActionRow(controller: widget.controller),
          ],
        ],
      ),
    );
  }
}

class _StatusRow extends StatelessWidget {
  const _StatusRow({required this.state});
  final AutoUnlockState state;

  @override
  Widget build(BuildContext context) {
    final lastSuccess = state.lastPickupAt ?? state.lastDepositAt;
    final reason = state.lastFailureReason;
    // Hide the failure surface entirely for benign tokens (e.g. the
    // operator-typed-password-first race). Those aren't failures the
    // operator should worry about.
    final showFailure = state.isInFailureState &&
        reason.isNotEmpty &&
        !isAutoUnlockBenignToken(reason);
    return Wrap(
      spacing: Spacing.lg,
      runSpacing: Spacing.sm,
      children: [
        // When showing a failure reason, collapse Status + Reason into the
        // single Reason badge — the doubled warning copy was Gestalt noise.
        if (!showFailure)
          const _StatusBadge(
            label: 'Status',
            value: 'Active',
            color: PiccoloTheme.success,
          ),
        if (lastSuccess != null)
          _StatusBadge(
            label: 'Last successful cycle',
            value: _formatRelative(lastSuccess),
            color: PiccoloTheme.inkMuted,
          ),
        if (showFailure)
          _StatusBadge(
            label: 'Last attempt',
            value: 'Failed — ${autoUnlockFailureReasonLabel(reason)}',
            color: PiccoloTheme.warning,
          ),
      ],
    );
  }
}

class _StatusBadge extends StatelessWidget {
  const _StatusBadge({
    required this.label,
    required this.value,
    required this.color,
  });
  final String label;
  final String value;
  final Color color;

  @override
  Widget build(BuildContext context) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      mainAxisSize: MainAxisSize.min,
      children: [
        Text(label, style: PiccoloTheme.textTheme.labelSmall),
        const SizedBox(height: 2),
        Text(value,
            style: PiccoloTheme.textTheme.bodySmall
                ?.copyWith(color: color, fontWeight: FontWeight.w600)),
      ],
    );
  }
}

class _AutoRebootSection extends StatelessWidget {
  const _AutoRebootSection({
    required this.state,
    required this.saving,
    required this.onToggle,
    required this.onWindowChanged,
  });
  final AutoUnlockState state;
  final bool saving;
  final ValueChanged<bool> onToggle;
  final void Function(int start, int end) onWindowChanged;

  @override
  Widget build(BuildContext context) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Row(
          children: [
            Expanded(
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Text('Apply system updates overnight',
                      style: PiccoloTheme.textTheme.bodyMedium
                          ?.copyWith(fontWeight: FontWeight.w600)),
                  const SizedBox(height: 2),
                  Text(
                    'Reboots automatically inside the maintenance window when '
                    'a system update is staged.',
                    style: PiccoloTheme.textTheme.labelSmall,
                  ),
                ],
              ),
            ),
            Switch(
              value: state.autoReboot.enabled,
              onChanged: saving ? null : onToggle,
            ),
          ],
        ),
        if (state.autoReboot.enabled) ...[
          const SizedBox(height: Spacing.base),
          _WindowPicker(
            startHour: state.autoReboot.windowStartHour,
            endHour: state.autoReboot.windowEndHour,
            saving: saving,
            onChanged: onWindowChanged,
          ),
        ],
      ],
    );
  }
}

class _WindowPicker extends StatelessWidget {
  const _WindowPicker({
    required this.startHour,
    required this.endHour,
    required this.saving,
    required this.onChanged,
  });
  final int startHour;
  final int endHour;
  final bool saving;
  final void Function(int start, int end) onChanged;

  @override
  Widget build(BuildContext context) {
    return Row(
      children: [
        Text('Window:', style: PiccoloTheme.textTheme.labelSmall),
        const SizedBox(width: Spacing.sm),
        _HourDropdown(
          value: startHour,
          disabledHour: endHour,
          saving: saving,
          onChanged: (h) {
            if (h != null && h != endHour) onChanged(h, endHour);
          },
        ),
        const SizedBox(width: Spacing.sm),
        const Text('to'),
        const SizedBox(width: Spacing.sm),
        _HourDropdown(
          value: endHour,
          disabledHour: startHour,
          saving: saving,
          onChanged: (h) {
            if (h != null && h != startHour) onChanged(startHour, h);
          },
        ),
        const SizedBox(width: Spacing.sm),
        Text('local time',
            style: PiccoloTheme.textTheme.labelSmall
                ?.copyWith(color: PiccoloTheme.inkMuted)),
      ],
    );
  }
}

class _HourDropdown extends StatelessWidget {
  const _HourDropdown({
    required this.value,
    required this.disabledHour,
    required this.saving,
    required this.onChanged,
  });
  final int value;
  // Hour to grey out — the other end of the window. Surfaces the
  // start != end constraint to the user instead of silently dropping
  // same-hour selections in the change handler.
  final int disabledHour;
  final bool saving;
  final ValueChanged<int?> onChanged;

  @override
  Widget build(BuildContext context) {
    return DropdownButton<int>(
      value: value,
      onChanged: saving ? null : onChanged,
      items: [
        for (var h = 0; h < 24; h++)
          DropdownMenuItem(
            value: h,
            enabled: h != disabledHour,
            child: Text(
              _formatHour(h),
              style: h == disabledHour
                  ? const TextStyle(color: PiccoloTheme.inkMuted)
                  : null,
            ),
          ),
      ],
    );
  }
}

class _TestActionRow extends StatelessWidget {
  const _TestActionRow({required this.controller});
  final SettingsController controller;

  @override
  Widget build(BuildContext context) {
    final testing = controller.autoUnlockTesting;
    final result = controller.autoUnlockTestResult;
    final latency = controller.autoUnlockTestLatencyMs;

    Widget? feedback;
    if (result != null) {
      if (result.isEmpty) {
        // Surface latency only when notably high — bare millisecond counts
        // are debug noise without a benchmark the operator can interpret.
        final slow = latency != null && latency > 2000;
        feedback = Row(
          children: [
            const Icon(PiccoloIcons.success,
                color: PiccoloTheme.success, size: 16),
            const SizedBox(width: Spacing.xs),
            Text(
              slow
                  ? 'Connection healthy but slow (${(latency / 1000).toStringAsFixed(1)}s)'
                  : 'Connection healthy',
              style: PiccoloTheme.textTheme.labelSmall
                  ?.copyWith(color: PiccoloTheme.success),
            ),
          ],
        );
      } else {
        feedback = Row(
          children: [
            const Icon(PiccoloIcons.warning,
                color: PiccoloTheme.warning, size: 16),
            const SizedBox(width: Spacing.xs),
            Expanded(
              child: Text(
                'Test failed: ${autoUnlockFailureReasonLabel(result)}',
                style: PiccoloTheme.textTheme.labelSmall
                    ?.copyWith(color: PiccoloTheme.warning),
              ),
            ),
          ],
        );
      }
    }

    return Row(
      children: [
        OutlinedButton.icon(
          onPressed: testing ? null : controller.runAutoUnlockTest,
          icon: testing
              ? const SizedBox(
                  width: 16,
                  height: 16,
                  child: CircularProgressIndicator(strokeWidth: 2),
                )
              : const Icon(PiccoloIcons.sync, size: 16),
          label: Text(testing ? 'Testing…' : 'Test connection'),
        ),
        if (feedback != null) ...[
          const SizedBox(width: Spacing.base),
          Expanded(child: feedback),
        ],
      ],
    );
  }
}

String _formatHour(int h) {
  final period = h < 12 ? 'AM' : 'PM';
  final hour12 = h == 0 ? 12 : (h > 12 ? h - 12 : h);
  return '$hour12:00 $period';
}

String _formatRelative(DateTime dt) {
  final diff = DateTime.now().difference(dt);
  if (diff.inSeconds < 60) return 'just now';
  if (diff.inMinutes < 60) return '${diff.inMinutes} min ago';
  if (diff.inHours < 24) return '${diff.inHours} h ago';
  if (diff.inDays < 7) return '${diff.inDays} d ago';
  return '${dt.year}-${dt.month.toString().padLeft(2, '0')}-${dt.day.toString().padLeft(2, '0')}';
}
