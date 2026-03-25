import 'dart:async';

import 'package:flutter/gestures.dart';
import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/remote_models.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/remote_controller.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'package:url_launcher/url_launcher.dart';

class SelfHostedSetupDialog extends StatefulWidget {

  const SelfHostedSetupDialog({required this.controller, super.key});
  final RemoteController controller;

  static void show(BuildContext context, RemoteController controller) {
    unawaited(showDialog<void>(
      context: context,
      builder: (context) => Dialog(
        child: ConstrainedBox(
          constraints: const BoxConstraints(maxWidth: 600, maxHeight: 700),
          child: ClipRRect(
            borderRadius: BorderRadius.circular(Radii.md),
            child: SelfHostedSetupDialog(controller: controller),
          ),
        ),
      ),
    ));
  }

  @override
  State<SelfHostedSetupDialog> createState() => _SelfHostedSetupDialogState();
}

class _SelfHostedSetupDialogState extends State<SelfHostedSetupDialog> {
  final TextEditingController _endpointCtrl = TextEditingController();
  final TextEditingController _portalCtrl = TextEditingController();
  final TextEditingController _secretCtrl = TextEditingController();

  // Dialog-local state
  RemoteGuideInfo? _guideInfo;
  bool _isConnecting = false;
  String? _error;
  TapGestureRecognizer? _docsLinkRecognizer;

  @override
  void initState() {
    super.initState();
    unawaited(_loadGuide());
  }

  Future<void> _loadGuide() async {
    try {
      final guide = await widget.controller.fetchGuideInfo();
      if (mounted) setState(() => _guideInfo = guide);
    } on Object catch (e) {
      if (mounted) setState(() => _error = 'Failed to load setup guide: $e');
    }
  }

  @override
  void dispose() {
    _endpointCtrl.dispose();
    _portalCtrl.dispose();
    _secretCtrl.dispose();
    _docsLinkRecognizer?.dispose();
    super.dispose();
  }

  Future<void> _connect() async {
    if (_isConnecting) return;
    if (_endpointCtrl.text.isEmpty || _secretCtrl.text.isEmpty || _portalCtrl.text.isEmpty) {
      setState(() => _error = 'Please fill all fields.');
      return;
    }
    if (!_portalCtrl.text.contains('.')) {
      setState(() => _error = 'Portal hostname must be a fully-qualified domain (e.g., portal.home.example.com).');
      return;
    }

    setState(() {
      _isConnecting = true;
      _error = null;
    });

    final result = await widget.controller.connectSelfHosted(
      _endpointCtrl.text.trim(),
      _portalCtrl.text.trim(),
      _secretCtrl.text.trim(),
    );

    if (!mounted) return;

    if (result == null) {
      Navigator.of(context).pop();
    } else {
      setState(() {
        _isConnecting = false;
        _error = result;
      });
    }
  }

  @override
  Widget build(BuildContext context) {
    return Material(
      color: PiccoloTheme.porcelain,
      child: SingleChildScrollView(
        padding: const EdgeInsets.all(Spacing.xl),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            // Header
            Row(
              mainAxisAlignment: MainAxisAlignment.spaceBetween,
              children: [
                Text('Self-hosted Relay Setup', style: PiccoloTheme.textTheme.headlineMedium),
                IconButton(
                  onPressed: () => Navigator.of(context).pop(),
                  icon: const Icon(PiccoloIcons.close),
                ),
              ],
            ),
            const SizedBox(height: Spacing.lg),

            // Error banner
            if (_error != null) ...[
              Container(
                padding: const EdgeInsets.all(Spacing.md),
                decoration: BoxDecoration(
                  color: PiccoloTheme.critical.withValues(alpha: 0.1),
                  borderRadius: BorderRadius.circular(Radii.sm),
                  border: Border.all(color: PiccoloTheme.critical.withValues(alpha: 0.3)),
                ),
                child: Row(
                  children: [
                    const Icon(PiccoloIcons.error, size: 16, color: PiccoloTheme.critical),
                    const SizedBox(width: Spacing.sm),
                    Expanded(child: Text(_error!, style: const TextStyle(color: PiccoloTheme.critical, fontSize: 13))),
                  ],
                ),
              ),
              const SizedBox(height: Spacing.lg),
            ],

            // Guide content (or loading spinner)
            if (_guideInfo == null && _error == null)
              const Center(child: CircularProgressIndicator())
            else if (_guideInfo != null)
              ..._buildGuideContent(_guideInfo!),

            // Connection form
            const Divider(),
            const SizedBox(height: Spacing.xl),
            Text('Connection Details', style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
            const SizedBox(height: Spacing.base),
            _buildTextField('Nexus Endpoint', _endpointCtrl, hint: 'wss://nexus.example.com'),
            const SizedBox(height: Spacing.base),
            _buildTextField('Portal Hostname', _portalCtrl, hint: 'portal.home.example.com'),
            Padding(
              padding: const EdgeInsets.only(top: Spacing.sm, bottom: Spacing.sm),
              child: Text(
                'The fully-qualified domain name where this device will be accessible remotely. '
                "App subdomains (e.g., myapp.home.example.com) are derived from this hostname's parent domain.",
                style: PiccoloTheme.textTheme.labelSmall?.copyWith(color: PiccoloTheme.inkMuted),
              ),
            ),
            const SizedBox(height: Spacing.sm),
            _buildTextField('Device Secret', _secretCtrl, obscureText: true),

            const SizedBox(height: Spacing.xl),
            Align(
              alignment: Alignment.centerRight,
              child: FilledButton.icon(
                onPressed: _isConnecting ? null : _connect,
                icon: _isConnecting
                    ? const SizedBox(width: 16, height: 16, child: CircularProgressIndicator(strokeWidth: 2))
                    : const Icon(PiccoloIcons.cloud),
                label: Text(_isConnecting ? 'Connecting...' : 'Connect'),
              ),
            ),
          ],
        ),
      ),
    );
  }

  List<Widget> _buildGuideContent(RemoteGuideInfo guide) {
    return [
      const Text('To enable self-hosted remote access, you need a Nexus Relay running on your VPS.'),
      const SizedBox(height: Spacing.lg),

      if (guide.requirements.isNotEmpty) ...[
        Text('Prerequisites', style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
        const SizedBox(height: Spacing.sm),
        ...guide.requirements.map((req) => Padding(
          padding: const EdgeInsets.only(bottom: Spacing.xs),
          child: Row(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              const Icon(PiccoloIcons.success, size: 16, color: PiccoloTheme.cobalt600),
              const SizedBox(width: Spacing.sm),
              Expanded(child: Text(req)),
            ],
          ),
        )),
        const SizedBox(height: Spacing.lg),
      ],

      Text('Installation', style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
      const SizedBox(height: Spacing.sm),
      Container(
        padding: const EdgeInsets.all(Spacing.base),
        decoration: BoxDecoration(
          color: PiccoloTheme.ink,
          borderRadius: BorderRadius.circular(Radii.sm),
        ),
        width: double.infinity,
        child: SelectableText(
          guide.command,
          style: PiccoloTheme.mono.copyWith(color: PiccoloTheme.success, height: 1.5),
        ),
      ),
      Padding(
        padding: const EdgeInsets.only(top: Spacing.sm),
        child: Text('Run this command on your VPS.', style: PiccoloTheme.textTheme.labelSmall),
      ),
      const SizedBox(height: Spacing.lg),

      if (guide.notes.isNotEmpty) ...[
        Container(
          padding: const EdgeInsets.all(Spacing.md),
          decoration: BoxDecoration(
            color: PiccoloTheme.info.withValues(alpha: 0.1),
            borderRadius: BorderRadius.circular(Radii.sm),
            border: Border.all(color: PiccoloTheme.info.withValues(alpha: 0.3)),
          ),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: guide.notes.map((note) => Padding(
              padding: const EdgeInsets.only(bottom: Spacing.xs),
              child: Row(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  const Icon(PiccoloIcons.info, size: 16, color: PiccoloTheme.info),
                  const SizedBox(width: Spacing.sm),
                  Expanded(child: Text(note, style: const TextStyle(fontSize: 13))),
                ],
              ),
            )).toList(),
          ),
        ),
        const SizedBox(height: Spacing.lg),
      ],

      if (guide.docsUrl.isNotEmpty)
        Padding(
          padding: const EdgeInsets.only(bottom: Spacing.lg),
          child: Row(
            children: [
              const Icon(PiccoloIcons.fileText, size: 16, color: PiccoloTheme.inkMuted),
              const SizedBox(width: Spacing.sm),
              Expanded(
                child: Text.rich(
                  TextSpan(
                    children: [
                      const TextSpan(
                        text: 'Documentation: ',
                        style: TextStyle(color: PiccoloTheme.inkMuted, fontSize: 12),
                      ),
                      TextSpan(
                        text: guide.docsUrl,
                        style: const TextStyle(
                          color: PiccoloTheme.cobalt600,
                          decoration: TextDecoration.underline,
                          fontSize: 12,
                        ),
                        recognizer: _docsLinkRecognizer ??= (TapGestureRecognizer()
                          ..onTap = () => launchUrl(Uri.parse(guide.docsUrl))),
                      ),
                    ],
                  ),
                ),
              ),
            ],
          ),
        ),
    ];
  }

  Widget _buildTextField(String label, TextEditingController ctrl, {String? hint, bool obscureText = false}) {
    return TextField(
      controller: ctrl,
      obscureText: obscureText,
      autofillHints: obscureText ? const [] : null,
      enabled: !_isConnecting,
      decoration: InputDecoration(
        labelText: label,
        hintText: hint,
        border: const OutlineInputBorder(),
      ),
    );
  }
}
