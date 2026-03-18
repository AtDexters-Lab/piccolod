import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
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
  final TextEditingController _urlCtrl = TextEditingController();
  bool _isEditingHostname = false;
  bool _isEditingUrl = false;

  @override
  void dispose() {
    _hostnameCtrl.dispose();
    _urlCtrl.dispose();
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
          _buildHeader(ids),
          const SizedBox(height: Spacing.base),
          _buildContent(ids),
        ],
      ),
    );
  }

  Widget _buildHeader(IdentityStatus ids) {
    final isActive = ids.state == 'active';
    final isDisabled = ids.state == 'disabled';

    return Row(
      children: [
        const Icon(PiccoloIcons.planet, size: 20, color: PiccoloTheme.cobalt600),
        const SizedBox(width: Spacing.sm),
        Text('piccolospace', style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
        const Spacer(),
        if (isActive || isDisabled)
          Switch(
            value: isActive,
            onChanged: (_) => isActive ? _confirmDisable() : unawaited(widget.controller.enableNamek()),
          ),
      ],
    );
  }

  Widget _buildContent(IdentityStatus ids) {
    switch (ids.state) {
      case 'not_enrolled':
        return _buildNotEnrolled(ids);
      case 'active':
        return _buildActive(ids);
      case 'disabled':
        return _buildDisabled();
      default:
        return _buildSuspended();
    }
  }

  Widget _buildNotEnrolled(IdentityStatus ids) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text(
          'Managed remote access with automatic DNS and certificates.',
          style: PiccoloTheme.textTheme.bodyMedium?.copyWith(color: PiccoloTheme.inkMuted),
        ),
        const SizedBox(height: Spacing.base),
        FilledButton(
          onPressed: widget.controller.enrollNamek,
          child: const Text('Enroll'),
        ),
        const SizedBox(height: Spacing.base),
        _buildServerUrl(ids),
      ],
    );
  }

  Widget _buildServerUrl(IdentityStatus ids) {
    final currentUrl = ids.namekUrl ?? '';

    if (!_isEditingUrl) {
      return Row(
        children: [
          Text('Server: ', style: PiccoloTheme.textTheme.bodySmall?.copyWith(color: PiccoloTheme.inkMuted)),
          Flexible(
            child: Text(
              currentUrl,
              style: PiccoloTheme.textTheme.bodySmall?.copyWith(color: PiccoloTheme.inkMuted),
              overflow: TextOverflow.ellipsis,
            ),
          ),
          const SizedBox(width: Spacing.sm),
          TextButton(
            onPressed: () {
              _urlCtrl.text = currentUrl;
              setState(() => _isEditingUrl = true);
            },
            style: TextButton.styleFrom(
              padding: const EdgeInsets.symmetric(horizontal: Spacing.sm),
              minimumSize: Size.zero,
              tapTargetSize: MaterialTapTargetSize.shrinkWrap,
            ),
            child: const Text('Change'),
          ),
        ],
      );
    }

    return Row(
      children: [
        Expanded(
          child: TextField(
            controller: _urlCtrl,
            decoration: const InputDecoration(
              labelText: 'Server URL',
              hintText: 'https://namek.example.com',
              border: OutlineInputBorder(),
              isDense: true,
            ),
          ),
        ),
        const SizedBox(width: Spacing.sm),
        FilledButton(
          onPressed: () async {
            final url = _urlCtrl.text.trim();
            if (url.isEmpty) return;
            await widget.controller.setNamekUrl(url);
            if (mounted && widget.controller.error == null) {
              setState(() => _isEditingUrl = false);
            }
          },
          child: const Text('Save'),
        ),
        const SizedBox(width: Spacing.xs),
        TextButton(
          onPressed: () => setState(() => _isEditingUrl = false),
          child: const Text('Cancel'),
        ),
      ],
    );
  }

  Widget _buildActive(IdentityStatus ids) {
    final hostname = ids.resolvedHostname;

    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        if (hostname != null) ...[
          Row(
            children: [
              Expanded(
                child: SelectableText(
                  hostname,
                  style: PiccoloTheme.textTheme.bodyMedium?.copyWith(fontWeight: FontWeight.w500),
                ),
              ),
              IconButton(
                onPressed: () {
                  unawaited(Clipboard.setData(ClipboardData(text: hostname)));
                  ScaffoldMessenger.of(context).showSnackBar(
                    const SnackBar(content: Text('Hostname copied'), duration: Duration(seconds: 2)),
                  );
                },
                icon: const Icon(PiccoloIcons.copy, size: 16),
                tooltip: 'Copy hostname',
                iconSize: 16,
                padding: const EdgeInsets.all(Spacing.xs),
                constraints: const BoxConstraints(),
              ),
            ],
          ),
          const SizedBox(height: Spacing.sm),
        ],
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
    return Text(
      'Managed remote access is disabled. Toggle to re-enable.',
      style: PiccoloTheme.textTheme.bodyMedium?.copyWith(color: PiccoloTheme.inkMuted),
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
        content: const Text('This will disable piccolospace remote access. Self-hosted relay connections are unaffected.'),
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
