import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/app_models.dart';
import 'package:piccolo_os/core/models/task_progress.dart';
import 'package:piccolo_os/core/services/app_service.dart';
import 'package:piccolo_os/core/utils/task_id.dart';
import 'package:piccolo_os/shared/widgets/task_progress_panel.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class ManifestUpdateWizard extends StatefulWidget {
  const ManifestUpdateWizard({
    required this.appId,
    required this.appService,
    required this.onApplied,
    super.key,
    this.onTaskStarted,
  });

  final String appId;
  final AppService appService;
  final Future<void> Function() onApplied;
  final void Function(String taskId, String taskType)? onTaskStarted;

  @override
  State<ManifestUpdateWizard> createState() => _ManifestUpdateWizardState();
}

class _ManifestUpdateWizardState extends State<ManifestUpdateWizard> {
  final TextEditingController _yamlController = TextEditingController();
  final ScrollController _scrollController = ScrollController();
  final GlobalKey _dryRunSummaryKey = GlobalKey();
  final GlobalKey _taskProgressKey = GlobalKey();
  final GlobalKey _errorKey = GlobalKey();
  final Map<String, TextEditingController> _inputControllers = {};
  final Set<String> _regenerateInputs = {};

  ManifestUpdateConfigureResult? _configure;
  ManifestUpdateResult? _dryRun;
  String? _error;
  bool _busy = false;
  bool _dryRunning = false;
  String? _taskId;
  bool _applied = false;

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
    if (yaml.isEmpty) {
      setState(() => _error = 'Manifest YAML is required.');
      _revealError();
      return;
    }
    setState(() {
      _busy = true;
      _error = null;
      _dryRun = null;
      _configure = null;
      _regenerateInputs.clear();
      for (final controller in _inputControllers.values) {
        controller.dispose();
      }
      _inputControllers.clear();
    });
    try {
      final result = await widget.appService.configureManifestUpdate(
        widget.appId,
        yaml,
      );
      for (final field in result.fields) {
        final raw = result.inputs[field.name];
        final schema = raw is Map<dynamic, dynamic>
            ? Map<String, dynamic>.from(raw)
            : <String, dynamic>{};
        final defaultValue = schema['default'];
        final text = field.locked && defaultValue != null
            ? defaultValue.toString()
            : '';
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
      final value = _valueForField(field);
      inputs[field.name] = value;
    }
    setState(() {
      _busy = true;
      _dryRunning = true;
      _error = null;
      _dryRun = null;
    });
    try {
      final result = await widget.appService.dryRunManifestUpdate(
        widget.appId,
        _yamlController.text,
        inputs,
        _regenerateInputs.toList(),
      );
      if (!mounted) return;
      setState(() => _dryRun = result);
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

  void _invalidateDryRun() {
    if (_dryRun == null || _taskId != null) return;
    setState(() => _dryRun = null);
  }

  Future<void> _apply() async {
    final dryRun = _dryRun;
    if (dryRun == null || !dryRun.applicable || dryRun.dryRunToken.isEmpty) {
      return;
    }
    final taskId = generateTaskId();
    setState(() {
      _busy = true;
      _error = null;
      _taskId = taskId;
    });
    widget.onTaskStarted?.call(taskId, 'update_manifest');
    _revealTaskProgress();
    try {
      await widget.appService.applyManifestUpdate(
        widget.appId,
        dryRun,
        taskId: taskId,
      );
    } on Object catch (e) {
      if (!mounted) return;
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
    if (event.error != null && event.error!.isNotEmpty) {
      if (!mounted) return;
      setState(() {
        _busy = false;
        _taskId = null;
        _error = event.error;
      });
      _revealError();
      return;
    }
    _applied = true;
    await widget.onApplied();
    if (mounted) Navigator.of(context).pop();
  }

  @override
  Widget build(BuildContext context) {
    return AlertDialog(
      title: const Text('Apply Manifest YAML'),
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
                if (_configure != null) ...[
                  const SizedBox(height: Spacing.lg),
                  _buildInputForm(_configure!),
                ],
                if (_dryRun != null) ...[
                  const SizedBox(height: Spacing.lg),
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
                      taskType: 'update_manifest',
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
        TextButton(
          onPressed: _busy ? null : () => Navigator.of(context).pop(),
          child: const Text('Cancel'),
        ),
        FilledButton.icon(
          onPressed:
              _busy ||
                  _dryRun == null ||
                  !_dryRun!.applicable ||
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
              onChanged: (_) => _invalidateDryRun(),
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
        onChanged: (_) => _invalidateDryRun(),
      ),
    );
  }

  Widget _buildDryRunSummary(ManifestUpdateResult result) {
    final summary = result.summary;
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        if (!result.applicable)
          _Banner(
            icon: PiccoloIcons.error,
            color: PiccoloTheme.critical,
            text: result.blockingReason,
          ),
        _SummarySection(title: 'Will change', items: summary.willChange),
        _SummarySection(title: 'Will restart', items: summary.willRestart),
        _SummarySection(title: 'Will preserve', items: summary.willPreserve),
        _SummarySection(
          title: 'Expected interruption',
          items: summary.expectedInterruption,
        ),
        if (summary.rejected.isNotEmpty)
          _SummarySection(title: 'Rejected', items: summary.rejected),
      ],
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
