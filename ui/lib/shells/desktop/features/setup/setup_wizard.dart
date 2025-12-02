import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import '../../../../theme/piccolo_theme.dart';
import '../../../../shared/piccolo_wordmark.dart';
import 'setup_controller.dart';

class SetupWizard extends StatefulWidget {
  final VoidCallback onComplete;

  const SetupWizard({super.key, required this.onComplete});

  @override
  State<SetupWizard> createState() => _SetupWizardState();
}

class _SetupWizardState extends State<SetupWizard> {
  final SetupController _controller = SetupController();

  @override
  Widget build(BuildContext context) {
    return ListenableBuilder(
      listenable: _controller,
      builder: (context, child) {
        // If complete, trigger callback
        if (_controller.state == SetupState.complete) {
          // Schedule callback to avoid build-phase issues
          WidgetsBinding.instance.addPostFrameCallback((_) => widget.onComplete());
          return const SizedBox.shrink();
        }

        return Center(
          child: Container(
            width: 480,
            constraints: const BoxConstraints(maxHeight: 600),
            decoration: BoxDecoration(
              color: PiccoloTheme.porcelain,
              borderRadius: BorderRadius.circular(24),
              boxShadow: [
                BoxShadow(
                  color: Colors.black.withValues(alpha: 0.2),
                  blurRadius: 40,
                  offset: const Offset(0, 20),
                ),
              ],
              border: Border.all(color: Colors.white, width: 1),
            ),
            child: Column(
              mainAxisSize: MainAxisSize.min,
              children: [
                // Header
                Padding(
                  padding: const EdgeInsets.all(32.0),
                  child: Column(
                    children: [
                      const PiccoloWordmark(height: 24, color: PiccoloTheme.ink),
                      const SizedBox(height: 8),
                      if (_controller.state == SetupState.loading)
                        const Text("Checking status...")
                      else
                        Text(
                          _getTitleForState(_controller.state),
                          style: PiccoloTheme.textTheme.bodyMedium?.copyWith(
                            color: PiccoloTheme.inkMuted,
                          ),
                        ),
                    ],
                  ),
                ),

                // Content Body
                Flexible(
                  child: AnimatedSwitcher(
                    duration: const Duration(milliseconds: 300),
                    child: _buildStepContent(_controller.state),
                  ),
                ),
              ],
            ),
          ),
        );
      },
    );
  }

  String _getTitleForState(SetupState state) {
    switch (state) {
      case SetupState.welcome: return "Welcome";
      case SetupState.credentials: return "Create Admin Account";
      case SetupState.recovery: return "Recovery Key";
      case SetupState.finishing: return "Finishing up...";
      default: return "";
    }
  }

  Widget _buildStepContent(SetupState state) {
    switch (state) {
      case SetupState.loading:
        return const Padding(
          padding: EdgeInsets.all(48.0),
          child: CircularProgressIndicator(color: PiccoloTheme.cobalt600),
        );
      case SetupState.welcome:
        return _WelcomeStep(
          deviceName: _controller.deviceName,
          onNext: _controller.startSetup,
        );
      case SetupState.credentials:
        return _CredentialsStep(
          onSubmit: _controller.submitCredentials,
        );
      case SetupState.recovery:
        return _RecoveryStep(
          words: _controller.recoveryWords,
          onNext: _controller.completeSetup,
        );
      default:
        return const SizedBox.shrink();
    }
  }
}

// --- Steps ---

class _WelcomeStep extends StatelessWidget {
  final String deviceName;
  final VoidCallback onNext;

  const _WelcomeStep({required this.deviceName, required this.onNext});

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          Text(
            "Hello, $deviceName",
            style: PiccoloTheme.textTheme.displayLarge,
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 16),
          Text(
            "Let's set up your Digital Sanctuary.",
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(color: PiccoloTheme.inkMuted),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 40),
          ElevatedButton(
            onPressed: onNext,
            style: ElevatedButton.styleFrom(
              backgroundColor: PiccoloTheme.cobalt600,
              foregroundColor: Colors.white,
              padding: const EdgeInsets.symmetric(horizontal: 32, vertical: 16),
              shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(12)),
              elevation: 2,
            ),
            child: const Text("Start Setup"),
          ),
        ],
      ),
    );
  }
}

class _CredentialsStep extends StatefulWidget {
  final Future<bool> Function(String) onSubmit;

  const _CredentialsStep({required this.onSubmit});

  @override
  State<_CredentialsStep> createState() => _CredentialsStepState();
}

class _CredentialsStepState extends State<_CredentialsStep> {
  final TextEditingController _passController = TextEditingController();
  final TextEditingController _confirmController = TextEditingController();
  String? _error;
  bool _isSubmitting = false;

  Future<void> _submit() async {
    setState(() => _error = null);

    if (_passController.text.isEmpty) {
      setState(() => _error = "Password is required");
      return;
    }
    if (_passController.text != _confirmController.text) {
      setState(() => _error = "Passwords do not match");
      return;
    }
    
    setState(() => _isSubmitting = true);
    await widget.onSubmit(_passController.text);
  }

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: AutofillGroup(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            TextField(
              controller: _passController,
              obscureText: true,
              autofillHints: const [AutofillHints.newPassword],
              decoration: InputDecoration(
                labelText: "Password",
                border: OutlineInputBorder(borderRadius: BorderRadius.circular(12)),
                filled: true,
                fillColor: Colors.white,
                errorText: _error,
              ),
            ),
            const SizedBox(height: 16),
            TextField(
              controller: _confirmController,
              obscureText: true,
              autofillHints: const [AutofillHints.newPassword], // Browser often treats both as new
              decoration: InputDecoration(
                labelText: "Confirm Password",
                border: OutlineInputBorder(borderRadius: BorderRadius.circular(12)),
                filled: true,
                fillColor: Colors.white,
              ),
              onSubmitted: (_) => _submit(),
            ),
            const SizedBox(height: 16),
            const Text(
              "This password secures your device. Don't lose it.",
              style: TextStyle(color: PiccoloTheme.inkMuted, fontSize: 13),
            ),
            const SizedBox(height: 32),
            ElevatedButton(
              onPressed: _isSubmitting ? null : _submit,
              style: ElevatedButton.styleFrom(
                backgroundColor: PiccoloTheme.cobalt600,
                foregroundColor: Colors.white,
                padding: const EdgeInsets.symmetric(vertical: 16),
                shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(12)),
              ),
              child: _isSubmitting 
                  ? const SizedBox(width: 20, height: 20, child: CircularProgressIndicator(color: Colors.white, strokeWidth: 2))
                  : const Text("Create Account"),
            ),
          ],
        ),
      ),
    );
  }
}

class _RecoveryStep extends StatefulWidget {
  final List<String> words;
  final VoidCallback onNext;

  const _RecoveryStep({required this.words, required this.onNext});

  @override
  State<_RecoveryStep> createState() => _RecoveryStepState();
}

class _RecoveryStepState extends State<_RecoveryStep> {
  bool _confirmed = false;

  void _copyToClipboard() {
    Clipboard.setData(ClipboardData(text: widget.words.join(" ")));
    ScaffoldMessenger.of(context).showSnackBar(
      const SnackBar(content: Text("Recovery key copied to clipboard"), duration: Duration(seconds: 2)),
    );
  }

  void _downloadKey() {
    // TODO: Implement file download
    ScaffoldMessenger.of(context).showSnackBar(
      const SnackBar(content: Text("Download started (Mock)"), duration: Duration(seconds: 2)),
    );
  }

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
           Text(
            "Save this Recovery Key",
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold),
          ),
          const SizedBox(height: 16),
          Container(
            padding: const EdgeInsets.all(16),
            decoration: BoxDecoration(
              color: Colors.white,
              borderRadius: BorderRadius.circular(12),
              border: Border.all(color: PiccoloTheme.ink.withValues(alpha: 0.1)),
            ),
            constraints: const BoxConstraints(maxHeight: 160), // slightly reduced to fit buttons
            child: SingleChildScrollView(
              child: Wrap(
                spacing: 8,
                runSpacing: 8,
                children: widget.words.asMap().entries.map((e) {
                  return Container(
                    padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 4),
                    decoration: BoxDecoration(
                      color: PiccoloTheme.mist,
                      borderRadius: BorderRadius.circular(6),
                    ),
                    child: Text(
                      "${e.key + 1}. ${e.value}",
                      style: const TextStyle(fontFamily: 'monospace', fontSize: 12),
                    ),
                  );
                }).toList(),
              ),
            ),
          ),
          const SizedBox(height: 16),
          
          // Action Buttons
          Row(
            mainAxisAlignment: MainAxisAlignment.center,
            children: [
              OutlinedButton.icon(
                onPressed: _copyToClipboard,
                icon: const Icon(Icons.copy, size: 16),
                label: const Text("Copy"),
                style: OutlinedButton.styleFrom(foregroundColor: PiccoloTheme.ink),
              ),
              const SizedBox(width: 16),
              OutlinedButton.icon(
                onPressed: _downloadKey,
                icon: const Icon(Icons.download, size: 16),
                label: const Text("Download"),
                style: OutlinedButton.styleFrom(foregroundColor: PiccoloTheme.ink),
              ),
            ],
          ),
          const SizedBox(height: 24),

          // Confirmation Checkbox
          InkWell(
            onTap: () => setState(() => _confirmed = !_confirmed),
            child: Row(
              children: [
                Checkbox(
                  value: _confirmed,
                  onChanged: (v) => setState(() => _confirmed = v ?? false),
                  activeColor: PiccoloTheme.cobalt600,
                ),
                Expanded(
                  child: Text(
                    "I have saved this key in a safe place.",
                    style: PiccoloTheme.textTheme.bodyMedium,
                  ),
                ),
              ],
            ),
          ),
          
          const SizedBox(height: 24),
          ElevatedButton(
            onPressed: _confirmed ? widget.onNext : null,
            style: ElevatedButton.styleFrom(
              backgroundColor: PiccoloTheme.success,
              foregroundColor: Colors.white,
              disabledBackgroundColor: PiccoloTheme.ink.withValues(alpha: 0.1),
              disabledForegroundColor: PiccoloTheme.ink.withValues(alpha: 0.3),
              padding: const EdgeInsets.symmetric(horizontal: 32, vertical: 16),
              shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(12)),
            ),
            child: const Text("Finish Setup"),
          ),
        ],
      ),
    );
  }
}