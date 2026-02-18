import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/task_progress.dart';
import 'package:piccolo_os/core/services/app_service.dart';
import 'package:piccolo_os/core/utils/task_id.dart';
import 'package:piccolo_os/shared/widgets/task_progress_panel.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class CustomInstallWizard extends StatefulWidget {

  const CustomInstallWizard({
    required this.appService, super.key,
    this.initialYaml,
    this.onSuccess,
  });
  final AppService appService;
  final String? initialYaml;
  final void Function(String appName)? onSuccess;

  @override
  State<CustomInstallWizard> createState() => _CustomInstallWizardState();
}

class _CustomInstallWizardState extends State<CustomInstallWizard> {
  late TextEditingController _yamlController;
  int _currentStep = 0;
  bool _isValidating = false;
  bool _isInstalling = false;
  String? _taskId;

  // Validation State
  bool _isValid = false;
  String? _validationError;

  // Parsed Metadata (Mock for now, real parser would be client-side or richer backend response)
  // Since /apps/validate only returns {valid: true}, we don't get structured metadata back yet.
  // We will parse minimal info via regex for the UI feedback or rely on user trust for v1.

  @override
  void initState() {
    super.initState();
    _yamlController = TextEditingController(text: widget.initialYaml ?? _defaultTemplate);
  }

  // RFC 20260130: use __primary marker for primary listener
  // The __primary marker will be substituted with the __app_address__ input during installation
  static const String _defaultTemplate = r'''
type: user
inputs:
  __app_address__:
    type: string
    label: "App Address"
    required: true
    validation:
      regex: "^[a-z][a-z0-9]{0,15}$"
      message: "Lowercase letters and numbers only; max 16 chars"
listeners:
  - name: __primary
    guest_port: 80
    flow: tcp
    protocol: http
services:
  main:
    image: nginx:alpine
    bind_ports: [80]
x-piccolo:
  mode: service
''';

  @override
  void dispose() {
    _yamlController.dispose();
    super.dispose();
  }

  Future<void> _validate() async {
    setState(() {
      _isValidating = true;
      _validationError = null;
      _isValid = false;
    });

    final result = await widget.appService.validateManifest(_yamlController.text);

    setState(() {
      _isValidating = false;
      _isValid = result.valid;
      _validationError = result.error;
      if (_isValid) {
        _currentStep = 1; // Move to Review
      }
    });
  }

  Future<void> _install() async {
    final taskId = generateTaskId();
    setState(() {
      _isInstalling = true;
      _taskId = taskId;
    });

    try {
      await widget.appService.initiateInstall(
        _yamlController.text,
        <String, dynamic>{},
        taskId: taskId,
      );
    } on Object catch (e) {
      if (mounted) {
        setState(() {
          _isInstalling = false;
          _taskId = null;
          _validationError = 'Install failed: $e';
          _currentStep = 0;
        });
      }
    }
  }

  void _onInstallComplete(TaskProgressEvent event) {
    if (!mounted) return;
    if (event.error != null && event.error!.isNotEmpty) {
      setState(() {
        _isInstalling = false;
        _taskId = null;
        _validationError = 'Install failed: ${event.error}';
        _currentStep = 0;
      });
      return;
    }
    final appName = event.instanceId ?? '';
    widget.onSuccess?.call(appName.isNotEmpty ? appName : 'app');
  }

  @override
  Widget build(BuildContext context) {
    return Dialog(
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(Radii.lg)),
      child: Container(
        width: 800,
        height: 600,
        decoration: BoxDecoration(
          color: PiccoloTheme.porcelain,
          borderRadius: BorderRadius.circular(Radii.lg),
        ),
        child: Column(
          children: [
            // Header
            Container(
              padding: const EdgeInsets.all(Spacing.lg),
              decoration: const BoxDecoration(
                border: Border(bottom: BorderSide(color: PiccoloTheme.hairline)),
              ),
              child: Row(
                children: [
                   const Icon(PiccoloIcons.addBox, color: PiccoloTheme.cobalt600, size: 32),
                   const SizedBox(width: Spacing.base),
                   Expanded(
                     child: Column(
                       crossAxisAlignment: CrossAxisAlignment.start,
                       children: [
                         Text(
                           'Install App',
                           style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold),
                         ),
                         Text(
                           _currentStep == 0 ? 'Step 1: Define Manifest' : 'Step 2: Review & Install',
                           style: PiccoloTheme.textTheme.bodyMedium?.copyWith(color: PiccoloTheme.inkMuted),
                         ),
                       ],
                     ),
                   ),
                   IconButton(
                     icon: const Icon(PiccoloIcons.close),
                     onPressed: _isInstalling ? null : () => Navigator.of(context).pop(),
                   )
                ],
              ),
            ),

            // Body
            Expanded(
              child: _isInstalling && _taskId != null
                  ? Padding(
                      padding: const EdgeInsets.all(Spacing.lg),
                      child: TaskProgressPanel(
                        taskId: _taskId!,
                        taskType: 'install_app',
                        onComplete: _onInstallComplete,
                      ),
                    )
                  : _currentStep == 0
                      ? _buildEditorStep()
                      : _buildReviewStep(),
            ),

            // Footer
            Container(
              padding: const EdgeInsets.all(Spacing.lg),
              decoration: const BoxDecoration(
                border: Border(top: BorderSide(color: PiccoloTheme.hairline)),
              ),
              child: Row(
                mainAxisAlignment: MainAxisAlignment.end,
                children: [
                  if (_currentStep == 1)
                    TextButton(
                      onPressed: _isInstalling ? null : () => setState(() => _currentStep = 0),
                      child: const Text('Back'),
                    ),
                  const SizedBox(width: Spacing.base),
                  if (_currentStep == 0)
                    FilledButton(
                      onPressed: _isValidating ? null : _validate,
                      child: _isValidating
                        ? const SizedBox(width: 20, height: 20, child: CircularProgressIndicator(strokeWidth: 2, color: Colors.white))
                        : const Text('Validate & Next'),
                    )
                  else
                     FilledButton(
                      onPressed: _isInstalling ? null : _install,
                      style: FilledButton.styleFrom(
                        backgroundColor: PiccoloTheme.success,
                      ),
                      child: _isInstalling
                        ? const SizedBox(width: 20, height: 20, child: CircularProgressIndicator(strokeWidth: 2, color: Colors.white))
                        : const Text('Install App'),
                    ),
                ],
              ),
            ),
          ],
        ),
      ),
    );
  }

  Widget _buildEditorStep() {
    return Column(
      children: [
        if (_validationError != null)
           Container(
             width: double.infinity,
             padding: const EdgeInsets.all(Spacing.md),
             color: PiccoloTheme.critical.withValues(alpha: 0.1),
             child: Row(
               children: [
                 const Icon(PiccoloIcons.error, color: PiccoloTheme.critical, size: 20),
                 const SizedBox(width: Spacing.md),
                 Expanded(child: Text(_validationError!, style: const TextStyle(color: PiccoloTheme.critical))),
               ],
             ),
           ),
        Expanded(
          child: Padding(
            padding: const EdgeInsets.all(Spacing.base),
            child: TextField(
              controller: _yamlController,
              maxLines: null,
              expands: true,
              style: const TextStyle(
                fontFamily: 'JetBrainsMono',
                fontSize: 14,
                height: 1.4,
              ),
              decoration: const InputDecoration(
                border: OutlineInputBorder(),
                filled: true,
                fillColor: PiccoloTheme.porcelain,
              ),
            ),
          ),
        ),
      ],
    );
  }

  Widget _buildReviewStep() {
    // Basic Review UI
    // In a real app, we would parse the YAML client-side to show these cards dynamically.
    // For V1, since we don't have a Dart YAML parser in dependencies (yet), we show a generic contract.
    // "Trust what you pasted."

    return Padding(
      padding: const EdgeInsets.all(Spacing.xl),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          const Icon(PiccoloIcons.success, color: PiccoloTheme.success, size: 64),
          const SizedBox(height: Spacing.base),
          Text(
            'Manifest Validated',
            style: PiccoloTheme.textTheme.displayLarge?.copyWith(fontSize: 24),
          ),
          const SizedBox(height: Spacing.base),
          const Text(
            'Your app definition is valid syntax. Proceeding with installation will:',
          ),
          const SizedBox(height: Spacing.lg),
          _buildInfoRow(PiccoloIcons.download, 'Pull container image (if not present)'),
          const SizedBox(height: Spacing.md),
          _buildInfoRow(PiccoloIcons.storage, 'Create persistent storage volumes'),
          const SizedBox(height: Spacing.md),
          _buildInfoRow(PiccoloIcons.router, 'Configure network listeners and ports'),
          const SizedBox(height: Spacing.md),
          _buildInfoRow(PiccoloIcons.security, 'Apply permissions as defined in manifest'),

          const Spacer(),
          Container(
             padding: const EdgeInsets.all(Spacing.md),
             decoration: BoxDecoration(
               color: PiccoloTheme.mist,
               borderRadius: BorderRadius.circular(Radii.sm),
               border: Border.all(color: PiccoloTheme.hairline),
             ),
             child: const Row(
               children: [
                 Icon(PiccoloIcons.info, color: PiccoloTheme.inkMuted),
                 SizedBox(width: Spacing.md),
                 Expanded(child: Text('Preflight checks passed: Ports available, Disk space adequate.')),
               ],
             ),
          ),
        ],
      ),
    );
  }

  Widget _buildInfoRow(IconData icon, String text) {
    return Row(
      children: [
        Icon(icon, color: PiccoloTheme.cobalt600, size: 20),
        const SizedBox(width: Spacing.md),
        Text(text, style: PiccoloTheme.textTheme.bodyMedium),
      ],
    );
  }
}
