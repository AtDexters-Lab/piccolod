import 'dart:async';
import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/app_models.dart';
import 'package:piccolo_os/core/models/task_progress.dart';
import 'package:piccolo_os/core/services/app_service.dart';
import 'package:piccolo_os/core/utils/task_id.dart';
import 'package:piccolo_os/features/apps/apply_settlement.dart';
import 'package:piccolo_os/features/apps/update_error_messages.dart';
import 'package:piccolo_os/shared/widgets/task_progress_panel.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class ManifestUpdateWizard extends StatefulWidget {
  const ManifestUpdateWizard({
    required this.appId,
    required this.appService,
    required this.onApplied,
    super.key,
    this.catalogPending = false,
    this.onTaskStarted,
  });

  final String appId;
  final AppService appService;
  final Future<void> Function() onApplied;
  final bool catalogPending;
  final void Function(String taskId, String taskType, String label)?
  onTaskStarted;

  @override
  State<ManifestUpdateWizard> createState() => _ManifestUpdateWizardState();
}

bool manifestUpdateShouldSubmitField(
  ManifestUpdateInputField field, {
  required bool touched,
  required bool hasDefault,
}) {
  if (touched) return true;
  if (field.type == 'boolean' && field.required) return true;
  return field.required && hasDefault;
}

class _ManifestUpdateWizardState extends State<ManifestUpdateWizard> {
  final TextEditingController _yamlController = TextEditingController();
  final ScrollController _scrollController = ScrollController();
  final GlobalKey _dryRunSummaryKey = GlobalKey();
  final GlobalKey _taskProgressKey = GlobalKey();
  final GlobalKey _errorKey = GlobalKey();
  final Map<String, TextEditingController> _inputControllers = {};
  final Set<String> _regenerateInputs = {};
  final Set<String> _replaceInputs = {};
  final Set<String> _clearInputs = {};
  final Set<String> _touchedInputs = {};
  final Set<String> _confirmedReviewItems = {};

  ManifestUpdateConfigureResult? _configure;
  ManifestUpdateResult? _dryRun;
  String? _error;
  bool _busy = false;
  bool _dryRunning = false;
  String? _taskId;
  String? _accessRepairMessage;
  bool _applied = false;
  bool _applyResponsePending = false;
  bool _applyTaskSucceeded = false;

  @override
  void initState() {
    super.initState();
    if (widget.catalogPending) {
      WidgetsBinding.instance.addPostFrameCallback((_) {
        if (mounted) unawaited(_prepare());
      });
    }
  }

  @override
  void dispose() {
    _yamlController.dispose();
    _scrollController.dispose();
    for (final controller in _inputControllers.values) {
      controller.dispose();
    }
    super.dispose();
  }

  Future<void> _prepare() async {
    final yaml = _yamlController.text.trim();
    if (yaml.isEmpty && !widget.catalogPending) {
      setState(() => _error = 'Manifest YAML is required.');
      _revealError();
      return;
    }
    setState(() {
      _busy = true;
      _error = null;
      _dryRun = null;
      _accessRepairMessage = null;
      _confirmedReviewItems.clear();
      _configure = null;
      _regenerateInputs.clear();
      _replaceInputs.clear();
      _clearInputs.clear();
      _touchedInputs.clear();
      for (final controller in _inputControllers.values) {
        controller.dispose();
      }
      _inputControllers.clear();
    });
    try {
      final result = await widget.appService.configureManifestUpdate(
        widget.appId,
        yaml,
        catalogPending: widget.catalogPending,
      );
      for (final field in result.fields) {
        final raw = result.inputs[field.name];
        final schema = raw is Map<dynamic, dynamic>
            ? Map<String, dynamic>.from(raw)
            : <String, dynamic>{};
        final text = _initialTextForField(field, schema);
        _inputControllers[field.name] = TextEditingController(text: text);
      }
      if (!mounted) return;
      setState(() => _configure = result);
    } on Object catch (e) {
      if (!mounted) return;
      setState(() => _error = updateErrorMessage(e));
      _revealError();
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  Future<void> _dryRunUpdate() async {
    final configure = _configure;
    if (configure == null) return;
    final inputs = <String, dynamic>{};
    for (final field in configure.fields) {
      if (_regenerateInputs.contains(field.name)) continue;
      if (_clearInputs.contains(field.name)) continue;
      if (field.hasCurrentValue && !_replaceInputs.contains(field.name)) {
        continue;
      }
      if (_replaceInputs.contains(field.name)) {
        if (_isSensitiveCurrentField(field) &&
            (_inputControllers[field.name]?.text.trim().isEmpty ?? true)) {
          final recovery = field.required
              ? 'keep the current value'
              : 'keep the current value, or choose Clear value';
          setState(() {
            _error =
                'Enter a replacement value for ${field.name}, or $recovery.';
          });
          _revealError();
          return;
        }
        inputs[field.name] = _valueForField(field);
        continue;
      }
      if (!manifestUpdateShouldSubmitField(
        field,
        touched: _touchedInputs.contains(field.name),
        hasDefault: _fieldHasDefault(configure, field.name),
      )) {
        continue;
      }
      final value = _valueForField(field);
      inputs[field.name] = value;
    }
    setState(() {
      _busy = true;
      _dryRunning = true;
      _error = null;
      _dryRun = null;
      _confirmedReviewItems.clear();
    });
    try {
      final result = await widget.appService.dryRunManifestUpdate(
        widget.appId,
        widget.catalogPending ? '' : _yamlController.text,
        inputs,
        _regenerateInputs.toList(),
        clearInputs: _clearInputs.toList(),
        catalogPending: widget.catalogPending,
      );
      if (!mounted) return;
      setState(() {
        _dryRun = result;
        _confirmedReviewItems.clear();
      });
      _revealDryRunSummary();
    } on Object catch (e) {
      if (!mounted) return;
      setState(() => _error = updateErrorMessage(e));
      _revealError();
    } finally {
      if (mounted) {
        setState(() {
          _busy = false;
          _dryRunning = false;
        });
      }
    }
  }

  void _revealDryRunSummary() {
    WidgetsBinding.instance.addPostFrameCallback((_) {
      final context = _dryRunSummaryKey.currentContext;
      if (!mounted || context == null) return;
      unawaited(
        Scrollable.ensureVisible(
          context,
          duration: const Duration(milliseconds: 220),
          curve: Curves.easeOutCubic,
          alignment: 0.08,
        ),
      );
    });
  }

  void _revealTaskProgress() {
    WidgetsBinding.instance.addPostFrameCallback((_) {
      final context = _taskProgressKey.currentContext;
      if (!mounted || context == null) return;
      unawaited(
        Scrollable.ensureVisible(
          context,
          duration: const Duration(milliseconds: 220),
          curve: Curves.easeOutCubic,
          alignment: 0.08,
        ),
      );
    });
  }

  void _revealError() {
    WidgetsBinding.instance.addPostFrameCallback((_) {
      final context = _errorKey.currentContext;
      if (!mounted || context == null) return;
      unawaited(
        Scrollable.ensureVisible(
          context,
          duration: const Duration(milliseconds: 220),
          curve: Curves.easeOutCubic,
          alignment: 0.08,
        ),
      );
    });
  }

  Object _valueForField(ManifestUpdateInputField field) {
    final text = _inputControllers[field.name]?.text ?? '';
    switch (field.type) {
      case 'boolean':
        return text == 'true';
      case 'int':
        return int.tryParse(text) ?? 0;
      case 'number':
        return double.tryParse(text) ?? 0;
      case 'array':
        return text
            .split(',')
            .map((e) => e.trim())
            .where((e) => e.isNotEmpty)
            .toList();
      default:
        return text;
    }
  }

  String _initialTextForField(
    ManifestUpdateInputField field,
    Map<String, dynamic> schema,
  ) {
    if (field.hasCurrentValue &&
        !_isSensitiveCurrentField(field) &&
        field.currentValueDisplay.trim().isNotEmpty) {
      return field.currentValueDisplay.trim();
    }
    if (!schema.containsKey('default')) return '';
    final value = schema['default'];
    if (value == null || field.type == 'password' || field.generate) {
      return '';
    }
    if (field.type == 'array' && value is Iterable) {
      return value.map((item) => item.toString()).join(', ');
    }
    return value.toString();
  }

  bool _fieldHasDefault(
    ManifestUpdateConfigureResult configure,
    String name,
  ) {
    final raw = configure.inputs[name];
    if (raw is! Map<dynamic, dynamic>) return false;
    return raw.containsKey('default');
  }

  void _markInputTouched(String name) {
    _touchedInputs.add(name);
    _invalidateDryRun();
  }

  void _setCurrentValueAction(ManifestUpdateInputField field, String action) {
    setState(() {
      _dryRun = null;
      _confirmedReviewItems.clear();
      _touchedInputs.remove(field.name);
      _replaceInputs.remove(field.name);
      _regenerateInputs.remove(field.name);
      _clearInputs.remove(field.name);
      if (action == 'replace') {
        _replaceInputs.add(field.name);
      } else {
        _inputControllers[field.name]?.clear();
        if (action == 'regenerate') {
          _regenerateInputs.add(field.name);
        } else if (action == 'clear') {
          _clearInputs.add(field.name);
        }
      }
    });
  }

  void _invalidateDryRun() {
    if (_dryRun == null || _taskId != null) return;
    setState(() {
      _dryRun = null;
      _confirmedReviewItems.clear();
      _accessRepairMessage = null;
    });
  }

  bool _allConfirmationsAccepted(ManifestUpdateResult result) {
    return result.requiredConfirmations.every(_confirmedReviewItems.contains);
  }

  Future<void> _apply() async {
    final dryRun = _dryRun;
    if (dryRun == null || !dryRun.applicable || dryRun.dryRunToken.isEmpty) {
      return;
    }
    if (!_allConfirmationsAccepted(dryRun)) return;
    final taskId = generateTaskId();
    setState(() {
      _busy = true;
      _error = null;
      _accessRepairMessage = null;
      _taskId = taskId;
      _applyResponsePending = true;
      _applyTaskSucceeded = false;
    });
    widget.onTaskStarted?.call(
      taskId,
      'update_service_app',
      widget.catalogPending ? 'Updating app' : 'Modifying app',
    );
    _revealTaskProgress();
    try {
      final result = await widget.appService.applyManifestUpdate(
        widget.appId,
        dryRun,
        taskId: taskId,
        confirmations: _confirmedReviewItems.toList(),
        catalogPending: widget.catalogPending,
      );
      if (!mounted || _applied) return;
      _applyResponsePending = false;
      if (result.accessRepairPending) {
        _applied = true;
        await widget.onApplied();
        if (!mounted) return;
        setState(() {
          _busy = false;
          _taskId = null;
          _dryRun = result;
          _accessRepairMessage = result.accessRepairMessage.isEmpty
              ? 'Update committed, but access publication needs repair.'
              : result.accessRepairMessage;
        });
        _revealDryRunSummary();
        return;
      }
      await _finishApply();
    } on Object catch (e) {
      if (!mounted) return;
      _applyResponsePending = false;
      if (shouldFinishApplyFromTaskSuccess(
        taskSucceeded: _applyTaskSucceeded,
        alreadyApplied: _applied,
      )) {
        await _finishApply();
        return;
      }
      if (isStaleUpdatePreviewError(e)) {
        setState(() {
          _busy = false;
          _taskId = null;
          _dryRun = null;
          _accessRepairMessage = null;
          _confirmedReviewItems.clear();
          _error = staleUpdatePreviewMessage;
        });
        _revealError();
        return;
      }
      setState(() {
        _busy = false;
        _taskId = null;
        _error = updateErrorMessage(e);
      });
      _revealError();
    }
  }

  Future<void> _completeApply(TaskProgressEvent event) async {
    if (_applied) return;
    if (_applyResponsePending) {
      _applyTaskSucceeded = event.error == null || event.error!.isEmpty;
      return;
    }
    if (event.error != null && event.error!.isNotEmpty) {
      if (!mounted) return;
      setState(() {
        _busy = false;
        _taskId = null;
        _applyResponsePending = false;
        _error = event.error;
      });
      _revealError();
      return;
    }
    await _finishApply();
  }

  Future<void> _finishApply() async {
    if (_applied) return;
    _applied = true;
    await widget.onApplied();
    if (mounted) Navigator.of(context).pop();
  }

  @override
  Widget build(BuildContext context) {
    final remainingConfirmations = _dryRun == null
        ? 0
        : _dryRun!.requiredConfirmations
              .where((item) => !_confirmedReviewItems.contains(item))
              .length;
    return AlertDialog(
      title: Text(widget.catalogPending ? 'Update' : 'Modify App'),
      content: SizedBox(
        width: 760,
        child: ConstrainedBox(
          constraints: const BoxConstraints(maxHeight: 720),
          child: SingleChildScrollView(
            controller: _scrollController,
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.stretch,
              mainAxisSize: MainAxisSize.min,
              children: [
                if (widget.catalogPending)
                  const Padding(
                    padding: EdgeInsets.only(bottom: Spacing.md),
                    child: _ManifestContextBanner(),
                  ),
                if (!widget.catalogPending) ...[
                  const Text(
                    'Paste the full replacement manifest. Piccolo keeps this app identity and reuses stored values where it can, without showing stored secrets.',
                  ),
                  const SizedBox(height: Spacing.xs),
                  Text(
                    'Source: pasted manifest YAML',
                    style: PiccoloTheme.textTheme.labelSmall,
                  ),
                  const SizedBox(height: Spacing.sm),
                  TextField(
                    controller: _yamlController,
                    minLines: 8,
                    maxLines: 14,
                    enabled: !_busy && _taskId == null,
                    style: const TextStyle(
                      fontFamily: 'JetBrainsMono',
                      fontSize: 12,
                    ),
                    decoration: const InputDecoration(
                      labelText: 'Full manifest YAML',
                      border: OutlineInputBorder(),
                      alignLabelWithHint: true,
                    ),
                    onChanged: _handleSourceChanged,
                  ),
                  const SizedBox(height: Spacing.base),
                  Align(
                    alignment: Alignment.centerLeft,
                    child: FilledButton.icon(
                      onPressed: _busy || _taskId != null ? null : _prepare,
                      icon: const Icon(PiccoloIcons.fileText),
                      label: const Text('Continue'),
                    ),
                  ),
                ],
                if (_configure != null) ...[
                  const SizedBox(height: Spacing.lg),
                  _buildInputForm(_configure!),
                ] else if (widget.catalogPending &&
                    _busy &&
                    _error == null &&
                    _taskId == null) ...[
                  const SizedBox(height: Spacing.lg),
                  const _LoadingCatalogUpdate(),
                ],
                if (_dryRun != null) ...[
                  const SizedBox(height: Spacing.lg),
                  if (_accessRepairMessage != null)
                    _Banner(
                      icon: PiccoloIcons.warning,
                      color: PiccoloTheme.warning,
                      text: _accessRepairMessage!,
                    ),
                  KeyedSubtree(
                    key: _dryRunSummaryKey,
                    child: _buildDryRunSummary(_dryRun!),
                  ),
                ],
                if (_taskId != null) ...[
                  const SizedBox(height: Spacing.lg),
                  KeyedSubtree(
                    key: _taskProgressKey,
                    child: TaskProgressPanel(
                      taskId: _taskId!,
                      taskType: 'update_service_app',
                      label: widget.catalogPending
                          ? 'Updating app'
                          : 'Modifying app',
                      onComplete: (evt) => unawaited(_completeApply(evt)),
                    ),
                  ),
                ],
                if (_error != null) ...[
                  const SizedBox(height: Spacing.base),
                  KeyedSubtree(
                    key: _errorKey,
                    child: Text(
                      _error!,
                      style: const TextStyle(color: PiccoloTheme.critical),
                    ),
                  ),
                ],
              ],
            ),
          ),
        ),
      ),
      actions: [
        if (remainingConfirmations > 0 && _taskId == null)
          Padding(
            padding: const EdgeInsets.only(right: Spacing.sm),
            child: Text(
              '$remainingConfirmations required ${remainingConfirmations == 1 ? 'review' : 'reviews'} left',
              style: PiccoloTheme.textTheme.bodySmall?.copyWith(
                color: PiccoloTheme.inkMuted,
              ),
            ),
          ),
        TextButton(
          onPressed: () => Navigator.of(context).pop(),
          child: Text(_taskId != null || _applied ? 'Close' : 'Cancel'),
        ),
        FilledButton.icon(
          onPressed:
              _busy ||
                  _applied ||
                  _dryRun == null ||
                  !_dryRun!.applicable ||
                  !_allConfirmationsAccepted(_dryRun!) ||
                  _taskId != null
              ? null
              : _apply,
          icon: _taskId == null
              ? const Icon(PiccoloIcons.check)
              : const SizedBox(
                  width: 18,
                  height: 18,
                  child: CircularProgressIndicator(strokeWidth: 2),
                ),
          label: Text(_taskId == null ? _applyLabel : '$_applyLabel...'),
        ),
      ],
    );
  }

  Widget _buildInputForm(ManifestUpdateConfigureResult configure) {
    if (!configure.eligible) {
      return _Banner(
        icon: PiccoloIcons.warning,
        color: PiccoloTheme.warning,
        text: configure.blockingReason,
      );
    }
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        if (configure.secretGeneratedPreflight.isNotEmpty)
          _Banner(
            icon: PiccoloIcons.lockKey,
            color: PiccoloTheme.warning,
            text: _secretPreflightText(configure),
          ),
        ...configure.fields.map(_buildField),
        const SizedBox(height: Spacing.base),
        Align(
          alignment: Alignment.centerLeft,
          child: OutlinedButton.icon(
            onPressed: _busy ? null : _dryRunUpdate,
            icon: _dryRunning
                ? const SizedBox(
                    width: 18,
                    height: 18,
                    child: CircularProgressIndicator(strokeWidth: 2),
                  )
                : const Icon(PiccoloIcons.search),
            label: Text(_dryRunning ? 'Previewing Changes' : 'Preview Changes'),
          ),
        ),
        if (_dryRunning || _dryRun != null) ...[
          const SizedBox(height: Spacing.sm),
          _DryRunStatusStrip(
            running: _dryRunning,
            result: _dryRun,
            remainingConfirmations: _dryRun == null
                ? 0
                : _dryRun!.requiredConfirmations
                      .where((item) => !_confirmedReviewItems.contains(item))
                      .length,
            onViewDetails: _dryRun == null ? null : _revealDryRunSummary,
          ),
        ],
      ],
    );
  }

  Widget _buildField(ManifestUpdateInputField field) {
    if (field.hasCurrentValue && !field.locked) {
      return _buildCurrentValueField(field);
    }
    if (field.generate && !field.locked) {
      final regenerating = _regenerateInputs.contains(field.name);
      return Padding(
        padding: const EdgeInsets.only(bottom: Spacing.sm),
        child: Column(
          children: [
            TextField(
              controller: _inputControllers[field.name],
              enabled: !_busy && !regenerating,
              obscureText: _isSecretField(field),
              decoration: InputDecoration(
                labelText: field.name,
                helperText: _manifestFieldHelp(field),
                border: const OutlineInputBorder(),
                suffixIcon: const Icon(PiccoloIcons.lockKey),
              ),
              onChanged: (_) => _markInputTouched(field.name),
            ),
            CheckboxListTile(
              value: regenerating,
              onChanged: _busy
                  ? null
                  : (value) {
                      setState(() {
                        _dryRun = null;
                        if (value ?? false) {
                          _regenerateInputs.add(field.name);
                          _inputControllers[field.name]?.clear();
                        } else {
                          _regenerateInputs.remove(field.name);
                        }
                      });
                    },
              title: Text(
                field.hasCurrentValue
                    ? 'Regenerate ${field.name}'
                    : 'Generate ${field.name}',
              ),
              controlAffinity: ListTileControlAffinity.leading,
              contentPadding: EdgeInsets.zero,
            ),
          ],
        ),
      );
    }
    if (field.type == 'boolean') {
      return CheckboxListTile(
        value: _inputControllers[field.name]?.text == 'true',
        onChanged: field.locked || _busy
            ? null
            : (value) => setState(
                () {
                  _dryRun = null;
                  _touchedInputs.add(field.name);
                  _inputControllers[field.name]?.text = (value ?? false)
                      ? 'true'
                      : 'false';
                },
              ),
        title: Text(field.name),
        subtitle: Text(_manifestFieldHelp(field)),
        controlAffinity: ListTileControlAffinity.leading,
      );
    }
    return Padding(
      padding: const EdgeInsets.only(bottom: Spacing.sm),
      child: TextField(
        controller: _inputControllers[field.name],
        enabled:
            !field.locked && !_busy && !_regenerateInputs.contains(field.name),
        obscureText: _isSecretField(field),
        decoration: InputDecoration(
          labelText: field.name,
          helperText: _manifestFieldHelp(field),
          border: const OutlineInputBorder(),
          suffixIcon: field.locked ? const Icon(PiccoloIcons.lock) : null,
        ),
        onChanged: (_) => _markInputTouched(field.name),
      ),
    );
  }

  String _secretPreflightText(ManifestUpdateConfigureResult configure) {
    final generate = <String>[];
    final reenter = <String>[];
    for (final name in configure.secretGeneratedPreflight) {
      ManifestUpdateInputField? field;
      for (final candidate in configure.fields) {
        if (candidate.name == name) {
          field = candidate;
          break;
        }
      }
      if (field?.generate ?? false) {
        generate.add(name);
      } else {
        reenter.add(name);
      }
    }
    final parts = <String>[];
    if (reenter.isNotEmpty) {
      parts.add('Re-enter required: ${reenter.join(', ')}');
    }
    if (generate.isNotEmpty) {
      parts.add('Generate or enter required: ${generate.join(', ')}');
    }
    return parts.join(' · ');
  }

  Widget _buildCurrentValueField(ManifestUpdateInputField field) {
    if (!_isSensitiveCurrentField(field) &&
        field.currentValueDisplay.trim().isNotEmpty) {
      return Padding(
        padding: const EdgeInsets.only(bottom: Spacing.sm),
        child: TextField(
          controller: _inputControllers[field.name],
          enabled: !_busy,
          decoration: InputDecoration(
            labelText: field.name,
            helperText: _manifestFieldHelp(field),
            border: const OutlineInputBorder(),
          ),
          onChanged: (_) => _markCurrentInputEdited(field.name),
        ),
      );
    }
    final action = _regenerateInputs.contains(field.name)
        ? 'regenerate'
        : _clearInputs.contains(field.name)
        ? 'clear'
        : _replaceInputs.contains(field.name)
        ? 'replace'
        : 'keep';
    final actions = <String>['keep', 'replace'];
    if (field.generate) actions.add('regenerate');
    if (_canClearCurrentField(field)) actions.add('clear');
    return Padding(
      padding: const EdgeInsets.only(bottom: Spacing.sm),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          DropdownButtonFormField<String>(
            initialValue: action,
            items: actions
                .map(
                  (value) => DropdownMenuItem<String>(
                    value: value,
                    child: Text(_currentValueActionLabel(value)),
                  ),
                )
                .toList(),
            onChanged: _busy
                ? null
                : (value) => _setCurrentValueAction(field, value ?? 'keep'),
            decoration: InputDecoration(
              labelText: field.name,
              helperText: _manifestFieldHelp(field),
              border: const OutlineInputBorder(),
              suffixIcon: Icon(
                field.currentValueSensitive || _isSecretField(field)
                    ? PiccoloIcons.lockKey
                    : PiccoloIcons.lock,
              ),
            ),
          ),
          if (action == 'replace') ...[
            const SizedBox(height: Spacing.xs),
            _replacementInput(field),
          ] else if (action == 'keep' &&
              field.currentValueDisplay.trim().isNotEmpty) ...[
            const SizedBox(height: Spacing.xs),
            Text(
              'Current value: ${field.currentValueDisplay.trim()}',
              style: PiccoloTheme.textTheme.bodySmall,
            ),
          ] else if (action == 'clear') ...[
            const SizedBox(height: Spacing.xs),
            Text(
              'This will remove the stored value for this optional field.',
              style: PiccoloTheme.textTheme.bodySmall?.copyWith(
                color: PiccoloTheme.warning,
              ),
            ),
          ],
        ],
      ),
    );
  }

  Widget _replacementInput(ManifestUpdateInputField field) {
    if (field.type == 'boolean') {
      return CheckboxListTile(
        value: _inputControllers[field.name]?.text == 'true',
        onChanged: _busy
            ? null
            : (value) => setState(() {
                _dryRun = null;
                _touchedInputs.add(field.name);
                _inputControllers[field.name]?.text = (value ?? false)
                    ? 'true'
                    : 'false';
              }),
        title: const Text('New value'),
        controlAffinity: ListTileControlAffinity.leading,
        contentPadding: EdgeInsets.zero,
      );
    }
    return TextField(
      controller: _inputControllers[field.name],
      enabled: !_busy,
      obscureText: _isSecretField(field) || field.currentValueSensitive,
      decoration: const InputDecoration(
        labelText: 'New value',
        border: OutlineInputBorder(),
      ),
      onChanged: (_) => _markInputTouched(field.name),
    );
  }

  String _currentValueActionLabel(String action) {
    switch (action) {
      case 'keep':
        return 'Keep current value';
      case 'replace':
        return 'Replace value';
      case 'regenerate':
        return 'Regenerate value';
      case 'clear':
        return 'Clear value';
      default:
        return action;
    }
  }

  bool _isSensitiveCurrentField(ManifestUpdateInputField field) {
    return field.currentValueSensitive ||
        _isSecretField(field) ||
        field.generate;
  }

  bool _isSecretField(ManifestUpdateInputField field) {
    return field.sensitive || field.type == 'password';
  }

  bool _canClearCurrentField(ManifestUpdateInputField field) {
    return field.hasCurrentValue &&
        !field.required &&
        _isSensitiveCurrentField(field);
  }

  String _manifestFieldHelp(ManifestUpdateInputField field) {
    if (field.locked) return field.provenance;
    if (field.hasCurrentValue) {
      if (field.currentValueSensitive || _isSecretField(field)) {
        return 'Stored secret will be kept and is not shown.';
      }
      if (field.currentValueDisplay.trim().isNotEmpty) {
        return 'Current stored value is shown below and will be kept unless replaced.';
      }
      return 'Stored value will be kept and is not shown.';
    }
    switch (field.provenance) {
      case 'Re-enter required':
        return 'Required once because this app predates stored config.';
      case 'Enter required':
        return 'Required for this manifest.';
      case 'New manifest default':
        return 'Default from the replacement manifest.';
      default:
        return field.provenance;
    }
  }

  void _markCurrentInputEdited(String name) {
    _replaceInputs.add(name);
    _touchedInputs.add(name);
    _regenerateInputs.remove(name);
    _clearInputs.remove(name);
    _invalidateDryRun();
  }

  void _handleSourceChanged(String value) {
    if (widget.catalogPending || _taskId != null) return;
    setState(_clearPreparedState);
  }

  void _clearPreparedState() {
    _error = null;
    _configure = null;
    _dryRun = null;
    _accessRepairMessage = null;
    _confirmedReviewItems.clear();
    _regenerateInputs.clear();
    _replaceInputs.clear();
    _clearInputs.clear();
    _touchedInputs.clear();
    for (final controller in _inputControllers.values) {
      controller.dispose();
    }
    _inputControllers.clear();
  }

  Widget _buildDryRunSummary(ManifestUpdateResult result) {
    final summary = result.summary;
    final rejectedItems = result.decisions
        .where((decision) => decision.outcome == 'rejected')
        .map(_decisionText)
        .toList();
    if (rejectedItems.isEmpty) {
      rejectedItems.addAll(summary.rejected);
    }
    final showRejectedFirst = !result.applicable && rejectedItems.isNotEmpty;
    final exposureConfirmations = _uniqueConfirmations(
      result.exposureReview.map((item) => item.confirmation),
    );
    final keptValueConfirmations = _uniqueConfirmations(
      result.keptValueReview.map((item) => item.confirmation),
    );
    final dataConfirmations =
        result.requiredConfirmations.contains('data_impact_review')
        ? const ['data_impact_review']
        : const <String>[];
    final imageConfirmations =
        result.requiredConfirmations.contains('image_update_review')
        ? const ['image_update_review']
        : const <String>[];
    final serviceConfirmations = result.requiredConfirmations
        .where(
          (confirmation) =>
              confirmation == 'service_shape_review' ||
              confirmation == 'service_removal_review',
        )
        .toList();
    final inlineConfirmations = {
      ...exposureConfirmations,
      ...keptValueConfirmations,
      ...dataConfirmations,
      ...imageConfirmations,
      ...serviceConfirmations,
    };
    final remainingConfirmations = result.requiredConfirmations
        .where((confirmation) => !inlineConfirmations.contains(confirmation))
        .toList();
    final technicalDetails = _technicalDetailGroups(result);
    final reviewDecisions = result.decisions
        .where((decision) => decision.outcome == 'operator_review')
        .map(_reviewDecisionText)
        .toList();
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        if (!result.applicable)
          _Banner(
            icon: PiccoloIcons.error,
            color: PiccoloTheme.critical,
            text: result.blockingReason,
          ),
        if (showRejectedFirst)
          _SummarySection(title: 'Fix before apply', items: rejectedItems),
        if (result.updateClass.isNotEmpty)
          _Banner(
            icon: PiccoloIcons.info,
            color: PiccoloTheme.inkMuted,
            text: _updateClassLabel(result.updateClass),
          ),
        if (reviewDecisions.isNotEmpty)
          _SummarySection(
            title: 'Why review is needed',
            items: reviewDecisions,
          ),
        if (result.applicable &&
            reviewDecisions.isEmpty &&
            result.requiredConfirmations.isEmpty)
          const _SummarySection(
            title: 'Ready to update',
            items: ['No extra operator review is required.'],
          ),
        if (result.exposureReview.isNotEmpty)
          _SummarySection(
            title: 'Exposure review',
            items: result.exposureReview.map(_reviewItemText).toList(),
            children: _confirmationTiles(exposureConfirmations, result),
          ),
        if (result.keptValueReview.isNotEmpty)
          _SummarySection(
            title: result.applicable
                ? 'Current values kept'
                : 'Current values needing action',
            items: result.keptValueReview.map(_keptValueReviewText).toList(),
            children: _confirmationTiles(keptValueConfirmations, result),
          ),
        if (result.dataSafety != null)
          _SummarySection(
            title: 'Data safety',
            items: _dataSafetyItems(result.dataSafety!),
            children: _confirmationTiles(dataConfirmations, result),
          ),
        if (result.stagedImageRootfs.isNotEmpty ||
            imageConfirmations.isNotEmpty)
          _SummarySection(
            title: 'Image and rootfs',
            items: result.stagedImageRootfs,
            children: _confirmationTiles(imageConfirmations, result),
          ),
        if (serviceConfirmations.isNotEmpty)
          _SummarySection(
            title: 'Service changes',
            items: _serviceReviewItems(result),
            children: _confirmationTiles(serviceConfirmations, result),
          ),
        _SummarySection(title: 'What will change', items: summary.willChange),
        _SummarySection(title: 'Will preserve', items: summary.willPreserve),
        _SummarySection(
          title: 'Expected interruption',
          items: summary.expectedInterruption,
        ),
        if (technicalDetails.isNotEmpty)
          _TechnicalDetailsSection(children: technicalDetails),
        if (summary.rejected.isNotEmpty && !showRejectedFirst)
          _SummarySection(title: 'Rejected', items: summary.rejected),
        if (remainingConfirmations.isNotEmpty)
          _ConfirmationSection(
            title: 'Additional review',
            requiredConfirmations: remainingConfirmations,
            reviewCounts: _confirmationReviewCounts(result),
            accepted: _confirmedReviewItems,
            enabled: !_busy && _taskId == null && result.applicable,
            onChanged: (confirmation, {required checked}) {
              setState(() {
                if (checked) {
                  _confirmedReviewItems.add(confirmation);
                } else {
                  _confirmedReviewItems.remove(confirmation);
                }
              });
            },
          ),
      ],
    );
  }

  List<String> _uniqueConfirmations(Iterable<String> confirmations) {
    final out = <String>[];
    for (final confirmation in confirmations) {
      final trimmed = confirmation.trim();
      if (trimmed.isEmpty || out.contains(trimmed)) continue;
      out.add(trimmed);
    }
    return out;
  }

  List<Widget> _confirmationTiles(
    List<String> confirmations,
    ManifestUpdateResult result,
  ) {
    return confirmations
        .map(
          (confirmation) => CheckboxListTile(
            value: _confirmedReviewItems.contains(confirmation),
            onChanged: !_busy && _taskId == null && result.applicable
                ? (value) {
                    setState(() {
                      if (value ?? false) {
                        _confirmedReviewItems.add(confirmation);
                      } else {
                        _confirmedReviewItems.remove(confirmation);
                      }
                    });
                  }
                : null,
            title: Text(_ConfirmationSection.confirmationLabel(confirmation)),
            controlAffinity: ListTileControlAffinity.leading,
            contentPadding: EdgeInsets.zero,
            dense: true,
          ),
        )
        .toList();
  }

  List<Widget> _technicalDetailGroups(ManifestUpdateResult result) {
    final summary = result.summary;
    final groups = <({String title, List<String> items})>[
      (title: 'Changed paths and keys', items: summary.willChange),
      (title: 'Restart behavior', items: summary.willRestart),
      (title: 'Listener routing and auth', items: result.listenerRoutingAuth),
      (title: 'Storage boundary', items: result.storageBoundary),
      (title: 'Runtime readiness', items: result.runtimeReadiness),
      (title: 'Review reason flags', items: result.operationRiskFlags),
      (
        title: 'Policy decisions',
        items: result.decisions.map(_decisionText).toList(),
      ),
    ];
    return groups
        .where((group) => group.items.isNotEmpty)
        .map(
          (group) => Padding(
            padding: const EdgeInsets.only(bottom: Spacing.sm),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Text(
                  group.title,
                  style: PiccoloTheme.textTheme.bodySmall?.copyWith(
                    fontWeight: FontWeight.w700,
                  ),
                ),
                const SizedBox(height: Spacing.xs),
                ...group.items.map((item) => Text('- $item')),
              ],
            ),
          ),
        )
        .toList();
  }

  List<String> _serviceReviewItems(ManifestUpdateResult result) {
    final items = result.decisions
        .where(
          (decision) =>
              decision.outcome == 'operator_review' &&
              (decision.flag.contains('service') ||
                  decision.flag.contains('startup')),
        )
        .map(_reviewDecisionText)
        .toList();
    if (items.isNotEmpty) return items;
    if (result.summary.willRestart.isNotEmpty) {
      return result.summary.willRestart;
    }
    return result.summary.willChange;
  }

  String _updateClassLabel(String updateClass) {
    switch (updateClass) {
      case 'service_app_update_v2':
        return 'App update';
      case 'manifest_update_v1':
        return 'Manifest modification';
      default:
        return updateClass;
    }
  }

  String get _applyLabel => widget.catalogPending ? 'Update' : 'Modify App';

  String _decisionText(ManifestUpdateDecision decision) {
    final path = decision.path.isEmpty ? '' : '${decision.path}: ';
    final reason = decision.reason.isEmpty ? '' : ' - ${decision.reason}';
    return '$path${decision.summary} (${_decisionOutcomeLabel(decision.outcome)})$reason';
  }

  String _reviewDecisionText(ManifestUpdateDecision decision) {
    final reason = decision.reason.isEmpty ? '' : ': ${decision.reason}';
    return '${decision.summary}$reason';
  }

  String _decisionOutcomeLabel(String outcome) {
    switch (outcome) {
      case 'operator_review':
        return 'review required';
      case 'supported':
        return 'supported';
      case 'rejected':
        return 'rejected';
      default:
        return outcome;
    }
  }

  String _reviewItemText(ManifestUpdateReviewItem item) {
    final oldValue = item.oldValue.isEmpty ? 'none' : item.oldValue;
    final newValue = item.newValue.isEmpty ? 'none' : item.newValue;
    return '${item.path}: $oldValue -> $newValue';
  }

  String _keptValueReviewText(ManifestUpdateKeptValueReviewItem item) {
    return manifestUpdateKeptValueReviewText(item);
  }

  List<String> _dataSafetyItems(ManifestUpdateDataSafetySummary dataSafety) {
    final items = <String>[];
    if (dataSafety.snapshotRequired) {
      items.add('Existing data will be snapshotted before candidate startup');
    } else {
      items.add(
        'Existing data does not need a rollback snapshot for this update',
      );
    }
    if (dataSafety.reason.isNotEmpty) items.add(dataSafety.reason);
    if (dataSafety.failureBehavior.isNotEmpty) {
      items.add(dataSafety.failureBehavior);
    }
    if (dataSafety.rollbackLimit.isNotEmpty) {
      items.add(dataSafety.rollbackLimit);
    }
    return items;
  }

  Map<String, int> _confirmationReviewCounts(ManifestUpdateResult result) {
    final counts = <String, int>{};
    for (final item in result.exposureReview) {
      final confirmation = item.confirmation.trim();
      if (confirmation.isEmpty) continue;
      counts[confirmation] = (counts[confirmation] ?? 0) + 1;
    }
    for (final item in result.keptValueReview) {
      final confirmation = item.confirmation.trim();
      if (confirmation.isEmpty) continue;
      counts[confirmation] = (counts[confirmation] ?? 0) + 1;
    }
    return counts;
  }
}

String manifestUpdateKeptValueReviewText(
  ManifestUpdateKeptValueReviewItem item,
) {
  final delta = item.semanticDelta.isEmpty
      ? 'meaning or usage changed'
      : item.semanticDelta.join(', ');
  final oldUsage = item.oldUsage.isEmpty
      ? 'previous usage unavailable'
      : item.oldUsage.join('; ');
  final newUsage = item.newUsage.isEmpty
      ? 'new usage unavailable'
      : item.newUsage.join('; ');
  if (item.blockingReason.isNotEmpty) {
    return '${item.field}: replace or regenerate before applying; ${item.blockingReason}; $delta; previous: $oldUsage; new: $newUsage';
  }
  return '${item.field}: current stored value will be kept after review; $delta; previous: $oldUsage; new: $newUsage';
}

class _ConfirmationSection extends StatelessWidget {
  const _ConfirmationSection({
    required this.requiredConfirmations,
    required this.reviewCounts,
    required this.accepted,
    required this.enabled,
    required this.onChanged,
    this.title = 'Required review',
  });

  final String title;
  final List<String> requiredConfirmations;
  final Map<String, int> reviewCounts;
  final Set<String> accepted;
  final bool enabled;
  final void Function(String confirmation, {required bool checked}) onChanged;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.only(bottom: Spacing.sm),
      child: DecoratedBox(
        decoration: BoxDecoration(
          border: Border.all(color: PiccoloTheme.hairline),
          borderRadius: BorderRadius.circular(Radii.sm),
          color: PiccoloTheme.porcelain,
        ),
        child: Padding(
          padding: const EdgeInsets.all(Spacing.sm),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              Padding(
                padding: const EdgeInsets.symmetric(horizontal: Spacing.sm),
                child: Text(
                  title,
                  style: const TextStyle(fontWeight: FontWeight.w700),
                ),
              ),
              const SizedBox(height: Spacing.xs),
              ...requiredConfirmations.map(
                (confirmation) => CheckboxListTile(
                  value: accepted.contains(confirmation),
                  onChanged: enabled
                      ? (value) =>
                            onChanged(confirmation, checked: value ?? false)
                      : null,
                  title: Text(_confirmationText(confirmation)),
                  controlAffinity: ListTileControlAffinity.leading,
                  contentPadding: EdgeInsets.zero,
                  dense: true,
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }

  String _confirmationText(String confirmation) {
    final label = confirmationLabel(confirmation);
    final count = reviewCounts[confirmation] ?? 0;
    if (count == 0) return label;
    return '$label ($count ${count == 1 ? 'item' : 'items'})';
  }

  static String confirmationLabel(String confirmation) {
    const exposurePrefix = 'exposure_review:';
    if (confirmation.startsWith(exposurePrefix)) {
      final target = confirmation
          .substring(exposurePrefix.length)
          .replaceAll('.', ' ')
          .replaceAll('_', ' ');
      return 'Exposure, routing, and auth reviewed: $target';
    }
    const keptValuePrefix = 'kept_value_review:';
    if (confirmation.startsWith(keptValuePrefix)) {
      final target = confirmation
          .substring(keptValuePrefix.length)
          .replaceAll('_', ' ');
      return 'Current value reuse reviewed: $target';
    }
    switch (confirmation) {
      case 'image_update_review':
        return 'Image/rootfs changes reviewed';
      case 'exposure_review':
        return 'Listener exposure, routing, and auth reviewed';
      case 'service_shape_review':
        return 'Service additions or startup-order changes reviewed';
      case 'service_removal_review':
        return 'Service removal reviewed';
      case 'data_impact_review':
        return 'Config/data impact reviewed';
      case 'kept_value_review':
        return 'Current value reuse reviewed';
      default:
        return confirmation;
    }
  }
}

class _LoadingCatalogUpdate extends StatelessWidget {
  const _LoadingCatalogUpdate();

  @override
  Widget build(BuildContext context) {
    return const Padding(
      padding: EdgeInsets.symmetric(vertical: Spacing.lg),
      child: Row(
        children: [
          SizedBox(
            width: 18,
            height: 18,
            child: CircularProgressIndicator(strokeWidth: 2),
          ),
          SizedBox(width: Spacing.sm),
          Expanded(child: Text('Loading update details')),
        ],
      ),
    );
  }
}

class _DryRunStatusStrip extends StatelessWidget {
  const _DryRunStatusStrip({
    required this.running,
    required this.result,
    required this.remainingConfirmations,
    required this.onViewDetails,
  });

  final bool running;
  final ManifestUpdateResult? result;
  final int remainingConfirmations;
  final VoidCallback? onViewDetails;

  @override
  Widget build(BuildContext context) {
    final result = this.result;
    final applicable = result?.applicable ?? false;
    final color = running
        ? PiccoloTheme.inkMuted
        : applicable
        ? PiccoloTheme.success
        : PiccoloTheme.critical;
    final icon = running
        ? PiccoloIcons.search
        : applicable
        ? PiccoloIcons.check
        : PiccoloIcons.error;
    final text = running ? 'Previewing changes...' : _summaryText(result);

    return Container(
      padding: const EdgeInsets.symmetric(
        horizontal: Spacing.base,
        vertical: Spacing.sm,
      ),
      decoration: BoxDecoration(
        color: color.withValues(alpha: 0.08),
        border: Border.all(color: color.withValues(alpha: 0.35)),
        borderRadius: BorderRadius.circular(Radii.sm),
      ),
      child: Row(
        children: [
          Icon(icon, color: color, size: 18),
          const SizedBox(width: Spacing.sm),
          Expanded(child: Text(text)),
          if (!running && onViewDetails != null)
            TextButton(
              onPressed: onViewDetails,
              child: const Text('View details'),
            ),
        ],
      ),
    );
  }

  String _summaryText(ManifestUpdateResult? result) {
    if (result == null) return '';
    if (!result.applicable) return 'Preview rejected';
    if (remainingConfirmations > 0) {
      return 'Preview ready: $remainingConfirmations required ${remainingConfirmations == 1 ? 'review' : 'reviews'} left';
    }
    final summary = result.summary;
    final changeCount =
        summary.willChange.length +
        summary.willRestart.length +
        summary.expectedInterruption.length;
    if (changeCount == 0) return 'Preview ready: no runtime changes';
    return 'Preview ready: $changeCount change${changeCount == 1 ? '' : 's'}';
  }
}

class _SummarySection extends StatelessWidget {
  const _SummarySection({
    required this.title,
    required this.items,
    this.children = const [],
  });

  final String title;
  final List<String> items;
  final List<Widget> children;

  @override
  Widget build(BuildContext context) {
    if (items.isEmpty && children.isEmpty) return const SizedBox.shrink();
    return Padding(
      padding: const EdgeInsets.only(bottom: Spacing.sm),
      child: DecoratedBox(
        decoration: BoxDecoration(
          border: Border.all(color: PiccoloTheme.hairline),
          borderRadius: BorderRadius.circular(Radii.sm),
          color: PiccoloTheme.porcelain,
        ),
        child: Padding(
          padding: const EdgeInsets.all(Spacing.base),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text(title, style: const TextStyle(fontWeight: FontWeight.w700)),
              const SizedBox(height: Spacing.xs),
              ...items.map((item) => Text('- $item')),
              if (children.isNotEmpty) ...[
                const SizedBox(height: Spacing.xs),
                ...children,
              ],
            ],
          ),
        ),
      ),
    );
  }
}

class _TechnicalDetailsSection extends StatelessWidget {
  const _TechnicalDetailsSection({required this.children});

  final List<Widget> children;

  @override
  Widget build(BuildContext context) {
    if (children.isEmpty) return const SizedBox.shrink();
    return Padding(
      padding: const EdgeInsets.only(bottom: Spacing.sm),
      child: DecoratedBox(
        decoration: BoxDecoration(
          border: Border.all(color: PiccoloTheme.hairline),
          borderRadius: BorderRadius.circular(Radii.sm),
          color: PiccoloTheme.porcelain,
        ),
        child: ExpansionTile(
          tilePadding: const EdgeInsets.symmetric(horizontal: Spacing.base),
          childrenPadding: const EdgeInsets.fromLTRB(
            Spacing.base,
            0,
            Spacing.base,
            Spacing.base,
          ),
          title: const Text(
            'Technical details',
            style: TextStyle(fontWeight: FontWeight.w700),
          ),
          children: children,
        ),
      ),
    );
  }
}

class _Banner extends StatelessWidget {
  const _Banner({required this.icon, required this.color, required this.text});

  final IconData icon;
  final Color color;
  final String text;

  @override
  Widget build(BuildContext context) {
    return Container(
      margin: const EdgeInsets.only(bottom: Spacing.base),
      padding: const EdgeInsets.all(Spacing.base),
      decoration: BoxDecoration(
        color: color.withValues(alpha: 0.08),
        border: Border.all(color: color.withValues(alpha: 0.35)),
        borderRadius: BorderRadius.circular(Radii.sm),
      ),
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Icon(icon, color: color, size: 18),
          const SizedBox(width: Spacing.sm),
          Expanded(child: Text(text)),
        ],
      ),
    );
  }
}

class _ManifestContextBanner extends StatelessWidget {
  const _ManifestContextBanner();

  @override
  Widget build(BuildContext context) {
    return const _Banner(
      icon: PiccoloIcons.fileText,
      color: PiccoloTheme.cobalt600,
      text: 'Update needs review. Source: app catalog.',
    );
  }
}
