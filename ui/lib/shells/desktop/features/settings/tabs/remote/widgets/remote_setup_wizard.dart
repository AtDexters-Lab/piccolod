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

  @override
  State<RemoteSetupWizard> createState() => _RemoteSetupWizardState();
}

class _RemoteSetupWizardState extends State<RemoteSetupWizard> {
  // Step 0: Guide
  final TextEditingController _endpointCtrl = TextEditingController();
  final TextEditingController _portalCtrl = TextEditingController();
  final TextEditingController _secretCtrl = TextEditingController();

  @override
  void initState() {
    super.initState();
    // Pre-fill from status if available (e.g. resuming)
    final status = widget.controller.status;
    if (status != null) {
      if (status.endpoint != null) _endpointCtrl.text = status.endpoint!;
      if (status.portalHostname != null) _portalCtrl.text = status.portalHostname!;

      // Smart Resume Logic
      if (status.state == 'preflight_required') {
        // Check if we have the config data in status (re-running on existing config)
        // vs first-time setup interrupted (config not yet persisted)
        if (status.endpoint != null && status.endpoint!.isNotEmpty) {
          widget.controller.wizardStep = 1;
          // Seed pending config from status so submitConfiguration has the data
          widget.controller.seedPendingConfigFromStatus();
          // Auto-run preflight if we are resuming into this state
          _autoRunPreflight();
        }
        // Otherwise stay on Step 0 - user needs to re-enter config
      }
    }

    // Load guide info
    unawaited(widget.controller.loadNexusGuide());
  }

  void _autoRunPreflight() {
    // 1-second delay for "heads up" then run
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
    return Column(
      children: [
        _buildStepperHeader(),
        const SizedBox(height: Spacing.xl),
        _buildCurrentStep(),
      ],
    );
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

  // --- Step 0: Connect (Nexus Guide) ---

  Widget _buildStep0Guide() {
    final guide = widget.controller.guideInfo;
    if (guide == null) return const Center(child: CircularProgressIndicator());

    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text('Connect to Nexus', style: PiccoloTheme.textTheme.headlineMedium),
        const SizedBox(height: Spacing.base),
        const Text('To enable remote access, you need a Nexus Relay. You can host your own.'),
        const SizedBox(height: Spacing.lg),

        // Requirements
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

        // Command
        Text('Installation', style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
        const SizedBox(height: Spacing.sm),
        Container(
          padding: const EdgeInsets.all(Spacing.base),
          decoration: BoxDecoration(
            color: PiccoloTheme.ink,
            borderRadius: BorderRadius.circular(Radii.sm),
          ),
          width: double.infinity,
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              SelectableText(
                guide.command,
                style: PiccoloTheme.mono.copyWith(color: PiccoloTheme.success, height: 1.5),
              ),
            ],
          ),
        ),
        Padding(
          padding: const EdgeInsets.only(top: Spacing.sm),
          child: Text('Run this command on your VPS.', style: PiccoloTheme.textTheme.labelSmall),
        ),
        const SizedBox(height: Spacing.lg),

        // Notes
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

        // Docs Link
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
              // Auto-run preflight after transition
              _autoRunPreflight();
            },
            child: const Text('Next: Run Preflight'),
          ),
        ),
      ],
    );
  }

  // --- Step 1: Preflight & Enable ---

  Widget _buildStep1Preflight() {
    final allPassed = widget.controller.preflightChecks.isNotEmpty &&
        !widget.controller.preflightChecks.any((c) => c.status == 'fail');

    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text('Verify & Enable', style: PiccoloTheme.textTheme.headlineMedium),
        const SizedBox(height: Spacing.base),
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
                    ? () => widget.controller.submitConfiguration()
                    : null, // Disable if any check failed
                child: const Text('Enable Remote Access'),
              ),
            ],
          ),

        // Info box about HTTP-01
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

  Widget _buildTextField(String label, TextEditingController ctrl, {String? hint, bool obscureText = false, void Function(String)? onChanged}) {
    return TextField(
      controller: ctrl,
      obscureText: obscureText,
      onChanged: onChanged,
      decoration: InputDecoration(
        labelText: label,
        hintText: hint,
        border: const OutlineInputBorder(),
      ),
    );
  }
}
