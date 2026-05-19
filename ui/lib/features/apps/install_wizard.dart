import 'dart:math';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/task_progress.dart';
import 'package:piccolo_os/core/services/api_client.dart' show ApiException;
import 'package:piccolo_os/core/services/app_service.dart';
import 'package:piccolo_os/core/utils/task_id.dart';
import 'package:piccolo_os/shared/widgets/task_progress_panel.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Unified install wizard. Drives both the "Custom App" paste flow (no
/// pre-supplied YAML, opens at the editor stage) and the catalog-tile flow
/// (YAML supplied, opens at the inputs stage when the manifest declares
/// inputs, else at the review stage).
class InstallWizard extends StatefulWidget {
  const InstallWizard({
    required this.appService,
    super.key,
    this.initialYaml,
    this.catalogSource,
    this.displayName,
    this.onSuccess,
  });

  final AppService appService;

  /// When provided, the editor stage is skipped — the wizard treats the YAML
  /// as fixed (catalog flow). When null, the wizard opens in editor mode with
  /// a default boilerplate template (custom-paste flow).
  final String? initialYaml;

  /// Optional catalog item name, used both to seed __app_address__ smart
  /// defaults and to enable init-script fetching at install time.
  final String? catalogSource;

  /// Title-bar display name. Defaults to a sensible label per entry point.
  final String? displayName;

  final void Function(String appName)? onSuccess;

  @override
  State<InstallWizard> createState() => _InstallWizardState();
}

enum _Stage { editor, inputs, review, installing }

class _InstallWizardState extends State<InstallWizard> {
  late final bool _hasFixedYaml;
  late TextEditingController _yamlController;
  final _formKey = GlobalKey<FormState>();
  final Map<String, dynamic> _formValues = {};

  _Stage _stage = _Stage.editor;
  Map<String, dynamic> _schema = const {};
  bool _isBusy = false;
  String? _taskId;
  String? _error;

  // RFC 20260130: use __primary marker for primary listener.
  // __primary will be substituted with the __app_address__ input during install.
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
  void initState() {
    super.initState();
    _hasFixedYaml = widget.initialYaml != null;
    _yamlController = TextEditingController(
      text: widget.initialYaml ?? _defaultTemplate,
    );
    if (_hasFixedYaml) {
      // Catalog flow — fetch schema immediately and route to the right stage.
      _stage = _Stage.inputs; // tentative; corrected after schema arrives
      WidgetsBinding.instance.addPostFrameCallback((_) => _loadSchema());
    }
  }

  @override
  void dispose() {
    _yamlController.dispose();
    super.dispose();
  }

  /// Fetch the input schema for the current YAML and decide whether to land
  /// on the inputs form or skip directly to review.
  Future<void> _loadSchema() async {
    setState(() {
      _isBusy = true;
      _error = null;
    });
    try {
      final schema = await widget.appService.configureManifest(
        _yamlController.text,
        catalogSource: widget.catalogSource,
      );
      if (!mounted) return;
      _seedDefaults(schema);
      setState(() {
        _schema = schema;
        _isBusy = false;
        _stage = schema.isEmpty ? _Stage.review : _Stage.inputs;
      });
    } on ApiException catch (e) {
      if (!mounted) return;
      setState(() {
        _isBusy = false;
        _error = e.message;
        _stage = _Stage.editor;
      });
    } on Object catch (e) {
      if (!mounted) return;
      setState(() {
        _isBusy = false;
        _error = e.toString();
        _stage = _Stage.editor;
      });
    }
  }

  void _seedDefaults(Map<String, dynamic> schema) {
    _formValues.clear();
    schema.forEach((key, value) {
      if (value is! Map) return;
      if (!value.containsKey('default')) return;
      final def = value['default'];
      if (value['type'] == 'array' && def is List) {
        _formValues[key] = def.map((e) => e.toString()).toList();
      } else {
        _formValues[key] = def;
      }
    });
  }

  Future<void> _install() async {
    // _formValues was already validated + saved during the inputs → review
    // transition in _onPrimary; the Form widget is no longer mounted here.
    final taskId = generateTaskId();
    setState(() {
      _isBusy = true;
      _taskId = taskId;
      _stage = _Stage.installing;
      _error = null;
    });
    try {
      await widget.appService.initiateInstall(
        _yamlController.text,
        _formValues,
        taskId: taskId,
        catalogSource: widget.catalogSource,
      );
    } on Object catch (e) {
      if (!mounted) return;
      setState(() {
        _isBusy = false;
        _taskId = null;
        _error = 'Install failed: $e';
        _stage = _recoveryStage;
      });
    }
  }

  void _onInstallComplete(TaskProgressEvent event) {
    if (!mounted) return;
    if (event.error != null && event.error!.isNotEmpty) {
      setState(() {
        _isBusy = false;
        _taskId = null;
        _error = 'Install failed: ${event.error}';
        _stage = _recoveryStage;
      });
      return;
    }
    final appName = event.instanceId ?? '';
    widget.onSuccess?.call(
      appName.isNotEmpty ? appName : (widget.catalogSource ?? 'app'),
    );
  }

  // -- Navigation helpers -----------------------------------------------------

  Future<void> _onPrimary() async {
    switch (_stage) {
      case _Stage.editor:
        await _loadSchema();
      case _Stage.inputs:
        if (!(_formKey.currentState?.validate() ?? false)) return;
        _formKey.currentState!.save();
        setState(() => _stage = _Stage.review);
      case _Stage.review:
        await _install();
      case _Stage.installing:
        break;
    }
  }

  void _onBack() {
    switch (_stage) {
      case _Stage.review:
        if (_schema.isNotEmpty) {
          setState(() => _stage = _Stage.inputs);
        } else if (!_hasFixedYaml) {
          setState(() => _stage = _Stage.editor);
        }
      case _Stage.inputs:
        if (!_hasFixedYaml) setState(() => _stage = _Stage.editor);
      case _Stage.editor:
      case _Stage.installing:
        break;
    }
  }

  bool get _canGoBack {
    switch (_stage) {
      case _Stage.editor:
      case _Stage.installing:
        return false;
      case _Stage.inputs:
        return !_hasFixedYaml;
      case _Stage.review:
        return _schema.isNotEmpty || !_hasFixedYaml;
    }
  }

  String get _primaryLabel {
    switch (_stage) {
      case _Stage.editor:
        return 'Validate & Next';
      case _Stage.inputs:
        return 'Next';
      case _Stage.review:
        return _appLabel == 'your app' ? 'Install' : 'Install $_appLabel';
      case _Stage.installing:
        return '';
    }
  }

  String get _subtitle {
    switch (_stage) {
      case _Stage.editor:
        return 'Paste your app manifest';
      case _Stage.inputs:
        return 'Configure your app';
      case _Stage.review:
        return 'Ready to install';
      case _Stage.installing:
        return 'Installing…';
    }
  }

  /// Where to land after a failed install so the user can recover gracefully.
  _Stage get _recoveryStage {
    if (_schema.isNotEmpty) return _Stage.inputs;
    return _hasFixedYaml ? _Stage.review : _Stage.editor;
  }

  String get _title {
    if (widget.displayName != null && widget.displayName!.isNotEmpty) {
      return 'Install ${widget.displayName}';
    }
    if (widget.catalogSource != null && widget.catalogSource!.isNotEmpty) {
      return 'Install ${widget.catalogSource}';
    }
    return 'Install App';
  }

  // -- Build ------------------------------------------------------------------

  @override
  Widget build(BuildContext context) {
    return Dialog(
      shape: RoundedRectangleBorder(borderRadius: BorderRadius.circular(Radii.lg)),
      child: Container(
        width: 800,
        height: 620,
        decoration: BoxDecoration(
          color: PiccoloTheme.porcelain,
          borderRadius: BorderRadius.circular(Radii.lg),
        ),
        child: Column(
          children: [
            _buildHeader(),
            Expanded(child: _buildBody()),
            _buildFooter(),
          ],
        ),
      ),
    );
  }

  Widget _buildHeader() {
    return Container(
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
                  _title,
                  style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
                    fontWeight: FontWeight.bold,
                  ),
                ),
                Text(
                  _subtitle,
                  style: PiccoloTheme.textTheme.bodyMedium?.copyWith(
                    color: PiccoloTheme.inkMuted,
                  ),
                ),
              ],
            ),
          ),
          IconButton(
            icon: const Icon(PiccoloIcons.close),
            onPressed: _stage == _Stage.installing
                ? null
                : () => Navigator.of(context).pop(),
          ),
        ],
      ),
    );
  }

  Widget _buildBody() {
    if (_stage == _Stage.installing && _taskId != null) {
      return Padding(
        padding: const EdgeInsets.all(Spacing.xl),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            const Icon(PiccoloIcons.download, color: PiccoloTheme.cobalt600, size: 48),
            const SizedBox(height: Spacing.base),
            Text(
              'Installing $_appLabel',
              style: PiccoloTheme.textTheme.displayLarge?.copyWith(fontSize: 22),
            ),
            const SizedBox(height: Spacing.lg),
            Expanded(
              child: TaskProgressPanel(
                taskId: _taskId!,
                taskType: 'install_app',
                onComplete: _onInstallComplete,
              ),
            ),
          ],
        ),
      );
    }
    if (_isBusy && _stage == _Stage.inputs) {
      // Catalog-flow tentative stage while initial schema load is in flight.
      return Center(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            const CircularProgressIndicator(),
            const SizedBox(height: Spacing.lg),
            Text(
              'Preparing $_appLabel…',
              style: PiccoloTheme.textTheme.bodyMedium?.copyWith(
                color: PiccoloTheme.inkMuted,
              ),
            ),
          ],
        ),
      );
    }
    switch (_stage) {
      case _Stage.editor:
        return _buildEditorStep();
      case _Stage.inputs:
        return _buildInputsStep();
      case _Stage.review:
        return _buildReviewStep();
      case _Stage.installing:
        return const SizedBox.shrink();
    }
  }

  Widget _buildErrorBanner() {
    if (_error == null) return const SizedBox.shrink();
    return Container(
      width: double.infinity,
      padding: const EdgeInsets.all(Spacing.md),
      color: PiccoloTheme.critical.withValues(alpha: 0.1),
      child: Row(
        children: [
          const Icon(PiccoloIcons.error, color: PiccoloTheme.critical, size: 20),
          const SizedBox(width: Spacing.md),
          Expanded(
            child: Text(
              _error!,
              style: const TextStyle(color: PiccoloTheme.critical),
            ),
          ),
        ],
      ),
    );
  }

  Widget _buildEditorStep() {
    return Column(
      children: [
        _buildErrorBanner(),
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

  Widget _buildInputsStep() {
    return Column(
      children: [
        _buildErrorBanner(),
        Expanded(
          child: SingleChildScrollView(
            padding: const EdgeInsets.all(Spacing.lg),
            child: Form(
              key: _formKey,
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: _schema.entries
                    .map((entry) => _buildField(entry.key, entry.value))
                    .toList(),
              ),
            ),
          ),
        ),
      ],
    );
  }

  Widget _buildReviewStep() {
    final label = _appLabel;
    final summary = _configuredSummary();
    return Column(
      children: [
        _buildErrorBanner(),
        Expanded(
          child: SingleChildScrollView(
            padding: const EdgeInsets.all(Spacing.xl),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                const Icon(PiccoloIcons.success, color: PiccoloTheme.success, size: 64),
                const SizedBox(height: Spacing.base),
                Text(
                  'Ready to install',
                  style: PiccoloTheme.textTheme.displayLarge?.copyWith(fontSize: 24),
                ),
                const SizedBox(height: Spacing.md),
                Text(
                  "We'll download $label and set it up on your Piccolo. "
                  'This usually takes a minute or two — you can keep using your Piccolo while it runs.',
                  style: PiccoloTheme.textTheme.bodyMedium,
                ),
                if (summary.isNotEmpty) ...[
                  const SizedBox(height: Spacing.lg),
                  Container(
                    padding: const EdgeInsets.all(Spacing.base),
                    decoration: BoxDecoration(
                      color: PiccoloTheme.mist,
                      borderRadius: BorderRadius.circular(Radii.sm),
                      border: Border.all(color: PiccoloTheme.hairline),
                    ),
                    child: Column(
                      crossAxisAlignment: CrossAxisAlignment.start,
                      children: [
                        Text(
                          'You configured',
                          style: PiccoloTheme.textTheme.bodyMedium?.copyWith(
                            color: PiccoloTheme.inkMuted,
                            fontWeight: FontWeight.w600,
                          ),
                        ),
                        const SizedBox(height: Spacing.sm),
                        ...summary.map(
                          (e) => Padding(
                            padding: const EdgeInsets.symmetric(vertical: Spacing.xs),
                            child: Row(
                              crossAxisAlignment: CrossAxisAlignment.start,
                              children: [
                                SizedBox(
                                  width: 160,
                                  child: Text(
                                    e.label,
                                    style: PiccoloTheme.textTheme.bodyMedium?.copyWith(
                                      color: PiccoloTheme.inkMuted,
                                    ),
                                  ),
                                ),
                                Expanded(
                                  child: Text(
                                    e.value,
                                    style: PiccoloTheme.textTheme.bodyMedium,
                                  ),
                                ),
                              ],
                            ),
                          ),
                        ),
                      ],
                    ),
                  ),
                ],
              ],
            ),
          ),
        ),
      ],
    );
  }

  List<_ConfiguredEntry> _configuredSummary() {
    if (_schema.isEmpty) return const [];
    final entries = <_ConfiguredEntry>[];
    _schema.forEach((key, raw) {
      if (raw is! Map) return;
      final value = _formValues[key];
      if (value == null) return;
      final type = (raw['type'] as String?) ?? 'string';
      final label = (raw['label'] as String?) ?? key;
      final display = _renderInputValue(type, value);
      if (display == null || display.isEmpty) return;
      entries.add(_ConfiguredEntry(label: label, value: display));
    });
    return entries;
  }

  String? _renderInputValue(String type, dynamic value) {
    switch (type) {
      case 'password':
        final s = value is String ? value : value.toString();
        return s.isEmpty ? null : '••••••• (set)';
      case 'boolean':
        return value == true || value == 'true' ? 'Enabled' : 'Disabled';
      case 'array':
        if (value is List) {
          final items = value.map((e) => e.toString()).where((s) => s.isNotEmpty).toList();
          return items.isEmpty ? null : items.join(', ');
        }
        return null;
      default:
        final s = value is String ? value : value.toString();
        return s.isEmpty ? null : s;
    }
  }

  String get _appLabel {
    if (widget.displayName != null && widget.displayName!.isNotEmpty) {
      return widget.displayName!;
    }
    if (widget.catalogSource != null && widget.catalogSource!.isNotEmpty) {
      return widget.catalogSource!;
    }
    return 'your app';
  }

  Widget _buildFooter() {
    return Container(
      padding: const EdgeInsets.all(Spacing.lg),
      decoration: const BoxDecoration(
        border: Border(top: BorderSide(color: PiccoloTheme.hairline)),
      ),
      child: Row(
        mainAxisAlignment: MainAxisAlignment.end,
        children: [
          if (_canGoBack)
            TextButton(
              onPressed: _isBusy ? null : _onBack,
              child: const Text('Back'),
            )
          else if (_stage == _Stage.inputs || _stage == _Stage.review)
            TextButton(
              onPressed: _isBusy ? null : () => Navigator.of(context).pop(),
              child: const Text('Cancel'),
            ),
          const SizedBox(width: Spacing.base),
          if (_stage != _Stage.installing)
            FilledButton(
              onPressed: _isBusy ? null : _onPrimary,
              style: _stage == _Stage.review
                  ? FilledButton.styleFrom(backgroundColor: PiccoloTheme.success)
                  : null,
              child: _isBusy
                  ? const SizedBox(
                      width: 20,
                      height: 20,
                      child: CircularProgressIndicator(
                        strokeWidth: 2,
                        color: Colors.white,
                      ),
                    )
                  : Text(_primaryLabel),
            ),
        ],
      ),
    );
  }

  // -- Input field rendering --------------------------------------------------

  Widget _buildField(String key, dynamic schema) {
    if (schema is! Map) return const SizedBox.shrink();
    final label = (schema['label'] as String?) ?? key;
    final description = (schema['description'] as String?) ?? '';
    final type = (schema['type'] as String?) ?? 'string';
    final required = (schema['required'] as bool?) ?? false;
    final validation = schema['validation'] as Map<String, dynamic>?;
    return Padding(
      padding: const EdgeInsets.only(bottom: Spacing.lg),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Text(label, style: const TextStyle(fontWeight: FontWeight.bold)),
          if (description.isNotEmpty) ...[
            const SizedBox(height: Spacing.xs),
            Text(
              description,
              style: PiccoloTheme.textTheme.labelMedium?.copyWith(
                color: PiccoloTheme.inkMuted,
              ),
            ),
          ],
          const SizedBox(height: Spacing.sm),
          _buildInputWidget(
            key,
            type,
            required,
            validation,
            schema['generate'] == true,
          ),
        ],
      ),
    );
  }

  Widget _buildInputWidget(
    String key,
    String type,
    bool required,
    Map<String, dynamic>? validation,
    bool canGenerate,
  ) {
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
                  child: Text(
                    state.errorText!,
                    style: const TextStyle(
                      color: PiccoloTheme.critical,
                      fontSize: 12,
                    ),
                  ),
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
        if (validation != null &&
            validation['regex'] != null &&
            val != null &&
            val.isNotEmpty) {
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
    required this.isPassword,
    required this.canGenerate,
    this.initialValue,
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
    // 20 alphanumeric chars from a cryptographically-secure RNG. The backend
    // also pre-fills passwords on generate:true inputs (see PrepareSmartDefaults);
    // this fires only when the user clicks the regenerate button.
    const charset = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789';
    final rng = Random.secure();
    _controller.text = List.generate(
      20,
      (_) => charset[rng.nextInt(charset.length)],
    ).join();
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
                icon: Icon(
                  _obscureText ? PiccoloIcons.visibility : PiccoloIcons.visibilityOff,
                ),
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
        ...List.generate(
          _controllers.length,
          (i) => Padding(
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
          ),
        ),
        TextButton.icon(
          icon: const Icon(PiccoloIcons.add, size: 16),
          label: const Text('Add item'),
          onPressed: _addItem,
        ),
      ],
    );
  }
}

class _ConfiguredEntry {
  const _ConfiguredEntry({required this.label, required this.value});
  final String label;
  final String value;
}
