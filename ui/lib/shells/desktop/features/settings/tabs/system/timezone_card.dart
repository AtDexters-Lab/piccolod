import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/system_timezone.dart';
import 'package:piccolo_os/shells/desktop/features/settings/settings_controller.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class TimezoneCard extends StatefulWidget {
  const TimezoneCard({required this.controller, super.key});
  final SettingsController controller;

  @override
  State<TimezoneCard> createState() => _TimezoneCardState();
}

class _TimezoneCardState extends State<TimezoneCard> {
  @override
  void initState() {
    super.initState();
    widget.controller.addListener(_onChanged);
    if (widget.controller.timezone == null) {
      unawaited(widget.controller.fetchTimezone());
    }
  }

  @override
  void dispose() {
    widget.controller.removeListener(_onChanged);
    super.dispose();
  }

  void _onChanged() {
    if (mounted) setState(() {});
  }

  Future<void> _change(String? zone) async {
    if (zone == null) return;
    final current = widget.controller.timezone?.timezone;
    if (zone == current) return;
    try {
      await widget.controller.updateTimezone(zone);
    } on Object catch (e) {
      if (!mounted) return;
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(content: Text('Could not update timezone: $e')),
      );
    }
  }

  @override
  Widget build(BuildContext context) {
    final tz = widget.controller.timezone;
    final saving = widget.controller.timezoneSaving;

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
              const Icon(PiccoloIcons.clock,
                  color: PiccoloTheme.cobalt600, size: 20),
              const SizedBox(width: Spacing.sm),
              Text('Timezone',
                  style: PiccoloTheme.textTheme.bodyMedium
                      ?.copyWith(fontWeight: FontWeight.bold)),
            ],
          ),
          const SizedBox(height: Spacing.sm),
          Text(
            'Used for scheduled tasks like overnight system updates. '
            'Your browser timezone is captured the first time you sign in. '
            'To change it later, pick from the list below.',
            style: PiccoloTheme.textTheme.labelSmall,
          ),
          const SizedBox(height: Spacing.lg),
          if (tz == null)
            const Center(child: CircularProgressIndicator())
          else
            _TimezoneRow(timezone: tz, saving: saving, onChange: _change),
        ],
      ),
    );
  }
}

class _TimezoneRow extends StatelessWidget {
  const _TimezoneRow({
    required this.timezone,
    required this.saving,
    required this.onChange,
  });
  final SystemTimezone timezone;
  final bool saving;
  final ValueChanged<String?> onChange;

  @override
  Widget build(BuildContext context) {
    final items = <String>{...commonTimezones, timezone.timezone}.toList()
      ..sort();
    return Row(
      children: [
        const Text('Current zone:'),
        const SizedBox(width: Spacing.base),
        Expanded(
          child: DropdownButton<String>(
            value: timezone.timezone,
            isExpanded: true,
            onChanged: saving ? null : onChange,
            items: [
              for (final z in items)
                DropdownMenuItem(value: z, child: Text(z)),
            ],
          ),
        ),
        if (saving) ...[
          const SizedBox(width: Spacing.base),
          const SizedBox(
            width: 16,
            height: 16,
            child: CircularProgressIndicator(strokeWidth: 2),
          ),
        ],
      ],
    );
  }
}
