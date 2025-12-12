import 'package:flutter/material.dart';
import '../../../../theme/piccolo_theme.dart';
import '../../../../shared/piccolo_wordmark.dart';
import '../../../../core/utils/downloader/downloader.dart';
import '../../../../core/utils/clipboard/clipboard.dart';
import '../../../../shared/widgets/password_set_form.dart';
import 'setup_controller.dart';

class SetupWizard extends StatefulWidget {
  final void Function(bool isFirstSetupFlow) onComplete;

  const SetupWizard({super.key, required this.onComplete});

  @override
  State<SetupWizard> createState() => _SetupWizardState();
}

class _SetupWizardState extends State<SetupWizard> {
  final SetupController _controller = SetupController();
  bool _didCallComplete = false;

  @override
  Widget build(BuildContext context) {
    return ListenableBuilder(
      listenable: _controller,
      builder: (context, child) {
        // If complete, trigger callback
        if (_controller.state == SetupState.complete && !_didCallComplete) {
          _didCallComplete = true;
          // Schedule callback to avoid build-phase issues
          WidgetsBinding.instance.addPostFrameCallback(
            (_) => widget.onComplete(_controller.isFirstSetupFlow),
          );
          return const SizedBox.shrink();
        } else if (_controller.state == SetupState.complete) {
          return const SizedBox.shrink();
        }

        return Container(
          color: Colors.black.withValues(alpha: 0.5),
          child: Center(
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
                        const PiccoloWordmark(
                          height: 24,
                          color: PiccoloTheme.ink,
                        ),
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
          ),
        );
      },
    );
  }

  String _getTitleForState(SetupState state) {
    switch (state) {
      case SetupState.welcome:
        return "Welcome";
      case SetupState.credentials:
        return "Create Admin Account";
      case SetupState.recovery:
        return "Recovery Key";
      case SetupState.finishing:
        return "Finishing up...";
      case SetupState.unlock:
        return "Unlock Device";
      case SetupState.login:
        return "Log In";
      case SetupState.forgotPassword:
        return "Reset Password";
      case SetupState.error:
        return "Connection Error";
      default:
        return "";
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
        return _CredentialsStep(onSubmit: _controller.submitCredentials);
      case SetupState.recovery:
        return _RecoveryStep(
          words: _controller.recoveryWords,
          onNext: _controller.completeSetup,
        );
      case SetupState.unlock:
        return _UnlockStep(
          onUnlock: _controller.unlock,
          onForgotPassword: _controller.startRecovery,
        );
      case SetupState.login:
        return _LoginStep(
          onLogin: _controller.login,
          onForgotPassword: _controller.startRecovery,
        );
      case SetupState.forgotPassword:
        return _ForgotPasswordStep(
          onReset: _controller.resetPassword,
          onCancel: _controller.cancelRecovery,
        );
      case SetupState.error:
        return _ErrorStep(
          error: _controller.error ?? "Unknown error",
          onRetry: _controller.retry,
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
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
              color: PiccoloTheme.inkMuted,
            ),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 40),
          ElevatedButton(
            onPressed: onNext,
            style: ElevatedButton.styleFrom(
              backgroundColor: PiccoloTheme.cobalt600,
              foregroundColor: Colors.white,
              padding: const EdgeInsets.symmetric(horizontal: 32, vertical: 16),
              shape: RoundedRectangleBorder(
                borderRadius: BorderRadius.circular(12),
              ),
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
  String? _confirmError;
  bool _isSubmitting = false;

  @override
  void dispose() {
    _passController.dispose();
    _confirmController.dispose();
    super.dispose();
  }

  Future<void> _submit() async {
    setState(() {
      _error = null;
      _confirmError = null;
    });

    if (_passController.text.isEmpty) {
      setState(() => _error = "Password is required");
      return;
    }
    if (_passController.text != _confirmController.text) {
      setState(() => _confirmError = "Passwords do not match");
      return;
    }

    setState(() => _isSubmitting = true);
    final success = await widget.onSubmit(_passController.text);

    if (mounted && !success) {
      setState(() {
        _isSubmitting = false;
        _error = "Setup failed. Please try again.";
      });
    }
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
            PasswordSetForm(
              passwordController: _passController,
              confirmController: _confirmController,
              passwordError: _error,
              confirmError: _confirmError,
              onSubmitted: _submit,
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
                shape: RoundedRectangleBorder(
                  borderRadius: BorderRadius.circular(12),
                ),
              ),
              child: _isSubmitting
                  ? const SizedBox(
                      width: 20,
                      height: 20,
                      child: CircularProgressIndicator(
                        color: Colors.white,
                        strokeWidth: 2,
                      ),
                    )
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

  Future<void> _copyToClipboard() async {
    try {
      await copyText(widget.words.join(" "));
      if (!mounted) return;
      ScaffoldMessenger.of(context).showSnackBar(
        const SnackBar(
          content: Text("Recovery key copied to clipboard"),
          duration: Duration(seconds: 2),
        ),
      );
    } catch (e) {
      // Fallback for some web contexts
      debugPrint("Clipboard error: $e");
      ScaffoldMessenger.of(context).showSnackBar(
        const SnackBar(
          content: Text("Failed to copy. Please download the file instead."),
          backgroundColor: Colors.red,
        ),
      );
    }
  }

  void _downloadKey() {
    final content =
        "PICCOLO RECOVERY KEY\n\n${widget.words.join(" ")}\n\nKeep this file safe.";
    downloadTextFile(content, "piccolo-recovery-key.txt");

    ScaffoldMessenger.of(context).showSnackBar(
      const SnackBar(
        content: Text("Downloading recovery key..."),
        duration: Duration(seconds: 2),
      ),
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
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
              fontWeight: FontWeight.bold,
            ),
          ),
          const SizedBox(height: 16),
          SelectionArea(
            child: Container(
              padding: const EdgeInsets.all(16),
              decoration: BoxDecoration(
                color: Colors.white,
                borderRadius: BorderRadius.circular(12),
                border: Border.all(
                  color: PiccoloTheme.ink.withValues(alpha: 0.1),
                ),
              ),
              constraints: const BoxConstraints(
                maxHeight: 160,
              ), // slightly reduced to fit buttons
              child: SingleChildScrollView(
                child: Wrap(
                  spacing: 8,
                  runSpacing: 8,
                  children: widget.words.asMap().entries.map((e) {
                    return Container(
                      padding: const EdgeInsets.symmetric(
                        horizontal: 8,
                        vertical: 4,
                      ),
                      decoration: BoxDecoration(
                        color: PiccoloTheme.mist,
                        borderRadius: BorderRadius.circular(6),
                      ),
                      child: Text(
                        "${e.key + 1}. ${e.value}",
                        style: const TextStyle(
                          fontFamily: 'monospace',
                          fontSize: 12,
                        ),
                      ),
                    );
                  }).toList(),
                ),
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
                style: OutlinedButton.styleFrom(
                  foregroundColor: PiccoloTheme.ink,
                ),
              ),
              const SizedBox(width: 16),
              OutlinedButton.icon(
                onPressed: _downloadKey,
                icon: const Icon(Icons.download, size: 16),
                label: const Text("Download"),
                style: OutlinedButton.styleFrom(
                  foregroundColor: PiccoloTheme.ink,
                ),
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
              shape: RoundedRectangleBorder(
                borderRadius: BorderRadius.circular(12),
              ),
            ),
            child: const Text("Finish Setup"),
          ),
        ],
      ),
    );
  }
}

class _UnlockStep extends StatefulWidget {
  final Future<bool> Function(String) onUnlock;
  final VoidCallback onForgotPassword;

  const _UnlockStep({required this.onUnlock, required this.onForgotPassword});

  @override
  State<_UnlockStep> createState() => _UnlockStepState();
}

class _UnlockStepState extends State<_UnlockStep> {
  final TextEditingController _passController = TextEditingController();
  bool _isSubmitting = false;
  bool _obscureText = true;
  String? _error;

  @override
  void dispose() {
    _passController.dispose();
    super.dispose();
  }

  Future<void> _submit() async {
    setState(() {
      _isSubmitting = true;
      _error = null;
    });

    final success = await widget.onUnlock(_passController.text);

    if (mounted && !success) {
      setState(() {
        _isSubmitting = false;
        _error = "Incorrect password";
      });
    }
  }

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          TextField(
            controller: _passController,
            obscureText: _obscureText,
            autofillHints: const [AutofillHints.password],
            decoration: InputDecoration(
              labelText: "Password",
              border: OutlineInputBorder(
                borderRadius: BorderRadius.circular(12),
              ),
              filled: true,
              fillColor: Colors.white,
              errorText: _error,
              suffixIcon: IconButton(
                icon: Icon(
                  _obscureText
                      ? Icons.visibility_off_outlined
                      : Icons.visibility_outlined,
                  color: PiccoloTheme.inkMuted,
                ),
                onPressed: () => setState(() => _obscureText = !_obscureText),
              ),
            ),
            onSubmitted: (_) => _submit(),
          ),
          const SizedBox(height: 24),
          Row(
            children: [
              TextButton(
                onPressed: widget.onForgotPassword,
                child: const Text("Forgot Password?"),
              ),
              const Spacer(),
              ElevatedButton(
                onPressed: _isSubmitting ? null : _submit,
                style: ElevatedButton.styleFrom(
                  backgroundColor: PiccoloTheme.cobalt600,
                  foregroundColor: Colors.white,
                  padding: const EdgeInsets.symmetric(
                    horizontal: 32,
                    vertical: 16,
                  ),
                  shape: RoundedRectangleBorder(
                    borderRadius: BorderRadius.circular(12),
                  ),
                ),
                child: _isSubmitting
                    ? const SizedBox(
                        width: 20,
                        height: 20,
                        child: CircularProgressIndicator(
                          color: Colors.white,
                          strokeWidth: 2,
                        ),
                      )
                    : const Text("Unlock"),
              ),
            ],
          ),
        ],
      ),
    );
  }
}

class _LoginStep extends StatefulWidget {
  final Future<bool> Function(String, String) onLogin;
  final VoidCallback onForgotPassword;

  const _LoginStep({required this.onLogin, required this.onForgotPassword});

  @override
  State<_LoginStep> createState() => _LoginStepState();
}

class _LoginStepState extends State<_LoginStep> {
  final TextEditingController _userController = TextEditingController(
    text: "admin",
  );
  final TextEditingController _passController = TextEditingController();
  bool _isSubmitting = false;
  bool _obscureText = true;
  String? _error;

  @override
  void dispose() {
    _userController.dispose();
    _passController.dispose();
    super.dispose();
  }

  Future<void> _submit() async {
    setState(() {
      _isSubmitting = true;
      _error = null;
    });

    final success = await widget.onLogin(
      _userController.text,
      _passController.text,
    );

    if (mounted && !success) {
      setState(() {
        _isSubmitting = false;
        _error = "Invalid credentials";
      });
    }
  }

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          TextField(
            controller: _userController,
            autofillHints: const [AutofillHints.username],
            decoration: InputDecoration(
              labelText: "Username",
              border: OutlineInputBorder(
                borderRadius: BorderRadius.circular(12),
              ),
              filled: true,
              fillColor: Colors.white,
            ),
          ),
          const SizedBox(height: 16),
          TextField(
            controller: _passController,
            obscureText: _obscureText,
            autofillHints: const [AutofillHints.password],
            decoration: InputDecoration(
              labelText: "Password",
              border: OutlineInputBorder(
                borderRadius: BorderRadius.circular(12),
              ),
              filled: true,
              fillColor: Colors.white,
              errorText: _error,
              suffixIcon: IconButton(
                icon: Icon(
                  _obscureText
                      ? Icons.visibility_off_outlined
                      : Icons.visibility_outlined,
                  color: PiccoloTheme.inkMuted,
                ),
                onPressed: () => setState(() => _obscureText = !_obscureText),
              ),
            ),
            onSubmitted: (_) => _submit(),
          ),
          const SizedBox(height: 24),
          Row(
            children: [
              TextButton(
                onPressed: widget.onForgotPassword,
                child: const Text("Forgot Password?"),
              ),
              const Spacer(),
              ElevatedButton(
                onPressed: _isSubmitting ? null : _submit,
                style: ElevatedButton.styleFrom(
                  backgroundColor: PiccoloTheme.cobalt600,
                  foregroundColor: Colors.white,
                  padding: const EdgeInsets.symmetric(
                    horizontal: 32,
                    vertical: 16,
                  ),
                  shape: RoundedRectangleBorder(
                    borderRadius: BorderRadius.circular(12),
                  ),
                ),
                child: _isSubmitting
                    ? const SizedBox(
                        width: 20,
                        height: 20,
                        child: CircularProgressIndicator(
                          color: Colors.white,
                          strokeWidth: 2,
                        ),
                      )
                    : const Text("Log In"),
              ),
            ],
          ),
        ],
      ),
    );
  }
}

class _ForgotPasswordStep extends StatefulWidget {
  final Future<bool> Function(String, String) onReset;
  final VoidCallback onCancel;

  const _ForgotPasswordStep({required this.onReset, required this.onCancel});

  @override
  State<_ForgotPasswordStep> createState() => _ForgotPasswordStepState();
}

class _ForgotPasswordStepState extends State<_ForgotPasswordStep> {
  final TextEditingController _keyController = TextEditingController();

  final TextEditingController _passController = TextEditingController();

  final TextEditingController _confirmController = TextEditingController();

  bool _isSubmitting = false;

  String? _generalError;

  String? _confirmError;

  String? _keyError;

  @override
  void dispose() {
    _keyController.dispose();

    _passController.dispose();

    _confirmController.dispose();

    super.dispose();
  }

  Future<void> _submit() async {
    setState(() {
      _generalError = null;

      _confirmError = null;

      _keyError = null;
    });

    if (_keyController.text.trim().isEmpty || _passController.text.isEmpty) {
      setState(() => _generalError = "All fields are required");

      return;
    }

    if (_passController.text != _confirmController.text) {
      setState(() => _confirmError = "Passwords do not match");

      return;
    }

    setState(() {
      _isSubmitting = true;
    });

    final success = await widget.onReset(
      _keyController.text,
      _passController.text,
    );

    if (mounted && !success) {
      setState(() {
        _isSubmitting = false;

        _keyError = "Invalid recovery key";
      });
    }
  }

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),

      child: Column(
        mainAxisSize: MainAxisSize.min,

        crossAxisAlignment: CrossAxisAlignment.stretch,

        children: [
          const Text(
            "Enter your 24-word recovery key to reset your password.",

            style: TextStyle(color: PiccoloTheme.inkMuted, fontSize: 13),
          ),

          const SizedBox(height: 16),

          TextField(
            controller: _keyController,

            maxLines: 3,

            decoration: InputDecoration(
              labelText: "Recovery Key",

              hintText: "word1 word2 ...",

              border: OutlineInputBorder(
                borderRadius: BorderRadius.circular(12),
              ),

              filled: true,

              fillColor: Colors.white,

              errorText: _keyError,
            ),
          ),

          const SizedBox(height: 16),

          Form(
            child: AutofillGroup(
              child: PasswordSetForm(
                passwordController: _passController,

                confirmController: _confirmController,

                passwordLabel: "New Password",

                confirmLabel: "Confirm New Password",

                passwordError: _generalError,

                confirmError: _confirmError,

                onSubmitted: _submit,
              ),
            ),
          ),

          const SizedBox(height: 24),

          Row(
            mainAxisAlignment: MainAxisAlignment.end,

            children: [
              TextButton(
                onPressed: widget.onCancel,

                child: const Text("Cancel"),
              ),

              const SizedBox(width: 16),

              ElevatedButton(
                onPressed: _isSubmitting ? null : _submit,

                style: ElevatedButton.styleFrom(
                  backgroundColor: PiccoloTheme.cobalt600,

                  foregroundColor: Colors.white,

                  padding: const EdgeInsets.symmetric(
                    horizontal: 32,
                    vertical: 16,
                  ),

                  shape: RoundedRectangleBorder(
                    borderRadius: BorderRadius.circular(12),
                  ),
                ),

                child: _isSubmitting
                    ? const SizedBox(
                        width: 20,
                        height: 20,
                        child: CircularProgressIndicator(
                          color: Colors.white,
                          strokeWidth: 2,
                        ),
                      )
                    : const Text("Reset Password"),
              ),
            ],
          ),
        ],
      ),
    );
  }
}

class _ErrorStep extends StatelessWidget {
  final String error;
  final VoidCallback onRetry;

  const _ErrorStep({required this.error, required this.onRetry});

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          const Icon(Icons.error_outline, color: Colors.red, size: 48),
          const SizedBox(height: 16),
          Text(
            error,
            style: const TextStyle(color: Colors.red),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 32),
          ElevatedButton.icon(
            onPressed: onRetry,
            icon: const Icon(Icons.refresh),
            label: const Text("Retry"),
            style: ElevatedButton.styleFrom(
              backgroundColor: PiccoloTheme.mist,
              foregroundColor: PiccoloTheme.ink,
              elevation: 0,
            ),
          ),
        ],
      ),
    );
  }
}
