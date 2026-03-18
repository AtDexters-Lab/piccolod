import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/task_progress.dart';
import 'package:piccolo_os/core/services/app_service.dart';
import 'package:piccolo_os/core/utils/task_id.dart';
import 'package:piccolo_os/shared/widgets/task_progress_panel.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class DynamicInstallWizard extends StatefulWidget {

  const DynamicInstallWizard({
    required this.appService, required this.appName, required this.yamlContent, required this.schema, super.key,
    this.onSuccess,
  });
  final AppService appService;
  final String appName;
  final String yamlContent;
  final Map<String, dynamic> schema; // The 'inputs' map from backend
  final void Function(String appName)? onSuccess;

  @override
  State<DynamicInstallWizard> createState() => _DynamicInstallWizardState();
}

class _DynamicInstallWizardState extends State<DynamicInstallWizard> {
  final _formKey = GlobalKey<FormState>();
  final Map<String, dynamic> _formValues = {};
  bool _isInstalling = false;
  String? _taskId;
  String? _error;

  @override
  void dispose() {
    super.dispose();
  }

  @override
  void initState() {
    super.initState();
    // Initialize defaults
    widget.schema.forEach((key, value) {
      if (value is Map && value.containsKey('default')) {
        final def = value['default'];
        if (value['type'] == 'array' && def is List) {
          _formValues[key] = def.map((e) => e.toString()).toList();
        } else {
          _formValues[key] = def;
        }
      }
    });
  }

  Future<void> _install() async {
    if (!_formKey.currentState!.validate()) return;
    _formKey.currentState!.save();

    final taskId = generateTaskId();
    setState(() {
      _isInstalling = true;
      _taskId = taskId;
      _error = null;
    });

    try {
      await widget.appService.initiateInstall(
        widget.yamlContent,
        _formValues,
        taskId: taskId,
        catalogSource: widget.appName,
      );
    } on Object catch (e) {
      if (mounted) {
        setState(() {
          _isInstalling = false;
          _taskId = null;
          _error = e.toString();
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
        _error = event.error;
      });
      return;
    }
    final appName = event.instanceId ?? '';
    widget.onSuccess?.call(appName.isNotEmpty ? appName : widget.appName);
  }

  @override
  Widget build(BuildContext context) {
    return Dialog(
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(Radii.lg)),
      child: Container(
        width: 600,
        constraints: const BoxConstraints(maxHeight: 700),
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
                  const Icon(PiccoloIcons.settingsApp, color: PiccoloTheme.cobalt600, size: 32),
                  const SizedBox(width: Spacing.base),
                  Expanded(
                    child: Column(
                      crossAxisAlignment: CrossAxisAlignment.start,
                      children: [
                        Text(
                          'Configure ${widget.appName}',
                          style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold),
                        ),
                        Text(
                          'Customize settings before installation',
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

            // Form Body
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
                  : SingleChildScrollView(
                      padding: const EdgeInsets.all(Spacing.lg),
                      child: Form(
                        key: _formKey,
                        child: Column(
                          crossAxisAlignment: CrossAxisAlignment.start,
                          children: [
                            if (_error != null)
                              Container(
                                width: double.infinity,
                                margin: const EdgeInsets.only(bottom: Spacing.lg),
                                padding: const EdgeInsets.all(Spacing.md),
                                color: PiccoloTheme.critical.withValues(alpha: 0.1),
                                child: Row(
                                  children: [
                                    const Icon(PiccoloIcons.error, color: PiccoloTheme.critical, size: 20),
                                    const SizedBox(height: Spacing.md, width: Spacing.md),
                                    Expanded(child: Text(_error!, style: const TextStyle(color: PiccoloTheme.critical))),
                                  ],
                                ),
                              ),
                            ...widget.schema.entries
                                .map((entry) => _buildField(entry.key, entry.value)),
                          ],
                        ),
                      ),
                    ),
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
                  TextButton(
                    onPressed: _isInstalling ? null : () => Navigator.of(context).pop(),
                    child: const Text('Cancel'),
                  ),
                  const SizedBox(width: Spacing.base),
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

  Widget _buildField(String key, dynamic schema) {
    if (schema is! Map) return const SizedBox.shrink();

    final label = (schema['label'] as String?) ?? key;
    final description = (schema['description'] as String?) ?? '';
    final type = (schema['type'] as String?) ?? 'string';
    final required = (schema['required'] as bool?) ?? false;
    final validation = schema['validation'] as Map<String, dynamic>?; // Map {regex: "...", message: "..."}

    return Padding(
      padding: const EdgeInsets.only(bottom: Spacing.lg),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text(label, style: const TextStyle(fontWeight: FontWeight.bold)),
          if (description.isNotEmpty) ...[
            const SizedBox(height: Spacing.xs),
            Text(description, style: PiccoloTheme.textTheme.labelMedium?.copyWith(color: PiccoloTheme.inkMuted)),
          ],
          const SizedBox(height: Spacing.sm),
          _buildInputWidget(key, type, required, validation, schema['generate'] == true),
        ],
      ),
    );
  }

  Widget _buildInputWidget(String key, String type, bool required, Map<String, dynamic>? validation, bool canGenerate) {
    if (type == 'array') {
      return FormField<List<String>>(
        initialValue: (_formValues[key] as List<String>?) ?? <String>[],
        onSaved: (val) => _formValues[key] = val ?? <String>[],
        validator: (val) {
          if (required && (val == null || val.isEmpty)) {
            return 'At least one item is required';
          }
          return null;
        },
        builder: (state) {
          return Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              _ArrayInputWidget(
                initialValues: state.value ?? <String>[],
                onChanged: (values) {
                  state.didChange(values);
                  _formValues[key] = values;
                },
              ),
              if (state.hasError)
                Padding(
                  padding: const EdgeInsets.only(top: Spacing.xs),
                  child: Text(state.errorText!, style: const TextStyle(color: PiccoloTheme.critical, fontSize: 12)),
                ),
            ],
          );
        },
      );
    }

    if (type == 'boolean') {
      return FormField<bool>(
        initialValue: _formValues[key] == true,
        builder: (state) {
          return SwitchListTile(
            title: Text(state.value ?? false ? 'Enabled' : 'Disabled'),
            value: state.value ?? false,
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
          return 'This field is required';
        }
        if (validation != null && validation['regex'] != null && val != null && val.isNotEmpty) {
           try {
             final reg = RegExp(validation['regex'] as String);
             if (!reg.hasMatch(val)) {
               return (validation['message'] as String?) ?? 'Invalid format';
             }
           } on Object catch (_) {}
        }
        return null;
      },
    );
  }
}

class _TextFormFieldWrapper extends StatefulWidget {

  const _TextFormFieldWrapper({
    required this.isPassword, required this.canGenerate, this.initialValue,
    this.onSaved,
    this.validator,
  });
  final String? initialValue;
  final bool isPassword;
  final bool canGenerate;
  final FormFieldSetter<String>? onSaved;
  final FormFieldValidator<String>? validator;

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
    _controller.text = 'gen-$timestamp-$random';
  }

  @override
  Widget build(BuildContext context) {
    return TextFormField(
      controller: _controller,
      obscureText: widget.isPassword && _obscureText,
      decoration: InputDecoration(
        border: const OutlineInputBorder(),
        filled: true,
        fillColor: PiccoloTheme.porcelain,
        suffixIcon: Row(
          mainAxisSize: MainAxisSize.min,
          children: [
            if (widget.isPassword)
              IconButton(
                icon: Icon(_obscureText ? PiccoloIcons.visibility : PiccoloIcons.visibilityOff),
                onPressed: () => setState(() => _obscureText = !_obscureText),
              ),
            if (widget.canGenerate)
               IconButton(
                 icon: const Icon(PiccoloIcons.refresh),
                 tooltip: 'Regenerate',
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

class _ArrayInputWidget extends StatefulWidget {

  const _ArrayInputWidget({required this.initialValues, required this.onChanged});
  final List<String> initialValues;
  final ValueChanged<List<String>> onChanged;

  @override
  State<_ArrayInputWidget> createState() => _ArrayInputWidgetState();
}

class _ArrayInputWidgetState extends State<_ArrayInputWidget> {
  late List<TextEditingController> _controllers;

  @override
  void initState() {
    super.initState();
    _controllers = widget.initialValues
        .map((v) => TextEditingController(text: v))
        .toList();
    if (_controllers.isEmpty) {
      _controllers.add(TextEditingController());
    }
  }

  @override
  void dispose() {
    for (final c in _controllers) {
      c.dispose();
    }
    super.dispose();
  }

  void _notify() {
    widget.onChanged(
      _controllers.map((c) => c.text).where((s) => s.isNotEmpty).toList(),
    );
  }

  void _addItem() {
    setState(() {
      _controllers.add(TextEditingController());
    });
  }

  void _removeItem(int index) {
    final controller = _controllers[index];
    _controllers.removeAt(index);
    controller.dispose();
    if (_controllers.isEmpty) {
      _controllers.add(TextEditingController());
    }
    _notify();
    setState(() {});
  }

  @override
  Widget build(BuildContext context) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        ...List.generate(_controllers.length, (i) => Padding(
          padding: const EdgeInsets.only(bottom: Spacing.sm),
          child: Row(
            children: [
              Expanded(
                child: TextField(
                  controller: _controllers[i],
                  decoration: const InputDecoration(
                    border: OutlineInputBorder(),
                    filled: true,
                    fillColor: PiccoloTheme.porcelain,
                    isDense: true,
                  ),
                  onChanged: (_) => _notify(),
                ),
              ),
              const SizedBox(width: Spacing.sm),
              IconButton(
                icon: const Icon(PiccoloIcons.delete, size: 18),
                onPressed: () => _removeItem(i),
                tooltip: 'Remove',
                iconSize: 18,
                padding: const EdgeInsets.all(Spacing.xs),
                constraints: const BoxConstraints(),
              ),
            ],
          ),
        )),
        TextButton.icon(
          icon: const Icon(PiccoloIcons.add, size: 16),
          label: const Text('Add item'),
          onPressed: _addItem,
        ),
      ],
    );
  }
}
