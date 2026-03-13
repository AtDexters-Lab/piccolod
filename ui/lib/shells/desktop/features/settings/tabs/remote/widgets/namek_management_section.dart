import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/identity_models.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/remote_controller.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class NamekManagementSection extends StatefulWidget {

  const NamekManagementSection({required this.controller, super.key});
  final RemoteController controller;

  @override
  State<NamekManagementSection> createState() => _NamekManagementSectionState();
}

class _NamekManagementSectionState extends State<NamekManagementSection> {
  final TextEditingController _hostnameCtrl = TextEditingController();
  bool _isEditingHostname = false;

  @override
  void dispose() {
    _hostnameCtrl.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    if (!widget.controller.isNamekAvailable) {
      return const SizedBox.shrink();
    }

    final ids = widget.controller.identityStatus;
    if (ids == null) return const SizedBox.shrink();

    return Container(
      padding: const EdgeInsets.all(Spacing.base),
      decoration: BoxDecoration(
        color: PiccoloTheme.porcelain,
        borderRadius: BorderRadius.circular(Radii.sm),
        boxShadow: Elevation.elev1,
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Row(
            children: [
              const Icon(PiccoloIcons.cloud, size: 20, color: PiccoloTheme.cobalt600),
              const SizedBox(width: Spacing.sm),
              Text('Piccolo Cloud', style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
            ],
          ),
          const SizedBox(height: Spacing.base),
          _buildContent(ids),
        ],
      ),
    );
  }

  Widget _buildContent(IdentityStatus ids) {
    final state = ids.state;

    switch (state) {
      case 'not_enrolled':
        return _buildNotEnrolled();
      case 'active':
        return _buildActive(ids);
      case 'disabled':
        return _buildDisabled();
      default:
        // Suspended or unknown
        return _buildSuspended();
    }
  }

  Widget _buildNotEnrolled() {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text(
          'Enroll your device with Piccolo Cloud for managed remote access with automatic DNS and certificates.',
          style: PiccoloTheme.textTheme.bodyMedium?.copyWith(color: PiccoloTheme.inkMuted),
        ),
        const SizedBox(height: Spacing.base),
        FilledButton(
          onPressed: widget.controller.enrollNamek,
          child: const Text('Enroll'),
        ),
      ],
    );
  }

  Widget _buildActive(IdentityStatus ids) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Row(
          children: [
            const Text('Enabled', style: TextStyle(fontWeight: FontWeight.w600)),
            const Spacer(),
            Switch(
              value: true,
              onChanged: (_) => _confirmDisable(),
            ),
          ],
        ),
        const SizedBox(height: Spacing.sm),
        if (ids.resolvedHostname != null) ...[
          Row(
            children: [
              const Icon(PiccoloIcons.link, size: 14, color: PiccoloTheme.inkMuted),
              const SizedBox(width: Spacing.xs),
              Text(ids.resolvedHostname!, style: PiccoloTheme.textTheme.bodyMedium),
            ],
          ),
          const SizedBox(height: Spacing.base),
        ],
        // Custom hostname editor
        if (!_isEditingHostname)
          TextButton.icon(
            onPressed: () {
              _hostnameCtrl.text = ids.customHostname ?? '';
              setState(() => _isEditingHostname = true);
            },
            icon: const Icon(PiccoloIcons.edit, size: 14),
            label: Text(ids.customHostname != null && ids.customHostname!.isNotEmpty
                ? 'Change custom hostname'
                : 'Set custom hostname'),
          )
        else
          Row(
            children: [
              Expanded(
                child: TextField(
                  controller: _hostnameCtrl,
                  decoration: InputDecoration(
                    labelText: 'Custom hostname',
                    hintText: 'mydevice',
                    suffixText: ids.baseDomain != null ? '.${ids.baseDomain!}' : '',
                    border: const OutlineInputBorder(),
                    isDense: true,
                  ),
                ),
              ),
              const SizedBox(width: Spacing.sm),
              FilledButton(
                onPressed: () async {
                  final hostname = _hostnameCtrl.text.trim();
                  await widget.controller.setNamekHostname(hostname);
                  if (mounted && widget.controller.error == null) {
                    setState(() => _isEditingHostname = false);
                  }
                },
                child: const Text('Save'),
              ),
              const SizedBox(width: Spacing.xs),
              TextButton(
                onPressed: () => setState(() => _isEditingHostname = false),
                child: const Text('Cancel'),
              ),
            ],
          ),
      ],
    );
  }

  Widget _buildDisabled() {
    return Row(
      children: [
        Expanded(
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              const Text('Currently disabled', style: TextStyle(color: PiccoloTheme.inkMuted)),
              const SizedBox(height: Spacing.xs),
              Text('Toggle to re-enable managed remote access.',
                  style: PiccoloTheme.textTheme.labelSmall),
            ],
          ),
        ),
        Switch(
          value: false,
          onChanged: (_) => unawaited(widget.controller.enableNamek()),
        ),
      ],
    );
  }

  Widget _buildSuspended() {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Container(
          padding: const EdgeInsets.all(Spacing.sm),
          decoration: BoxDecoration(
            color: PiccoloTheme.warning.withValues(alpha: 0.1),
            borderRadius: BorderRadius.circular(Radii.sm),
            border: Border.all(color: PiccoloTheme.warning.withValues(alpha: 0.3)),
          ),
          child: const Row(
            children: [
              Icon(PiccoloIcons.warning, size: 16, color: PiccoloTheme.warning),
              SizedBox(width: Spacing.sm),
              Expanded(child: Text('Your device identity has been suspended by the server.')),
            ],
          ),
        ),
        const SizedBox(height: Spacing.base),
        FilledButton(
          onPressed: widget.controller.enrollNamek,
          child: const Text('Re-enroll'),
        ),
      ],
    );
  }

  void _confirmDisable() {
    unawaited(showDialog<void>(
      context: context,
      builder: (dialogContext) => AlertDialog(
        title: const Text('Disable Managed Remote Access?'),
        content: const Text('This will disable Piccolo Cloud remote access. Self-hosted relay connections are unaffected.'),
        actions: [
          TextButton(onPressed: () => Navigator.pop(dialogContext), child: const Text('Cancel')),
          FilledButton(
            style: FilledButton.styleFrom(backgroundColor: PiccoloTheme.critical),
            onPressed: () async {
              Navigator.pop(dialogContext);
              await widget.controller.disableNamek();
            },
            child: const Text('Disable'),
          ),
        ],
      ),
    ));
  }
}
