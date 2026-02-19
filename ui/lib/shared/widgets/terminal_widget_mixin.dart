import 'dart:async';

import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:piccolo_os/core/config/core_config.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/services/error_reporter.dart';
import 'package:piccolo_os/core/utils/clipboard/clipboard.dart' as clipboard_utils;
import 'package:piccolo_os/shared/widgets/render_error_boundary.dart';
import 'package:piccolo_os/shells/desktop/features/terminal/terminal_backend.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'package:xterm/xterm.dart';

/// Mixin providing common terminal functionality for host and workspace terminals.
///
/// Supports persistent terminal sessions: a server-side PTY is created via REST,
/// then a WebSocket attaches to it. If the connection drops, reconnecting
/// reattaches to the same living PTY with scrollback replay.
mixin TerminalWidgetMixin<T extends StatefulWidget> on State<T> {
  late final Terminal terminal;
  late final TerminalController terminalController;
  late final ScrollController terminalScrollController;
  PiccoloTerminalBackend? terminalBackend;

  /// Tracks the server-side persistent session ID.
  /// Null means no session exists yet (will be created on connect).
  String? _terminalSessionId;

  static const _clipboardHint =
      '\r\n\x1b[33mClipboard unavailable. Browsers block copy/paste on insecure origins.\x1b[0m\r\n';

  /// Returns the REST path for creating a new persistent session.
  /// Example: '/api/v1/terminal/sessions'
  String getSessionCreatePath();

  /// Returns the WebSocket path for attaching to an existing session.
  /// Example: '/api/v1/terminal/sessions/$sessionId/attach'
  String getSessionAttachPath(String sessionId);

  /// Optional callback when terminal session ends normally (e.g., Ctrl+D).
  /// Subclasses can override to close window/navigate away.
  void Function()? getOnSessionEnd() => null;

  /// Initialize terminal components. Call in initState().
  void initTerminal() {
    terminal = Terminal(maxLines: 10000);
    terminalController = TerminalController();
    terminalScrollController = ScrollController();
  }

  /// Connect to the WebSocket terminal. Creates a persistent session if needed.
  Future<void> connectTerminal() async {
    // Create a new server-side session if we don't have one
    if (_terminalSessionId == null) {
      try {
        final result = await ApiClient().post(getSessionCreatePath());
        // Guard against widget disposal during async gap
        if (!mounted) return;
        if (result is Map && result.containsKey('id')) {
          _terminalSessionId = result['id'] as String;
        } else {
          terminal.write('\r\n\x1b[31mFailed to create terminal session\x1b[0m\r\n');
          return;
        }
      } on Object catch (e) {
        if (!mounted) return;
        terminal.write('\r\n\x1b[31mFailed to create terminal session: $e\x1b[0m\r\n');
        return;
      }
    }

    if (!mounted) return;

    final url = _buildWebSocketUrl(getSessionAttachPath(_terminalSessionId!));
    terminalBackend = PiccoloTerminalBackend(
      terminal,
      url,
      onSessionEnd: () {
        // Shell exited — clear session ID so next connect creates a fresh one
        _terminalSessionId = null;
        getOnSessionEnd()?.call();
      },
      onSessionLost: () {
        // Session is dead (expired/reaped) — clear ID and reconnect with backoff
        _terminalSessionId = null;
        Future<void>.delayed(const Duration(seconds: 2), reconnectTerminal);
      },
    );
    terminalBackend!.init();
  }

  /// Clean up terminal resources. Call in dispose().
  void disposeTerminal() {
    terminalScrollController.dispose();
    terminalController.dispose();
    terminalBackend?.dispose();
  }

  /// Dispose the current backend and reconnect with a fresh WebSocket.
  void reconnectTerminal() {
    if (!mounted) return;
    terminalBackend?.dispose();
    terminalBackend = null;
    unawaited(connectTerminal());
  }

  String _buildWebSocketUrl(String path) {
    final devUrl = CoreConfig.wsBaseUrl;
    if (devUrl.isNotEmpty) {
      return '$devUrl$path';
    } else if (kIsWeb) {
      final uri = Uri.base;
      final scheme = uri.scheme == 'https' ? 'wss' : 'ws';
      return '$scheme://${uri.host}:${uri.port}$path';
    } else {
      return 'ws://127.0.0.1$path';
    }
  }

  /// Copy selected text to clipboard.
  /// Uses platform-specific clipboard utils that fall back to execCommand('copy')
  /// on non-secure origins (HTTP), enabling copy on http://piccolo.local.
  Future<void> copyTerminalSelection() async {
    final selection = terminalController.selection;
    if (selection == null) return;
    final text = terminal.buffer.getText(selection);
    try {
      await clipboard_utils.copyText(text);
    } on Object catch (_) {
      terminal.write(_clipboardHint);
    }
  }

  /// Paste from clipboard into terminal.
  Future<void> pasteToTerminal() async {
    try {
      final data = await Clipboard.getData(Clipboard.kTextPlain);
      final text = data?.text;
      if (text == null || text.isEmpty) return;
      terminal.paste(text);
    } on PlatformException {
      terminal.write(_clipboardHint);
    }
  }

  /// Handle Ctrl+C shortcut: copy if selection exists, otherwise let terminal handle (SIGINT).
  void _handleCopyShortcut() {
    if (terminalController.selection != null) {
      unawaited(copyTerminalSelection());
    } else {
      // No selection - send Ctrl+C to terminal (SIGINT)
      terminal.keyInput(TerminalKey.keyC, ctrl: true);
    }
  }

  /// Show context menu with copy/paste options.
  Future<void> showTerminalContextMenu(BuildContext ctx, Offset position) async {
    final hasSelection = terminalController.selection != null;
    final choice = await showMenu<String>(
      context: ctx,
      position: RelativeRect.fromLTRB(
        position.dx,
        position.dy,
        position.dx,
        position.dy,
      ),
      items: [
        PopupMenuItem<String>(
          value: 'copy',
          enabled: hasSelection,
          child: const Text('Copy'),
        ),
        const PopupMenuItem<String>(value: 'paste', child: Text('Paste')),
      ],
    );

    switch (choice) {
      case 'copy':
        await copyTerminalSelection();
      case 'paste':
        await pasteToTerminal();
      default:
        break;
    }
  }

  /// Build the terminal view widget with standard styling.
  /// Uses CallbackShortcuts to intercept Ctrl+C before it reaches xterm,
  /// copying selected text or sending SIGINT if no selection.
  /// Wrapped in [RenderErrorBoundary] to catch paint/layout errors and
  /// show a recoverable fallback instead of Flutter's red error screen.
  Widget buildTerminalView() {
    return RenderErrorBoundary(
      onError: (error) => ErrorReporter().report(
        type: 'render_error',
        message: 'Terminal render error: $error',
        stack: error is Error ? error.stackTrace?.toString() : null,
      ),
      onRetry: reconnectTerminal,
      fallbackBuilder: (error, retry) =>
          RenderErrorFallback(label: 'Terminal', retry: retry),
      child: RepaintBoundary(
        child: ColoredBox(
          color: PiccoloTheme.terminalBg,
          child: SizedBox.expand(
            child: Scrollbar(
              controller: terminalScrollController,
              thumbVisibility: true,
              child: CallbackShortcuts(
                bindings: {
                  // Ctrl+Shift+C - explicit copy (Linux terminal convention)
                  const SingleActivator(LogicalKeyboardKey.keyC, control: true, shift: true):
                      copyTerminalSelection,
                  // Ctrl+Shift+V - explicit paste (Linux terminal convention)
                  const SingleActivator(LogicalKeyboardKey.keyV, control: true, shift: true):
                      pasteToTerminal,
                  // Ctrl+C on Linux/Windows - intercept to handle copy vs SIGINT
                  const SingleActivator(LogicalKeyboardKey.keyC, control: true):
                      _handleCopyShortcut,
                  // Cmd+C on macOS
                  const SingleActivator(LogicalKeyboardKey.keyC, meta: true):
                      _handleCopyShortcut,
                  // Cmd+V on macOS
                  const SingleActivator(LogicalKeyboardKey.keyV, meta: true):
                      pasteToTerminal,
                },
                child: TerminalView(
                  terminal,
                  controller: terminalController,
                  scrollController: terminalScrollController,
                  textStyle: const TerminalStyle(
                    fontFamily: 'JetBrainsMono',
                  ),
                  padding: const EdgeInsets.all(Spacing.md),
                  autofocus: true,
                  onSecondaryTapDown: (details, cell) =>
                      showTerminalContextMenu(context, details.globalPosition),
                ),
              ),
            ),
          ),
        ),
      ),
    );
  }

}
