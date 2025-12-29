import 'package:flutter/material.dart';
import '../../theme/piccolo_theme.dart';
import '../../core/services/app_service.dart';

class DynamicInstallWizard extends StatefulWidget {
  final AppService appService;
  final String appName;
  final String yamlContent;
  final Map<String, dynamic> schema; // The 'inputs' map from backend
  final Function(String appName)? onSuccess;

  const DynamicInstallWizard({
    super.key,
    required this.appService,
    required this.appName,
    required this.yamlContent,
    required this.schema,
    this.onSuccess,
  });

  @override
  State<DynamicInstallWizard> createState() => _DynamicInstallWizardState();
}

class _DynamicInstallWizardState extends State<DynamicInstallWizard> {
  final _formKey = GlobalKey<FormState>();
  final Map<String, dynamic> _formValues = {};
  final TextEditingController _displayNameController = TextEditingController();
  bool _isInstalling = false;
  String? _error;

  @override
  void dispose() {
    _displayNameController.dispose();
    super.dispose();
  }

  @override
  void initState() {
    super.initState();
    // Initialize defaults
    widget.schema.forEach((key, value) {
      if (value is Map && value.containsKey('default')) {
        _formValues[key] = value['default'];
      }
    });
  }

  Future<void> _install() async {
    if (!_formKey.currentState!.validate()) return;
    _formKey.currentState!.save();

    setState(() {
      _isInstalling = true;
      _error = null;
    });

    try {
      final app = await widget.appService.installAppWithInputs(
        widget.yamlContent,
        _formValues,
        displayName: _displayNameController.text.trim(),
      );

      if (mounted) {
        widget.onSuccess?.call(app.name);
        if (widget.onSuccess == null) {
          Navigator.of(context).pop();
        }
      }
    } catch (e) {
      if (mounted) {
        setState(() {
          _isInstalling = false;
          _error = e.toString();
        });
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    return Dialog(
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(16)),
      child: Container(
        width: 600,
        constraints: const BoxConstraints(maxHeight: 700),
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
                  const Icon(Icons.settings_applications, color: PiccoloTheme.cobalt600, size: 32),
                  const SizedBox(width: 16),
                  Expanded(
                    child: Column(
                      crossAxisAlignment: CrossAxisAlignment.start,
                      children: [
                        Text(
                          "Configure ${widget.appName}",
                          style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold),
                        ),
                        Text(
                          "Customize settings before installation",
                          style: PiccoloTheme.textTheme.bodyMedium?.copyWith(color: PiccoloTheme.inkMuted),
                        ),
                      ],
                    ),
                  ),
                  IconButton(
                    icon: const Icon(Icons.close),
                    onPressed: () => Navigator.of(context).pop(),
                  )
                ],
              ),
            ),

            // Form Body
            Expanded(
              child: SingleChildScrollView(
                padding: const EdgeInsets.all(24),
                child: Form(
                  key: _formKey,
                  child: Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      TextFormField(
                        controller: _displayNameController,
                        decoration: const InputDecoration(
                          labelText: "Display name (optional)",
                          hintText: "e.g., Work Projects",
                          filled: true,
                          fillColor: Colors.white,
                          border: OutlineInputBorder(),
                        ),
                      ),
                      const SizedBox(height: 24),
                      if (_error != null)
                        Container(
                          width: double.infinity,
                          margin: const EdgeInsets.only(bottom: 24),
                          padding: const EdgeInsets.all(12),
                          color: PiccoloTheme.critical.withValues(alpha: 0.1),
                          child: Row(
                            children: [
                              const Icon(Icons.error, color: PiccoloTheme.critical, size: 20),
                              const SizedBox(width: 12),
                              Expanded(child: Text(_error!, style: const TextStyle(color: PiccoloTheme.critical))),
                            ],
                          ),
                        ),
                      ...widget.schema.entries.map((entry) => _buildField(entry.key, entry.value)),
                    ],
                  ),
                ),
              ),
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
                  TextButton(
                    onPressed: () => Navigator.of(context).pop(),
                    child: const Text("Cancel"),
                  ),
                  const SizedBox(width: 16),
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

  Widget _buildField(String key, dynamic schema) {
    if (schema is! Map) return const SizedBox.shrink();
    
    final label = schema['label'] ?? key;
    final description = schema['description'] ?? '';
    final type = schema['type'] ?? 'string';
    final required = schema['required'] ?? false;
    final validation = schema['validation']; // Map {regex: "...", message: "..."}

    return Padding(
      padding: const EdgeInsets.only(bottom: 24.0),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text(label, style: const TextStyle(fontWeight: FontWeight.bold)),
          if (description.isNotEmpty) ...[
            const SizedBox(height: 4),
            Text(description, style: PiccoloTheme.textTheme.labelMedium?.copyWith(color: PiccoloTheme.inkMuted)),
          ],
          const SizedBox(height: 8),
          _buildInputWidget(key, type, required, validation, schema['generate'] == true),
        ],
      ),
    );
  }

  Widget _buildInputWidget(String key, String type, bool required, dynamic validation, bool canGenerate) {
    if (type == 'boolean') {
      return FormField<bool>(
        initialValue: _formValues[key] == true,
        builder: (state) {
          return SwitchListTile(
            title: Text(state.value == true ? "Enabled" : "Disabled"),
            value: state.value == true,
            onChanged: (val) {
              state.didChange(val);
              _formValues[key] = val;
            },
            contentPadding: EdgeInsets.zero,
          );
        },
      );
    }

    final isPassword = type == 'password';
    
    return _TextFormFieldWrapper(
      initialValue: _formValues[key]?.toString(),
      isPassword: isPassword,
      canGenerate: canGenerate,
      onSaved: (val) => _formValues[key] = val,
      validator: (val) {
        if (required && (val == null || val.isEmpty)) {
          return "This field is required";
        }
        if (validation is Map && validation['regex'] != null && val != null && val.isNotEmpty) {
           try {
             final reg = RegExp(validation['regex']);
             if (!reg.hasMatch(val)) {
               return validation['message'] ?? "Invalid format";
             }
           } catch (_) {}
        }
        return null;
      },
    );
  }
}

class _TextFormFieldWrapper extends StatefulWidget {
  final String? initialValue;
  final bool isPassword;
  final bool canGenerate;
  final FormFieldSetter<String>? onSaved;
  final FormFieldValidator<String>? validator;

  const _TextFormFieldWrapper({
    this.initialValue,
    required this.isPassword,
    required this.canGenerate,
    this.onSaved,
    this.validator,
  });

  @override
  State<_TextFormFieldWrapper> createState() => _TextFormFieldWrapperState();
}

class _TextFormFieldWrapperState extends State<_TextFormFieldWrapper> {
  late TextEditingController _controller;
  bool _obscureText = true;

  @override
  void initState() {
    super.initState();
    _controller = TextEditingController(text: widget.initialValue);
  }

  @override
  void dispose() {
    _controller.dispose();
    super.dispose();
  }

  void _generate() {
    // Simple client-side generation if user wants to re-roll
    // Real generation happened on backend, but if they clear it and want another...
    // Let's just restore the initial value for now or generate a simple one
    // Ideally we'd call an API, but for now let's just use a simple random string
    final timestamp = DateTime.now().millisecondsSinceEpoch.toRadixString(36);
    final random = (1000 + (DateTime.now().microsecond % 8999)).toString();
    _controller.text = "gen-$timestamp-$random"; 
  }

  @override
  Widget build(BuildContext context) {
    return TextFormField(
      controller: _controller,
      obscureText: widget.isPassword && _obscureText,
      decoration: InputDecoration(
        border: const OutlineInputBorder(),
        filled: true,
        fillColor: Colors.white,
        suffixIcon: Row(
          mainAxisSize: MainAxisSize.min,
          children: [
            if (widget.isPassword)
              IconButton(
                icon: Icon(_obscureText ? Icons.visibility : Icons.visibility_off),
                onPressed: () => setState(() => _obscureText = !_obscureText),
              ),
            if (widget.canGenerate)
               IconButton(
                 icon: const Icon(Icons.refresh),
                 tooltip: "Regenerate",
                 onPressed: _generate,
               ),
          ],
        ),
      ),
      onSaved: widget.onSaved,
      validator: widget.validator,
    );
  }
}
