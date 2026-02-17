import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:xterm/xterm.dart';

import '../../core/config/core_config.dart';
import '../../core/services/error_reporter.dart';
import '../../core/utils/clipboard/clipboard.dart' as clipboard_utils;
import '../../shells/desktop/features/terminal/terminal_backend.dart';
import '../../theme/piccolo_theme.dart';
import 'render_error_boundary.dart';

/// Mixin providing common terminal functionality for host and workspace terminals.
///
/// Usage:
/// ```dart
/// class _MyTerminalState extends State<MyTerminal> with TerminalWidgetMixin {
///   @override
///   String getTerminalPath() => '/api/v1/terminal';
///
///   @override
///   void initState() {
///     super.initState();
///     initTerminal();
///     WidgetsBinding.instance.addPostFrameCallback((_) => connectTerminal());
///   }
///
///   @override
///   void dispose() {
///     disposeTerminal();
///     super.dispose();
///   }
///
///   @override
///   Widget build(BuildContext context) => buildTerminalView();
/// }
/// ```
mixin TerminalWidgetMixin<T extends StatefulWidget> on State<T> {
  late final Terminal terminal;
  late final TerminalController terminalController;
  late final ScrollController terminalScrollController;
  PiccoloTerminalBackend? terminalBackend;

  static const _clipboardHint =
      '\r\n\x1b[33mClipboard unavailable. Browsers block copy/paste on insecure origins.\x1b[0m\r\n';

  /// Subclasses must implement this to provide the WebSocket endpoint path.
  /// Example: '/api/v1/terminal' or '/api/v1/apps/myapp/terminal'
  String getTerminalPath();

  /// Optional callback when terminal session ends normally (e.g., Ctrl+D).
  /// Subclasses can override to close window/navigate away.
  void Function()? getOnSessionEnd() => null;

  /// Initialize terminal components. Call in initState().
  void initTerminal() {
    terminal = Terminal(maxLines: 10000);
    terminalController = TerminalController();
    terminalScrollController = ScrollController();
  }

  /// Connect to the WebSocket terminal. Call in postFrameCallback.
  void connectTerminal() {
    final url = _buildWebSocketUrl(getTerminalPath());
    terminalBackend = PiccoloTerminalBackend(
      terminal,
      url,
      onSessionEnd: getOnSessionEnd(),
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
    terminalBackend?.dispose();
    terminalBackend = null;
    connectTerminal();
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
    } catch (e) {
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
      copyTerminalSelection();
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
        break;
      case 'paste':
        await pasteToTerminal();
        break;
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
      maxRetries: 3,
      onError: (error) => ErrorReporter().report(
        type: 'render_error',
        message: 'Terminal render error: $error',
        stack: error is Error ? error.stackTrace?.toString() : null,
      ),
      onRetry: reconnectTerminal,
      fallbackBuilder: (error, retry) =>
          RenderErrorFallback(label: 'Terminal', retry: retry),
      child: RepaintBoundary(
        child: Container(
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
                    height: 1.2,
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
