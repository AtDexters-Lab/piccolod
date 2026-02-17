import 'package:flutter/material.dart';
import '../../../../theme/piccolo_icons.dart';
import '../../../../theme/piccolo_theme.dart';
import 'files_controller.dart';
import 'widgets/path_bar.dart';
import 'widgets/file_list_view.dart';

class FilesApp extends StatefulWidget {
  const FilesApp({super.key});

  @override
  State<FilesApp> createState() => _FilesAppState();
}

class _FilesAppState extends State<FilesApp> {
  final FilesController _controller = FilesController();

  @override
  void dispose() {
    _controller.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return ListenableBuilder(
      listenable: _controller,
      builder: (context, child) {
        return Scaffold(
          backgroundColor: PiccoloTheme.mist,
          appBar: PreferredSize(
            preferredSize: const Size.fromHeight(56),
            child: PathBar(controller: _controller),
          ),
          body: _buildBody(),
        );
      },
    );
  }

  Widget _buildBody() {
    if (_controller.isLoading) {
      return const Center(child: CircularProgressIndicator());
    }

    if (_controller.error != null) {
      return Center(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            const Icon(PiccoloIcons.error, color: PiccoloTheme.critical, size: 48),
            const SizedBox(height: Spacing.base),
            Text("Error", style: PiccoloTheme.textTheme.bodyLarge),
            const SizedBox(height: Spacing.sm),
            Text(_controller.error!, style: PiccoloTheme.textTheme.labelSmall),
          ],
        ),
      );
    }

    if (_controller.entries.isEmpty) {
      return Center(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            Icon(PiccoloIcons.folderOpen, color: PiccoloTheme.ink.withValues(alpha: 0.2), size: 64),
            const SizedBox(height: Spacing.base),
            Text("Folder is empty", style: PiccoloTheme.textTheme.bodyMedium?.copyWith(color: PiccoloTheme.inkMuted)),
          ],
        ),
      );
    }

    return FileListView(
      entries: _controller.entries,
      onEntryTap: (entry) {
        if (entry.isDirectory) {
          _controller.navigateTo(entry.path);
        }
      },
    );
  }
}
