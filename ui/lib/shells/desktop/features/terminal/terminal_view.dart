import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:xterm/xterm.dart';
import '../../../../../core/config/core_config.dart';
import 'terminal_backend.dart';

class TerminalApp extends StatefulWidget {
  const TerminalApp({super.key});

  @override
  State<TerminalApp> createState() => _TerminalAppState();
}

class _TerminalAppState extends State<TerminalApp> {
  late final Terminal _terminal;
  late final TerminalController _controller;
  late final ScrollController _scrollController;
  PiccoloTerminalBackend? _backend;

  static const _clipboardHint =
      '\r\n\x1b[33mClipboard unavailable. Browsers block copy/paste on insecure origins.\x1b[0m\r\n';

  @override
  void initState() {
    super.initState();
    _terminal = Terminal(maxLines: 10000);
    _controller = TerminalController();
    _scrollController = ScrollController();

    // Defer backend init to post-frame so we can get context/URL if needed,
    // though here we construct it dynamically.
    WidgetsBinding.instance.addPostFrameCallback((_) {
      _connect();
    });
  }

  void _connect() {
    String url;

    // 1. Check if an explicit dev URL is set via environment
    final devUrl = CoreConfig.wsBaseUrl;
    if (devUrl.isNotEmpty) {
      url = '$devUrl/api/v1/terminal';
    } else if (kIsWeb) {
      // 2. Web Prod: Auto-detect host from browser
      final uri = Uri.base;
      final scheme = uri.scheme == 'https' ? 'wss' : 'ws';
      url = '$scheme://${uri.host}:${uri.port}/api/v1/terminal';
    } else {
      // 3. Native Default
      url = 'ws://127.0.0.1/api/v1/terminal';
    }

    _backend = PiccoloTerminalBackend(_terminal, url);
    _backend!.init();
  }

  @override
  void dispose() {
    _scrollController.dispose();
    _controller.dispose();
    _backend?.dispose();
    super.dispose();
  }

  Future<void> _copySelection() async {
    final selection = _controller.selection;
    if (selection == null) return;
    final text = _terminal.buffer.getText(selection);
    try {
      await Clipboard.setData(ClipboardData(text: text));
    } on PlatformException {
      _terminal.write(_clipboardHint);
    }
  }

  Future<void> _pasteFromClipboard() async {
    try {
      final data = await Clipboard.getData(Clipboard.kTextPlain);
      final text = data?.text;
      if (text == null || text.isEmpty) return;
      // xterm will wrap in bracketed-paste sequence when supported
      _terminal.paste(text);
    } on PlatformException {
      _terminal.write(_clipboardHint);
    }
  }

  Future<void> _showContextMenu(BuildContext context, Offset position) async {
    final hasSelection = _controller.selection != null;
    final choice = await showMenu<String>(
      context: context,
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
        await _copySelection();
        break;
      case 'paste':
        await _pasteFromClipboard();
        break;
      default:
        break;
    }
  }

  @override
  Widget build(BuildContext context) {
    return Container(
      color: const Color(0xFF1E1E1E), // Dark background for terminal
      child: SizedBox.expand(
        child: Scrollbar(
          controller: _scrollController,
          thumbVisibility: true,
          child: TerminalView(
            _terminal,
            controller: _controller,
            scrollController: _scrollController,
            // Use bundled JetBrains Mono for consistent monospace metrics.
            textStyle: const TerminalStyle(
              fontFamily: 'JetBrainsMono',
              height: 1.2,
            ),
            padding: const EdgeInsets.all(12.0),
            autofocus: true,
            onSecondaryTapDown: (details, cell) =>
                _showContextMenu(context, details.globalPosition),
          ),
        ),
      ),
    );
  }
}
