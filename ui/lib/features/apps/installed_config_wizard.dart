import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/app_models.dart';
import 'package:piccolo_os/core/models/task_progress.dart';
import 'package:piccolo_os/core/services/app_service.dart';
import 'package:piccolo_os/core/utils/task_id.dart';
import 'package:piccolo_os/shared/widgets/task_progress_panel.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class InstalledConfigWizard extends StatefulWidget {
  const InstalledConfigWizard({
    required this.appId,
    required this.appService,
    required this.onApplied,
    super.key,
  });

  final String appId;
  final AppService appService;
  final Future<void> Function() onApplied;

  @override
  State<InstalledConfigWizard> createState() => _InstalledConfigWizardState();
}

Object installedConfigInputValueForField(
  InstalledConfigField field,
  String text,
) {
  switch (field.type) {
    case 'boolean':
      return text == 'true';
    case 'int':
      return int.tryParse(text) ?? text;
    case 'number':
      return double.tryParse(text) ?? text;
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

bool installedConfigShouldSubmitPlainField(
  InstalledConfigField field, {
  required bool changed,
}) {
  if (changed) return true;
  return field.type == 'boolean' && field.required && !field.present;
}

class _InstalledConfigWizardState extends State<InstalledConfigWizard> {
  final ScrollController _scrollController = ScrollController();
  final GlobalKey _dryRunSummaryKey = GlobalKey();
  final GlobalKey _taskProgressKey = GlobalKey();
  final GlobalKey _errorKey = GlobalKey();
  final Map<String, TextEditingController> _controllers = {};
  final Map<String, String> _initialText = {};
  final Map<String, String> _secretActions = {};

  InstalledConfigReadResult? _config;
  InstalledConfigUpdateResult? _dryRun;
  String? _error;
  String? _taskId;
  bool _loading = true;
  bool _busy = false;
  bool _dryRunning = false;
  bool _applied = false;

  @override
  void initState() {
    super.initState();
    unawaited(_loadConfig());
  }

  @override
  void dispose() {
    _scrollController.dispose();
    for (final controller in _controllers.values) {
      controller.dispose();
    }
    super.dispose();
  }

  Future<void> _loadConfig() async {
    setState(() {
      _loading = true;
      _error = null;
    });
    try {
      final result = await widget.appService.getInstalledConfig(widget.appId);
      for (final controller in _controllers.values) {
        controller.dispose();
      }
      _controllers.clear();
      _initialText.clear();
      _secretActions.clear();
      for (final field in result.fields) {
        final text = _displayText(field.display);
        _controllers[field.name] = TextEditingController(text: text);
        _initialText[field.name] = text;
        if (field.generate) {
          _secretActions[field.name] = field.present ? 'keep' : 'regenerate';
        } else if (field.sensitive) {
          _secretActions[field.name] = field.present || !field.required
              ? 'keep'
              : 'replace';
        }
      }
      if (!mounted) return;
      setState(() {
        _config = result;
        _dryRun = null;
      });
    } on Object catch (e) {
      if (!mounted) return;
      setState(() => _error = e.toString());
      _revealError();
    } finally {
      if (mounted) setState(() => _loading = false);
    }
  }

  String _displayText(Object? value) {
    if (value == null) return '';
    if (value is List) {
      return value.map((e) => e.toString()).join(', ');
    }
    return value.toString();
  }

  Object _valueForField(InstalledConfigField field) {
    final text = _controllers[field.name]?.text ?? '';
    return installedConfigInputValueForField(field, text);
  }

  bool _fieldChanged(InstalledConfigField field) {
    return (_controllers[field.name]?.text ?? '') !=
        (_initialText[field.name] ?? '');
  }

  void _invalidateDryRun() {
    if (_dryRun == null || _taskId != null) return;
    setState(() => _dryRun = null);
  }

  Future<void> _dryRunUpdate() async {
    final config = _config;
    if (config == null || !config.recoverable) return;

    final inputs = <String, dynamic>{};
    final secretActions = <String, String>{};
    final regenerateInputs = <String>[];

    for (final field in config.fields) {
      if (!field.editable) continue;
      final action = _secretActions[field.name] ?? 'keep';
      if (field.generate) {
        if (action == 'regenerate') regenerateInputs.add(field.name);
        continue;
      }
      if (field.sensitive) {
        secretActions[field.name] = action;
        if (action == 'replace') {
          inputs[field.name] = _valueForField(field);
        }
        continue;
      }
      if (installedConfigShouldSubmitPlainField(
        field,
        changed: _fieldChanged(field),
      )) {
        inputs[field.name] = _valueForField(field);
      }
    }

    setState(() {
      _busy = true;
      _dryRunning = true;
      _dryRun = null;
      _error = null;
    });
    try {
      final result = await widget.appService.dryRunInstalledConfigUpdate(
        widget.appId,
        inputs: inputs,
        secretActions: secretActions,
        regenerateInputs: regenerateInputs,
        base: config,
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
    _revealTaskProgress();
    try {
      await widget.appService.applyInstalledConfigUpdate(
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
      title: const Text('Edit Config'),
      content: SizedBox(
        width: 720,
        child: ConstrainedBox(
          constraints: const BoxConstraints(maxHeight: 720),
          child: SingleChildScrollView(
            controller: _scrollController,
            child: _buildContent(),
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

  Widget _buildContent() {
    if (_loading) return const Center(child: CircularProgressIndicator());
    final config = _config;
    if (_error != null && config == null) {
      return _Banner(
        icon: PiccoloIcons.error,
        color: PiccoloTheme.critical,
        text: _error!,
      );
    }
    if (config == null) return const SizedBox.shrink();

    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      mainAxisSize: MainAxisSize.min,
      children: [
        for (final warning in config.warnings)
          _Banner(
            icon: config.recoverable
                ? PiccoloIcons.warning
                : PiccoloIcons.error,
            color: config.recoverable
                ? PiccoloTheme.warning
                : PiccoloTheme.critical,
            text: warning,
          ),
        if (!config.recoverable)
          const SizedBox.shrink()
        else if (config.fields.isEmpty)
          const Text('No declared config values.')
        else
          ...config.fields.map(_buildField),
        if (config.recoverable) ...[
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
              taskType: 'update_config',
              onComplete: (evt) => unawaited(_completeApply(evt)),
            ),
          ),
        ],
        if (_error != null) ...[
          const SizedBox(height: Spacing.base),
          KeyedSubtree(
            key: _errorKey,
            child: _Banner(
              icon: PiccoloIcons.error,
              color: PiccoloTheme.critical,
              text: _error!,
            ),
          ),
        ],
      ],
    );
  }

  Widget _buildField(InstalledConfigField field) {
    final label = field.label.isNotEmpty ? field.label : field.name;
    if (field.generate) {
      return _buildActionField(field, label, const ['keep', 'regenerate']);
    }
    if (field.sensitive) {
      final actions = field.actions.isNotEmpty
          ? field.actions
          : const ['keep', 'replace'];
      return _buildActionField(field, label, actions);
    }
    if (field.type == 'boolean') {
      return CheckboxListTile(
        value: _controllers[field.name]?.text == 'true',
        onChanged: !field.editable || _busy
            ? null
            : (value) => setState(() {
                _controllers[field.name]?.text = (value ?? false).toString();
                _dryRun = null;
              }),
        title: Text(label),
        subtitle: Text(_fieldSubtitle(field)),
        controlAffinity: ListTileControlAffinity.leading,
      );
    }
    return Padding(
      padding: const EdgeInsets.only(bottom: Spacing.sm),
      child: TextField(
        controller: _controllers[field.name],
        enabled: field.editable && !_busy,
        decoration: InputDecoration(
          labelText: label,
          helperText: _fieldSubtitle(field),
          border: const OutlineInputBorder(),
          suffixIcon: field.editable ? null : const Icon(PiccoloIcons.lock),
        ),
        onChanged: (_) => _invalidateDryRun(),
      ),
    );
  }

  Widget _buildActionField(
    InstalledConfigField field,
    String label,
    List<String> actions,
  ) {
    final currentAction = _secretActions[field.name] ?? actions.first;
    final needsValue = currentAction == 'replace';
    return Padding(
      padding: const EdgeInsets.only(bottom: Spacing.sm),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          DropdownButtonFormField<String>(
            initialValue: actions.contains(currentAction)
                ? currentAction
                : actions.first,
            items: actions
                .map(
                  (action) => DropdownMenuItem<String>(
                    value: action,
                    child: Text(_actionLabel(action)),
                  ),
                )
                .toList(),
            onChanged: !field.editable || _busy
                ? null
                : (value) => setState(() {
                    _secretActions[field.name] = value ?? actions.first;
                    if (value != 'replace') {
                      _controllers[field.name]?.clear();
                    }
                    _dryRun = null;
                  }),
            decoration: InputDecoration(
              labelText: label,
              helperText: _fieldSubtitle(field),
              border: const OutlineInputBorder(),
              suffixIcon: const Icon(PiccoloIcons.lockKey),
            ),
          ),
          if (needsValue) ...[
            const SizedBox(height: Spacing.xs),
            TextField(
              controller: _controllers[field.name],
              enabled: !_busy,
              obscureText: true,
              decoration: const InputDecoration(
                labelText: 'New value',
                border: OutlineInputBorder(),
              ),
              onChanged: (_) => _invalidateDryRun(),
            ),
          ],
        ],
      ),
    );
  }

  String _fieldSubtitle(InstalledConfigField field) {
    final pieces = <String>[
      if (field.description.isNotEmpty) field.description,
      field.provenance,
      if (field.required) 'required',
    ];
    return pieces.join(' | ');
  }

  String _actionLabel(String action) {
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

  Widget _buildDryRunSummary(InstalledConfigUpdateResult result) {
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
        _ActionSection(items: result.actions),
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
  final InstalledConfigUpdateResult? result;
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

  String _summaryText(InstalledConfigUpdateResult? result) {
    if (result == null) return '';
    if (!result.applicable) return 'Dry run rejected';
    final summary = result.summary;
    final changeCount =
        result.actions.length +
        summary.willChange.length +
        summary.willRestart.length +
        summary.expectedInterruption.length;
    if (changeCount == 0) return 'Dry run complete: no runtime changes';
    return 'Dry run complete: $changeCount change${changeCount == 1 ? '' : 's'}';
  }
}

class _ActionSection extends StatelessWidget {
  const _ActionSection({required this.items});

  final List<InstalledConfigActionSummary> items;

  @override
  Widget build(BuildContext context) {
    if (items.isEmpty) return const SizedBox.shrink();
    return _SummarySection(
      title: 'Config actions',
      items: items.map((item) {
        final suffix = item.consequence.isEmpty ? '' : ' (${item.consequence})';
        return '${item.field}: ${item.action}$suffix';
      }).toList(),
    );
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
