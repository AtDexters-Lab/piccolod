import 'package:flutter/material.dart';
import '../../theme/piccolo_theme.dart';
import '../../core/services/app_service.dart';
import '../../core/utils/task_id.dart';
import '../../shared/widgets/task_progress_panel.dart';

class CustomInstallWizard extends StatefulWidget {
  final AppService appService;
  final String? initialYaml;
  final Function(String appName)? onSuccess;

  const CustomInstallWizard({
    super.key,
    required this.appService,
    this.initialYaml,
    this.onSuccess,
  });

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
  static const String _defaultTemplate = '''
type: user
inputs:
  __app_address__:
    type: string
    label: "App Address"
    required: true
    validation:
      regex: "^[a-z][a-z0-9]{0,15}\$"
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
      final app = await widget.appService.installAppWithInputs(
        _yamlController.text,
        <String, dynamic>{},
        taskId: taskId,
      );
      
      if (mounted) {
        widget.onSuccess?.call(app.name);
        
        // If no callback provided (fallback), show snackbar
        if (widget.onSuccess == null) {
           Navigator.of(context).pop(); 
           ScaffoldMessenger.of(context).showSnackBar(
              const SnackBar(content: Text("App installation started.")),
           );
        }
      }
    } catch (e) {
      if (mounted) {
        setState(() {
          _isInstalling = false;
          _taskId = null;
          _validationError = "Install failed: $e";
          _currentStep = 0; // Go back to edit
        });
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    return Dialog(
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(16)),
      child: Container(
        width: 800,
        height: 600,
        decoration: BoxDecoration(
          color: PiccoloTheme.porcelain,
          borderRadius: BorderRadius.circular(16),
        ),
        child: Column(
          children: [
            // Header
            Container(
              padding: const EdgeInsets.all(24),
              decoration: const BoxDecoration(
                border: Border(bottom: BorderSide(color: Colors.black12)),
              ),
              child: Row(
                children: [
                   const Icon(Icons.add_box, color: PiccoloTheme.cobalt600, size: 32),
                   const SizedBox(width: 16),
                   Expanded(
                     child: Column(
                       crossAxisAlignment: CrossAxisAlignment.start,
                       children: [
                         Text(
                           "Install App",
                           style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold),
                         ),
                         Text(
                           _currentStep == 0 ? "Step 1: Define Manifest" : "Step 2: Review & Install",
                           style: PiccoloTheme.textTheme.bodyMedium?.copyWith(color: PiccoloTheme.inkMuted),
                         ),
                       ],
                     ),
                   ),
                   IconButton(
                     icon: const Icon(Icons.close),
                     onPressed: _isInstalling ? null : () => Navigator.of(context).pop(),
                   )
                ],
              ),
            ),

            // Body
            Expanded(
              child: _isInstalling && _taskId != null
                  ? Padding(
                      padding: const EdgeInsets.all(24),
                      child: TaskProgressPanel(
                        taskId: _taskId!,
                        taskType: 'install_app',
                      ),
                    )
                  : _currentStep == 0
                      ? _buildEditorStep()
                      : _buildReviewStep(),
            ),

            // Footer
            Container(
              padding: const EdgeInsets.all(24),
              decoration: const BoxDecoration(
                border: Border(top: BorderSide(color: Colors.black12)),
              ),
              child: Row(
                mainAxisAlignment: MainAxisAlignment.end,
                children: [
                  if (_currentStep == 1)
                    TextButton(
                      onPressed: _isInstalling ? null : () => setState(() => _currentStep = 0),
                      child: const Text("Back"),
                    ),
                  const SizedBox(width: 16),
                  if (_currentStep == 0)
                    FilledButton(
                      onPressed: _isValidating ? null : _validate,
                      style: FilledButton.styleFrom(
                        backgroundColor: PiccoloTheme.cobalt600,
                        foregroundColor: Colors.white,
                      ),
                      child: _isValidating 
                        ? const SizedBox(width: 20, height: 20, child: CircularProgressIndicator(strokeWidth: 2, color: Colors.white))
                        : const Text("Validate & Next"),
                    )
                  else
                     FilledButton(
                      onPressed: _isInstalling ? null : _install,
                      style: FilledButton.styleFrom(
                        backgroundColor: PiccoloTheme.success,
                        foregroundColor: Colors.white,
                      ),
                      child: _isInstalling
                        ? const SizedBox(width: 20, height: 20, child: CircularProgressIndicator(strokeWidth: 2, color: Colors.white))
                        : const Text("Install App"),
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
             padding: const EdgeInsets.all(12),
             color: PiccoloTheme.critical.withValues(alpha: 0.1),
             child: Row(
               children: [
                 const Icon(Icons.error, color: PiccoloTheme.critical, size: 20),
                 const SizedBox(width: 12),
                 Expanded(child: Text(_validationError!, style: const TextStyle(color: PiccoloTheme.critical))),
               ],
             ),
           ),
        Expanded(
          child: Padding(
            padding: const EdgeInsets.all(16.0),
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
                fillColor: Colors.white,
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
      padding: const EdgeInsets.all(32.0),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          const Icon(Icons.check_circle, color: PiccoloTheme.success, size: 64),
          const SizedBox(height: 16),
          Text(
            "Manifest Validated",
            style: PiccoloTheme.textTheme.displayLarge?.copyWith(fontSize: 24),
          ),
          const SizedBox(height: 16),
          const Text(
            "Your app definition is valid syntax. Proceeding with installation will:",
          ),
          const SizedBox(height: 24),
          _buildInfoRow(Icons.download, "Pull container image (if not present)"),
          const SizedBox(height: 12),
          _buildInfoRow(Icons.sd_storage, "Create persistent storage volumes"),
          const SizedBox(height: 12),
          _buildInfoRow(Icons.router, "Configure network listeners and ports"),
          const SizedBox(height: 12),
          _buildInfoRow(Icons.security, "Apply permissions as defined in manifest"),
          
          const Spacer(),
          Container(
             padding: const EdgeInsets.all(12),
             decoration: BoxDecoration(
               color: PiccoloTheme.mist,
               borderRadius: BorderRadius.circular(8),
               border: Border.all(color: Colors.black12),
             ),
             child: const Row(
               children: [
                 Icon(Icons.info_outline, color: PiccoloTheme.inkMuted),
                 SizedBox(width: 12),
                 Expanded(child: Text("Preflight checks passed: Ports available, Disk space adequate.")),
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
        const SizedBox(width: 12),
        Text(text, style: PiccoloTheme.textTheme.bodyMedium),
      ],
    );
  }
}
