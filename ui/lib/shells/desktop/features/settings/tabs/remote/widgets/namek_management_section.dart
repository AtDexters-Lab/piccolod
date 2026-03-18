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
  _EditMode? _editMode;

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
          _buildBody(ids),
        ],
      ),
    );
  }

  // ── Header ──────────────────────────────────────────────────────────────

  Widget _buildHeader(IdentityStatus ids) {
    final showToggle = ids.state == 'active' || ids.state == 'disabled';

    return Row(
      children: [
        const Icon(PiccoloIcons.planet, size: 20, color: PiccoloTheme.cobalt600),
        const SizedBox(width: Spacing.sm),
        Text('piccolospace', style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
        const Spacer(),
        if (showToggle)
          Switch(
            value: ids.state == 'active',
            onChanged: (_) => ids.state == 'active'
                ? _showDisableConfirmation()
                : unawaited(widget.controller.enableNamek()),
          ),
      ],
    );
  }

  // ── Body (state dispatch) ───────────────────────────────────────────────

  Widget _buildBody(IdentityStatus ids) {
    // Clear edit mode if state transitioned to one that doesn't support editing
    // (e.g., server pushed a suspension while editor was open).
    if (_editMode != null && ids.state == 'suspended') {
      _editMode = null;
    }
    if (_editMode != null) return _buildEditor(ids);

    switch (ids.state) {
      case 'not_enrolled':
        return _buildNotEnrolled(ids);
      case 'active':
        return _buildActive(ids);
      case 'disabled':
        return _buildDisabled(ids);
      default:
        return _buildSuspended();
    }
  }

  // ── Not Enrolled ────────────────────────────────────────────────────────

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
        _serverUrlLine(ids),
      ],
    );
  }

  // ── Active ──────────────────────────────────────────────────────────────

  Widget _buildActive(IdentityStatus ids) {
    final hostname = ids.resolvedHostname;

    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        // Hostname — hero element
        if (hostname != null)
          Row(
            children: [
              Expanded(
                child: SelectableText(
                  hostname,
                  style: PiccoloTheme.textTheme.bodyMedium?.copyWith(fontWeight: FontWeight.w500),
                ),
              ),
              IconButton(
                onPressed: () => _copyToClipboard(hostname),
                icon: const Icon(PiccoloIcons.copy, size: 16),
                tooltip: 'Copy hostname',
                iconSize: 16,
                padding: const EdgeInsets.all(Spacing.xs),
                constraints: const BoxConstraints(),
              ),
            ],
          ),
        const SizedBox(height: Spacing.sm),

        // Server URL — context
        _serverUrlLine(ids),
        const SizedBox(height: Spacing.sm),

        // Action links — compact row
        Wrap(
          spacing: Spacing.base,
          runSpacing: Spacing.xs,
          children: [
            _actionLink(
              label: ids.customHostname != null && ids.customHostname!.isNotEmpty
                  ? 'Change hostname'
                  : 'Set custom hostname',
              icon: PiccoloIcons.edit,
              onPressed: () {
                _hostnameCtrl.text = ids.customHostname ?? '';
                setState(() => _editMode = _EditMode.hostname);
              },
            ),
            _actionLink(
              label: 'Re-enroll',
              icon: PiccoloIcons.refresh,
              onPressed: _showReenrollConfirmation,
            ),
            _actionLink(
              label: 'Change server',
              icon: PiccoloIcons.settings,
              onPressed: () {
                _urlCtrl.text = ids.namekUrl ?? '';
                setState(() => _editMode = _EditMode.serverUrl);
              },
            ),
          ],
        ),
      ],
    );
  }

  // ── Disabled ────────────────────────────────────────────────────────────

  Widget _buildDisabled(IdentityStatus ids) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text(
          'Managed remote access is disabled.',
          style: PiccoloTheme.textTheme.bodyMedium?.copyWith(color: PiccoloTheme.inkMuted),
        ),
        if (ids.hostname != null && ids.hostname!.isNotEmpty) ...[
          const SizedBox(height: Spacing.xs),
          Text(
            ids.hostname!,
            style: PiccoloTheme.textTheme.bodySmall?.copyWith(color: PiccoloTheme.inkMuted),
          ),
        ],
        const SizedBox(height: Spacing.sm),
        _serverUrlLine(ids),
      ],
    );
  }

  // ── Suspended ───────────────────────────────────────────────────────────

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
              Expanded(child: Text('Device identity suspended by the server.')),
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

  // ── Inline Editor (shared for hostname + server URL) ────────────────────

  Widget _buildEditor(IdentityStatus ids) {
    final isHostname = _editMode == _EditMode.hostname;
    final controller = isHostname ? _hostnameCtrl : _urlCtrl;
    final label = isHostname ? 'Custom hostname' : 'Server URL';
    final hint = isHostname ? 'mydevice' : 'https://namek.example.com';
    final suffix = isHostname && ids.baseDomain != null ? '.${ids.baseDomain!}' : null;

    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Row(
          children: [
            Expanded(
              child: TextField(
                controller: controller,
                decoration: InputDecoration(
                  labelText: label,
                  hintText: hint,
                  suffixText: suffix,
                  border: const OutlineInputBorder(),
                  isDense: true,
                ),
                autofocus: true,
              ),
            ),
            const SizedBox(width: Spacing.sm),
            FilledButton(
              onPressed: () => _saveEdit(ids),
              child: const Text('Save'),
            ),
            const SizedBox(width: Spacing.xs),
            TextButton(
              onPressed: _cancelEdit,
              child: const Text('Cancel'),
            ),
          ],
        ),
        if (!isHostname) ...[
          const SizedBox(height: Spacing.xs),
          Text(
            'Changing the server will clear your current enrollment.',
            style: PiccoloTheme.textTheme.bodySmall?.copyWith(color: PiccoloTheme.warning),
          ),
        ],
      ],
    );
  }

  // ── Shared Widgets ──────────────────────────────────────────────────────

  Widget _serverUrlLine(IdentityStatus ids) {
    final url = ids.namekUrl ?? '';
    return Row(
      children: [
        Text('Server: ', style: PiccoloTheme.textTheme.bodySmall?.copyWith(color: PiccoloTheme.inkMuted)),
        Flexible(
          child: Text(
            url,
            style: PiccoloTheme.textTheme.bodySmall?.copyWith(color: PiccoloTheme.inkMuted),
            overflow: TextOverflow.ellipsis,
          ),
        ),
        // "Change" shown only in not_enrolled state; active state uses action row
        if (ids.state == 'not_enrolled' || ids.state == 'disabled') ...[
          const SizedBox(width: Spacing.sm),
          _actionLink(
            label: 'Change',
            onPressed: () {
              _urlCtrl.text = url;
              setState(() => _editMode = _EditMode.serverUrl);
            },
          ),
        ],
      ],
    );
  }

  Widget _actionLink({required String label, required VoidCallback onPressed, IconData? icon}) {
    if (icon != null) {
      return TextButton.icon(
        onPressed: onPressed,
        icon: Icon(icon, size: 14),
        label: Text(label),
        style: TextButton.styleFrom(
          padding: const EdgeInsets.symmetric(horizontal: Spacing.sm, vertical: Spacing.xs),
          minimumSize: Size.zero,
          tapTargetSize: MaterialTapTargetSize.shrinkWrap,
        ),
      );
    }
    return TextButton(
      onPressed: onPressed,
      style: TextButton.styleFrom(
        padding: const EdgeInsets.symmetric(horizontal: Spacing.sm, vertical: Spacing.xs),
        minimumSize: Size.zero,
        tapTargetSize: MaterialTapTargetSize.shrinkWrap,
      ),
      child: Text(label),
    );
  }

  // ── Actions ─────────────────────────────────────────────────────────────

  void _copyToClipboard(String text) {
    unawaited(Clipboard.setData(ClipboardData(text: text)));
    ScaffoldMessenger.of(context).showSnackBar(
      const SnackBar(content: Text('Copied'), duration: Duration(seconds: 2)),
    );
  }

  Future<void> _saveEdit(IdentityStatus ids) async {
    if (_editMode == _EditMode.hostname) {
      await widget.controller.setNamekHostname(_hostnameCtrl.text.trim());
    } else {
      final url = _urlCtrl.text.trim();
      if (url.isEmpty) return;
      await widget.controller.setNamekUrl(url);
    }
    if (mounted && widget.controller.error == null) {
      setState(() => _editMode = null);
    }
  }

  void _cancelEdit() => setState(() => _editMode = null);

  void _showReenrollConfirmation() {
    unawaited(showDialog<void>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: const Text('Re-enroll Device?'),
        content: const Text('This will refresh your enrollment with the server and update relay endpoints.'),
        actions: [
          TextButton(onPressed: () => Navigator.pop(ctx), child: const Text('Cancel')),
          FilledButton(
            onPressed: () async {
              Navigator.pop(ctx);
              await widget.controller.enrollNamek();
            },
            child: const Text('Re-enroll'),
          ),
        ],
      ),
    ));
  }

  void _showDisableConfirmation() {
    unawaited(showDialog<void>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: const Text('Disable Managed Remote Access?'),
        content: const Text('This will disable piccolospace remote access. Self-hosted relay connections are unaffected.'),
        actions: [
          TextButton(onPressed: () => Navigator.pop(ctx), child: const Text('Cancel')),
          FilledButton(
            style: FilledButton.styleFrom(backgroundColor: PiccoloTheme.critical),
            onPressed: () async {
              Navigator.pop(ctx);
              await widget.controller.disableNamek();
            },
            child: const Text('Disable'),
          ),
        ],
      ),
    ));
  }
}

enum _EditMode { hostname, serverUrl }
