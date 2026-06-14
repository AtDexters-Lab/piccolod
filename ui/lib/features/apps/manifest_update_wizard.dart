import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/app_models.dart';
import 'package:piccolo_os/core/models/task_progress.dart';
import 'package:piccolo_os/core/services/app_service.dart';
import 'package:piccolo_os/core/utils/task_id.dart';
import 'package:piccolo_os/features/apps/apply_settlement.dart';
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
  final void Function(String taskId, String taskType)? onTaskStarted;

  @override
  State<ManifestUpdateWizard> createState() => _ManifestUpdateWizardState();
}

bool manifestUpdateShouldSubmitField(
  ManifestUpdateInputField field, {
  required bool touched,
  required bool hasDefault,
}) {
  if (touched) return true;
  if (!hasDefault) return true;
  return field.required;
}

class _ManifestUpdateWizardState extends State<ManifestUpdateWizard> {
  final TextEditingController _yamlController = TextEditingController();
  final ScrollController _scrollController = ScrollController();
  final GlobalKey _dryRunSummaryKey = GlobalKey();
  final GlobalKey _taskProgressKey = GlobalKey();
  final GlobalKey _errorKey = GlobalKey();
  final Map<String, TextEditingController> _inputControllers = {};
  final Set<String> _regenerateInputs = {};
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
      setState(() => _error = e.toString());
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
      setState(() => _error = e.toString());
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
    widget.onTaskStarted?.call(taskId, 'update_service_app');
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
      setState(() {
        _busy = false;
        _taskId = null;
        _error = e.toString();
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
      title: Text(
        widget.catalogPending ? 'Review Catalog Update' : 'Review App Update',
      ),
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
                if (!widget.catalogPending) ...[
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
                      labelText: 'Manifest YAML',
                      border: OutlineInputBorder(),
                      alignLabelWithHint: true,
                    ),
                    onChanged: (_) => _invalidateDryRun(),
                  ),
                  const SizedBox(height: Spacing.base),
                  Align(
                    alignment: Alignment.centerLeft,
                    child: FilledButton.icon(
                      onPressed: _busy || _taskId != null ? null : _prepare,
                      icon: const Icon(PiccoloIcons.fileText),
                      label: const Text('Prepare'),
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
          label: Text(_taskId == null ? 'Apply' : 'Applying'),
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
            text:
                'Re-enter or regenerate: ${configure.secretGeneratedPreflight.join(', ')}',
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
            label: Text(_dryRunning ? 'Running Dry Run' : 'Dry Run'),
          ),
        ),
        if (_dryRunning || _dryRun != null) ...[
          const SizedBox(height: Spacing.sm),
          _DryRunStatusStrip(
            running: _dryRunning,
            result: _dryRun,
            onViewDetails: _dryRun == null ? null : _revealDryRunSummary,
          ),
        ],
      ],
    );
  }

  Widget _buildField(ManifestUpdateInputField field) {
    if (field.generate && !field.locked) {
      final regenerating = _regenerateInputs.contains(field.name);
      return Padding(
        padding: const EdgeInsets.only(bottom: Spacing.sm),
        child: Column(
          children: [
            TextField(
              controller: _inputControllers[field.name],
              enabled: !_busy && !regenerating,
              obscureText: field.type == 'password',
              decoration: InputDecoration(
                labelText: field.name,
                helperText: field.provenance,
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
              title: Text('Regenerate ${field.name}'),
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
        subtitle: Text(field.provenance),
        controlAffinity: ListTileControlAffinity.leading,
      );
    }
    return Padding(
      padding: const EdgeInsets.only(bottom: Spacing.sm),
      child: TextField(
        controller: _inputControllers[field.name],
        enabled:
            !field.locked && !_busy && !_regenerateInputs.contains(field.name),
        obscureText: field.type == 'password',
        decoration: InputDecoration(
          labelText: field.name,
          helperText: field.provenance,
          border: const OutlineInputBorder(),
          suffixIcon: field.locked ? const Icon(PiccoloIcons.lock) : null,
        ),
        onChanged: (_) => _markInputTouched(field.name),
      ),
    );
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
        if (result.decisions.isNotEmpty)
          _SummarySection(
            title: 'Decisions',
            items: result.decisions.map(_decisionText).toList(),
          ),
        if (result.exposureReview.isNotEmpty)
          _SummarySection(
            title: 'Exposure review',
            items: result.exposureReview.map(_reviewItemText).toList(),
          ),
        _SummarySection(title: 'Will change', items: summary.willChange),
        _SummarySection(title: 'Will restart', items: summary.willRestart),
        _SummarySection(
          title: 'Image and rootfs',
          items: result.stagedImageRootfs,
        ),
        _SummarySection(
          title: 'Listener routing and auth',
          items: result.listenerRoutingAuth,
        ),
        _SummarySection(
          title: 'Storage boundary',
          items: result.storageBoundary,
        ),
        _SummarySection(
          title: 'Runtime readiness',
          items: result.runtimeReadiness,
        ),
        _SummarySection(title: 'Risk flags', items: result.operationRiskFlags),
        if (result.dataSafety != null)
          _SummarySection(
            title: 'Data safety',
            items: _dataSafetyItems(result.dataSafety!),
          ),
        _SummarySection(title: 'Will preserve', items: summary.willPreserve),
        _SummarySection(
          title: 'Expected interruption',
          items: summary.expectedInterruption,
        ),
        if (summary.rejected.isNotEmpty && !showRejectedFirst)
          _SummarySection(title: 'Rejected', items: summary.rejected),
        if (result.requiredConfirmations.isNotEmpty)
          _ConfirmationSection(
            requiredConfirmations: result.requiredConfirmations,
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

  String _updateClassLabel(String updateClass) {
    switch (updateClass) {
      case 'service_app_update_v2':
        return 'Service app update';
      case 'manifest_update_v1':
        return 'Manifest update';
      default:
        return updateClass;
    }
  }

  String _decisionText(ManifestUpdateDecision decision) {
    final path = decision.path.isEmpty ? '' : '${decision.path}: ';
    final reason = decision.reason.isEmpty ? '' : ' - ${decision.reason}';
    return '$path${decision.summary} (${_decisionOutcomeLabel(decision.outcome)})$reason';
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
    final confirmation = item.confirmation.isEmpty
        ? ''
        : ' (requires ${_ConfirmationSection.confirmationLabel(item.confirmation)})';
    return '${item.path}: $oldValue -> $newValue$confirmation';
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
    return counts;
  }
}

class _ConfirmationSection extends StatelessWidget {
  const _ConfirmationSection({
    required this.requiredConfirmations,
    required this.reviewCounts,
    required this.accepted,
    required this.enabled,
    required this.onChanged,
  });

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
              const Padding(
                padding: EdgeInsets.symmetric(horizontal: Spacing.sm),
                child: Text(
                  'Required review',
                  style: TextStyle(fontWeight: FontWeight.w700),
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
          Expanded(child: Text('Loading pending catalog update')),
        ],
      ),
    );
  }
}

class _DryRunStatusStrip extends StatelessWidget {
  const _DryRunStatusStrip({
    required this.running,
    required this.result,
    required this.onViewDetails,
  });

  final bool running;
  final ManifestUpdateResult? result;
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
    final text = running ? 'Running dry run...' : _summaryText(result);

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
    if (!result.applicable) return 'Dry run rejected';
    final summary = result.summary;
    final changeCount =
        summary.willChange.length +
        summary.willRestart.length +
        summary.expectedInterruption.length;
    if (changeCount == 0) return 'Dry run complete: no runtime changes';
    return 'Dry run complete: $changeCount change${changeCount == 1 ? '' : 's'}';
  }
}

class _SummarySection extends StatelessWidget {
  const _SummarySection({required this.title, required this.items});

  final String title;
  final List<String> items;

  @override
  Widget build(BuildContext context) {
    if (items.isEmpty) return const SizedBox.shrink();
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
            ],
          ),
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
