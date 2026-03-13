import 'dart:async';

import 'package:flutter/gestures.dart';
import 'package:flutter/material.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/remote_controller.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/widgets/remote_preflight_list.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'package:url_launcher/url_launcher.dart';

class RemoteSetupWizard extends StatefulWidget {

  const RemoteSetupWizard({required this.controller, super.key});
  final RemoteController controller;

  /// Opens the self-hosted setup wizard as a dialog.
  static void show(BuildContext context, RemoteController controller) {
    unawaited(showDialog<void>(
      context: context,
      builder: (context) => Dialog(
        child: ConstrainedBox(
          constraints: const BoxConstraints(maxWidth: 600, maxHeight: 700),
          child: ClipRRect(
            borderRadius: BorderRadius.circular(Radii.md),
            child: RemoteSetupWizard(controller: controller),
          ),
        ),
      ),
    ).then((_) {
      // Reset wizard state on dismiss to avoid stale state on reopen
      controller.resetWizardState();
    }));
  }

  @override
  State<RemoteSetupWizard> createState() => _RemoteSetupWizardState();
}

class _RemoteSetupWizardState extends State<RemoteSetupWizard> {
  final TextEditingController _endpointCtrl = TextEditingController();
  final TextEditingController _portalCtrl = TextEditingController();
  final TextEditingController _secretCtrl = TextEditingController();

  @override
  void initState() {
    super.initState();
    final status = widget.controller.status;
    if (status != null) {
      if (status.endpoint != null) _endpointCtrl.text = status.endpoint!;
      if (status.portalHostname != null) _portalCtrl.text = status.portalHostname!;

      if (status.state == 'preflight_required') {
        if (status.endpoint != null && status.endpoint!.isNotEmpty) {
          widget.controller.wizardStep = 1;
          widget.controller.seedPendingConfigFromStatus();
          _autoRunPreflight();
        }
      }
    }

    unawaited(widget.controller.loadNexusGuide());
  }

  void _autoRunPreflight() {
    Timer(const Duration(seconds: 1), () {
      if (mounted && !widget.controller.isRunningPreflight && widget.controller.preflightChecks.isEmpty) {
        unawaited(widget.controller.runPreflight());
      }
    });
  }

  @override
  void dispose() {
    _endpointCtrl.dispose();
    _portalCtrl.dispose();
    _secretCtrl.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return ListenableBuilder(
      listenable: widget.controller,
      builder: (context, _) {
        return Material(
          color: PiccoloTheme.porcelain,
          child: SingleChildScrollView(
            padding: const EdgeInsets.all(Spacing.xl),
            child: Column(
              mainAxisSize: MainAxisSize.min,
              children: [
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
                _buildStepperHeader(),
                const SizedBox(height: Spacing.xl),
                _buildCurrentStep(),
                // Management actions when self-hosted is already active
                if (_isSelfHostedActive) ...[
                  const Divider(height: Spacing.xl * 2),
                  _buildManagementActions(),
                ],
              ],
            ),
          ),
        );
      },
    );
  }

  bool get _isSelfHostedActive {
    final status = widget.controller.status;
    return status != null &&
        status.enabled &&
        status.portalHostname != null &&
        status.portalHostname!.isNotEmpty;
  }

  Widget _buildManagementActions() {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text('Management', style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
        const SizedBox(height: Spacing.base),
        Row(
          children: [
            OutlinedButton(
              onPressed: () => _confirmDisable(context),
              style: OutlinedButton.styleFrom(foregroundColor: PiccoloTheme.critical),
              child: const Text('Disable'),
            ),
            const SizedBox(width: Spacing.base),
            OutlinedButton(
              onPressed: () => _confirmRotate(context),
              child: const Text('Rotate Credentials'),
            ),
          ],
        ),
      ],
    );
  }

  void _confirmDisable(BuildContext parentContext) {
    unawaited(showDialog<void>(
      context: parentContext,
      builder: (dialogContext) => AlertDialog(
        title: const Text('Disable Self-hosted Remote Access?'),
        content: const Text('This will stop the self-hosted relay connection. Managed (Piccolo Cloud) access is unaffected.'),
        actions: [
          TextButton(onPressed: () => Navigator.pop(dialogContext), child: const Text('Cancel')),
          FilledButton(
            style: FilledButton.styleFrom(backgroundColor: PiccoloTheme.critical),
            onPressed: () async {
              Navigator.pop(dialogContext);
              await widget.controller.disableRemote();
              if (parentContext.mounted) Navigator.of(parentContext).pop();
            },
            child: const Text('Disable'),
          ),
        ],
      ),
    ));
  }

  void _confirmRotate(BuildContext parentContext) {
    unawaited(showDialog<void>(
      context: parentContext,
      builder: (dialogContext) => AlertDialog(
        title: const Text('Rotate Credentials?'),
        content: const Text('This will briefly disconnect the tunnel. Ensure your configuration is backed up.'),
        actions: [
          TextButton(onPressed: () => Navigator.pop(dialogContext), child: const Text('Cancel')),
          FilledButton(
            onPressed: () async {
              Navigator.pop(dialogContext);
              final secret = await widget.controller.rotateCredentials();
              if (secret != null && parentContext.mounted) {
                _showSecretDialog(parentContext, secret);
              }
            },
            child: const Text('Rotate'),
          ),
        ],
      ),
    ));
  }

  void _showSecretDialog(BuildContext context, String secret) {
    unawaited(showDialog<void>(
      context: context,
      barrierDismissible: false,
      builder: (context) => AlertDialog(
        title: const Text('New Device Secret'),
        content: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            const Text(
              'Credentials rotated successfully. You must update your Nexus Relay with this new secret to reconnect.',
              style: TextStyle(color: PiccoloTheme.ink),
            ),
            const SizedBox(height: Spacing.base),
            Container(
              padding: const EdgeInsets.all(Spacing.base),
              width: double.infinity,
              decoration: BoxDecoration(
                color: PiccoloTheme.mist,
                borderRadius: BorderRadius.circular(Radii.sm),
                border: Border.all(color: PiccoloTheme.cobalt600),
              ),
              child: SelectableText(
                secret,
                style: PiccoloTheme.mono.copyWith(fontWeight: FontWeight.bold, fontSize: 16),
              ),
            ),
          ],
        ),
        actions: [
          FilledButton(
            onPressed: () {
              Navigator.pop(context);
              unawaited(widget.controller.refresh());
            },
            child: const Text('Done'),
          ),
        ],
      ),
    ));
  }

  Widget _buildStepperHeader() {
    return Row(
      children: [
        _buildStepIndicator(0, 'Connect'),
        _buildStepSeparator(),
        _buildStepIndicator(1, 'Verify & Enable'),
      ],
    );
  }

  Widget _buildStepIndicator(int step, String label) {
    final current = widget.controller.wizardStep;
    final isActive = step == current;
    final isCompleted = step < current;

    return Expanded(
      child: Column(
        children: [
          CircleAvatar(
            backgroundColor: isActive || isCompleted ? PiccoloTheme.cobalt600 : PiccoloTheme.mist,
            foregroundColor: isActive || isCompleted ? PiccoloTheme.porcelain : PiccoloTheme.inkMuted,
            child: isCompleted ? const Icon(PiccoloIcons.check) : Text('${step + 1}'),
          ),
          const SizedBox(height: Spacing.sm),
          Text(
            label,
            style: TextStyle(
              fontWeight: isActive ? FontWeight.bold : FontWeight.normal,
              color: isActive ? PiccoloTheme.ink : PiccoloTheme.inkMuted,
            ),
          ),
        ],
      ),
    );
  }

  Widget _buildStepSeparator() {
    return const SizedBox(width: Spacing.xl, child: Divider());
  }

  Widget _buildCurrentStep() {
    switch (widget.controller.wizardStep) {
      case 0:
        return _buildStep0Guide();
      case 1:
        return _buildStep1Preflight();
      default:
        return const Text('Unknown step');
    }
  }

  Widget _buildStep0Guide() {
    final guide = widget.controller.guideInfo;
    if (guide == null) return const Center(child: CircularProgressIndicator());

    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
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
                           recognizer: TapGestureRecognizer()
                             ..onTap = () => launchUrl(Uri.parse(guide.docsUrl)),
                         ),
                       ],
                     ),
                   ),
                 ),
               ],
             ),
           ),

        const Divider(),
        const SizedBox(height: Spacing.xl),

        Text('Enter Connection Details', style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
        const SizedBox(height: Spacing.base),
        _buildTextField('Nexus Endpoint', _endpointCtrl, hint: 'wss://nexus.example.com'),
        const SizedBox(height: Spacing.base),
        _buildTextField('Portal Hostname', _portalCtrl, hint: 'portal.home.example.com'),
        Padding(
          padding: const EdgeInsets.only(top: Spacing.sm, bottom: Spacing.sm),
          child: Text(
            'This is the fully-qualified domain name where this Piccolo device will be accessible remotely. '
            "All app subdomains (e.g., myapp.home.example.com) will be derived from this hostname's parent domain.",
            style: PiccoloTheme.textTheme.labelSmall?.copyWith(color: PiccoloTheme.inkMuted),
          ),
        ),
        const SizedBox(height: Spacing.sm),
        _buildTextField('Device Secret', _secretCtrl, obscureText: true),

        const SizedBox(height: Spacing.xl),
        Align(
          alignment: Alignment.centerRight,
          child: FilledButton(
            onPressed: () async {
              if (_endpointCtrl.text.isEmpty || _secretCtrl.text.isEmpty || _portalCtrl.text.isEmpty) {
                ScaffoldMessenger.of(context).showSnackBar(const SnackBar(content: Text('Please fill all fields')));
                return;
              }
              if (!_portalCtrl.text.contains('.')) {
                ScaffoldMessenger.of(context).showSnackBar(const SnackBar(content: Text('Portal hostname must be a fully-qualified domain (e.g., portal.home.example.com)')));
                return;
              }
              await widget.controller.verifyNexusGuide(
                _endpointCtrl.text,
                _portalCtrl.text,
                _secretCtrl.text,
              );
              _autoRunPreflight();
            },
            child: const Text('Next: Run Preflight'),
          ),
        ),
      ],
    );
  }

  Widget _buildStep1Preflight() {
    final allPassed = widget.controller.preflightChecks.isNotEmpty &&
        !widget.controller.preflightChecks.any((c) => c.status == 'fail');

    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        const Text('Verifying your environment and connection settings.'),
        const SizedBox(height: Spacing.lg),

        if (!widget.controller.isRunningPreflight && widget.controller.preflightChecks.isEmpty)
           Center(
             child: FilledButton(
               onPressed: widget.controller.runPreflight,
               child: const Text('Run Checks'),
             ),
           )
        else ...[
           RemotePreflightList(checks: widget.controller.preflightChecks),
           if (!widget.controller.isRunningPreflight)
             Padding(
               padding: const EdgeInsets.only(top: Spacing.base),
               child: Center(
                 child: TextButton.icon(
                   onPressed: widget.controller.runPreflight,
                   icon: const Icon(PiccoloIcons.refresh),
                   label: const Text('Re-run Checks'),
                 ),
               ),
             ),
        ],

        if (widget.controller.isRunningPreflight)
          const Padding(padding: EdgeInsets.only(top: Spacing.base), child: Center(child: CircularProgressIndicator())),

        if (widget.controller.isSubmittingConfig)
          const Padding(padding: EdgeInsets.only(top: Spacing.base), child: Center(child: CircularProgressIndicator())),

        const SizedBox(height: Spacing.xl),
        if (widget.controller.preflightChecks.isNotEmpty &&
            !widget.controller.isRunningPreflight &&
            !widget.controller.isSubmittingConfig)
          Row(
            mainAxisAlignment: MainAxisAlignment.spaceBetween,
            children: [
              TextButton(onPressed: () {
                setState(() => widget.controller.wizardStep = 0);
              }, child: const Text('Back')),

              FilledButton(
                onPressed: allPassed
                    ? () async {
                        await widget.controller.submitConfiguration();
                        if (mounted && widget.controller.error == null) {
                          Navigator.of(context).pop();
                        }
                      }
                    : null,
                child: const Text('Enable Remote Access'),
              ),
            ],
          ),

        if (allPassed) ...[
          const SizedBox(height: Spacing.lg),
          Container(
            padding: const EdgeInsets.all(Spacing.md),
            decoration: BoxDecoration(
              color: PiccoloTheme.info.withValues(alpha: 0.1),
              borderRadius: BorderRadius.circular(Radii.sm),
              border: Border.all(color: PiccoloTheme.info.withValues(alpha: 0.3)),
            ),
            child: const Row(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Icon(PiccoloIcons.info, size: 16, color: PiccoloTheme.info),
                SizedBox(width: Spacing.sm),
                Expanded(
                  child: Text(
                    'Certificates will be issued using HTTP-01 challenge. '
                    'Each app will receive its own certificate automatically.',
                    style: TextStyle(fontSize: 13),
                  ),
                ),
              ],
            ),
          ),
        ],
      ],
    );
  }

  Widget _buildTextField(String label, TextEditingController ctrl, {String? hint, bool obscureText = false}) {
    return TextField(
      controller: ctrl,
      obscureText: obscureText,
      decoration: InputDecoration(
        labelText: label,
        hintText: hint,
        border: const OutlineInputBorder(),
      ),
    );
  }
}
