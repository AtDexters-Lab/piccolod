import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class RemoteAddressStep extends StatefulWidget {
  const RemoteAddressStep({
    required this.enrolled,
    required this.baseDomain,
    required this.onSubmit,
    required this.onSkip,
    required this.onRefresh,
    this.suggestedHostname,
    this.error,
    this.isSubmitting = false,
    super.key,
  });
  final bool enrolled;
  final String baseDomain;
  final Future<bool> Function(String) onSubmit;
  final VoidCallback onSkip;
  final Future<void> Function() onRefresh;
  final String? suggestedHostname;
  final String? error;
  final bool isSubmitting;

  @override
  State<RemoteAddressStep> createState() => _RemoteAddressStepState();
}

class _RemoteAddressStepState extends State<RemoteAddressStep> {
  late final TextEditingController _hostnameController;
  final _focusNode = FocusNode();
  String? _localError;
  bool _isFocused = false;
  Timer? _pollTimer;
  bool _pollInFlight = false;
  bool _suggestionApplied = false;

  @override
  void initState() {
    super.initState();
    final suggestion = widget.suggestedHostname ?? '';
    _hostnameController = TextEditingController(text: suggestion);
    _suggestionApplied = suggestion.isNotEmpty;
    _focusNode.addListener(_onFocusChange);
    unawaited(widget.onRefresh());
    if (!widget.enrolled) {
      // Poll for enrollment completion while not yet enrolled.
      // Auto-enrollment may finish seconds after this step renders.
      _startPoll(const Duration(seconds: 3));
    } else if (_needsSuggestionPoll) {
      // Enrolled but suggestion hasn't arrived yet (fetchSuggestedHostname
      // still in flight). Poll briefly to catch it.
      _startSuggestionPoll();
    }
  }

  bool get _needsSuggestionPoll =>
      widget.suggestedHostname == null || widget.suggestedHostname!.isEmpty;

  void _startPoll(Duration interval) {
    _pollTimer?.cancel();
    _pollTimer = Timer.periodic(
      interval,
      (_) async {
        if (_pollInFlight) return;
        _pollInFlight = true;
        try {
          await widget.onRefresh();
        } finally {
          _pollInFlight = false;
        }
      },
    );
  }

  /// Short-lived poll to catch a late-arriving suggested hostname after
  /// enrollment. Auto-cancels after 20 seconds (the backend fetch has a
  /// 15-second timeout; 20s provides margin for slow responses).
  void _startSuggestionPoll() {
    _startPoll(const Duration(seconds: 2));
    Future<void>.delayed(const Duration(seconds: 20), () {
      if (!mounted) return;
      _pollTimer?.cancel();
      _pollTimer = null;
    });
  }

  @override
  void didUpdateWidget(RemoteAddressStep oldWidget) {
    super.didUpdateWidget(oldWidget);
    // Pre-populate if suggested hostname arrives after initial build
    // (e.g., from refreshEnrollmentStatus polling) and the user hasn't typed yet.
    if (widget.suggestedHostname != oldWidget.suggestedHostname &&
        widget.suggestedHostname != null &&
        widget.suggestedHostname!.isNotEmpty &&
        _hostnameController.text.isEmpty) {
      _hostnameController.text = widget.suggestedHostname!;
      // Setting .text resets cursor to position 0; move it to the end.
      _hostnameController.selection = TextSelection.collapsed(
        offset: _hostnameController.text.length,
      );
      _suggestionApplied = true;
    }
    // Enrollment just detected — switch from enrollment poll to suggestion poll.
    if (widget.enrolled && !oldWidget.enrolled) {
      _pollTimer?.cancel();
      _pollTimer = null;
      if (_needsSuggestionPoll) {
        _startSuggestionPoll();
      }
    }
    // Suggestion arrived — stop any polling.
    if (widget.suggestedHostname != null &&
        widget.suggestedHostname!.isNotEmpty &&
        _pollTimer != null) {
      _pollTimer?.cancel();
      _pollTimer = null;
    }
    // Suggested hostname was rejected (taken/unavailable) — clear the field
    // only if the user hadn't edited it (preserve user's modified input).
    if (oldWidget.suggestedHostname != null &&
        widget.suggestedHostname == null &&
        widget.error != null &&
        _hostnameController.text == oldWidget.suggestedHostname) {
      _hostnameController.clear();
      _suggestionApplied = false;
    }
  }

  void _onFocusChange() {
    setState(() => _isFocused = _focusNode.hasFocus);
  }

  @override
  void dispose() {
    _pollTimer?.cancel();
    _focusNode.removeListener(_onFocusChange);
    _hostnameController.dispose();
    _focusNode.dispose();
    super.dispose();
  }

  bool _isValidDNSLabel(String label) {
    if (label.isEmpty || label.length > 63) return false;
    return RegExp(r'^[a-z0-9]([a-z0-9-]*[a-z0-9])?$').hasMatch(label);
  }

  Future<void> _submit() async {
    final hostname = _hostnameController.text.trim().toLowerCase();
    if (hostname.isEmpty) {
      setState(() => _localError = 'Please enter an address');
      return;
    }
    if (!_isValidDNSLabel(hostname)) {
      setState(() => _localError = 'Use lowercase letters, numbers, and hyphens only');
      return;
    }
    setState(() => _localError = null);
    await widget.onSubmit(hostname);
  }

  @override
  Widget build(BuildContext context) {
    final displayError = widget.error ?? _localError;

    if (!widget.enrolled) {
      return Padding(
        padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            const Icon(
              PiccoloIcons.router,
              size: 32,
              color: PiccoloTheme.inkMuted,
            ),
            const SizedBox(height: 16),
            Text(
              'Remote access is not available yet.\nYou can set it up later in Settings.',
              style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
                color: PiccoloTheme.inkMuted,
              ),
              textAlign: TextAlign.center,
            ),
            const SizedBox(height: 32),
            FilledButton(
              onPressed: widget.onSkip,
              child: const Text('Continue'),
            ),
          ],
        ),
      );
    }

    final isFocused = _isFocused;
    final hasError = displayError != null;

    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          Text(
            'Choose a permanent address for your Piccolo.',
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
              color: PiccoloTheme.inkMuted,
            ),
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 24),
          GestureDetector(
            onTap: _focusNode.requestFocus,
            child: Container(
              decoration: BoxDecoration(
                color: PiccoloTheme.porcelain,
                borderRadius: BorderRadius.circular(Radii.sm),
                border: Border.all(
                  color: hasError
                      ? PiccoloTheme.critical
                      : isFocused
                          ? PiccoloTheme.cobalt600
                          : PiccoloTheme.outline,
                  width: isFocused ? 2 : 1,
                ),
              ),
              child: Row(
                children: [
                  Expanded(
                    child: TextField(
                      controller: _hostnameController,
                      focusNode: _focusNode,
                      decoration: const InputDecoration(
                        hintText: 'yourname',
                        filled: false,
                        border: InputBorder.none,
                        enabledBorder: InputBorder.none,
                        focusedBorder: InputBorder.none,
                        errorBorder: InputBorder.none,
                        focusedErrorBorder: InputBorder.none,
                        contentPadding:
                            EdgeInsets.symmetric(horizontal: 12, vertical: 14),
                        isDense: true,
                      ),
                      enabled: !widget.isSubmitting,
                      autofocus: true,
                      textInputAction: TextInputAction.go,
                      onSubmitted: (_) => _submit(),
                    ),
                  ),
                  Container(
                    width: 1,
                    height: 24,
                    color: PiccoloTheme.hairline,
                  ),
                  Padding(
                    padding: const EdgeInsets.symmetric(horizontal: 12),
                    child: Text(
                      '.${widget.baseDomain}',
                      style: PiccoloTheme.textTheme.bodyMedium?.copyWith(
                        color: PiccoloTheme.inkMuted,
                      ),
                    ),
                  ),
                ],
              ),
            ),
          ),
          if (displayError != null)
            Padding(
              padding: const EdgeInsets.only(top: 6, left: 12),
              child: Align(
                alignment: Alignment.centerLeft,
                child: Text(
                  displayError,
                  style: PiccoloTheme.textTheme.bodySmall?.copyWith(
                    color: PiccoloTheme.critical,
                  ),
                ),
              ),
            ),
          if (_suggestionApplied && displayError == null)
            Padding(
              padding: const EdgeInsets.only(top: 6, left: 12),
              child: Align(
                alignment: Alignment.centerLeft,
                child: Text(
                  'Your previous address has been pre-filled. You can change it.',
                  style: PiccoloTheme.textTheme.bodySmall?.copyWith(
                    color: PiccoloTheme.inkMuted,
                  ),
                ),
              ),
            ),
          const SizedBox(height: 32),
          if (widget.isSubmitting)
            const Padding(
              padding: EdgeInsets.symmetric(vertical: 8),
              child: CircularProgressIndicator(color: PiccoloTheme.cobalt600),
            )
          else ...[
            FilledButton(
              onPressed: _submit,
              child: const Text('Claim address'),
            ),
            const SizedBox(height: 8),
            TextButton(
              onPressed: widget.onSkip,
              child: const Text('Skip for now'),
            ),
          ],
        ],
      ),
    );
  }
}
