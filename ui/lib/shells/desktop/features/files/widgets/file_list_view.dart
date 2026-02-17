import 'package:flutter/material.dart';
import '../../../../../theme/piccolo_icons.dart';
import '../../../../../theme/piccolo_theme.dart';
import '../file_system_entry.dart';

class FileListView extends StatelessWidget {
  final List<FileSystemEntry> entries;
  final Function(FileSystemEntry) onEntryTap;

  const FileListView({
    super.key,
    required this.entries,
    required this.onEntryTap,
  });

  @override
  Widget build(BuildContext context) {
    return LayoutBuilder(
      builder: (context, constraints) {
        // Responsive Grid: 120px wide items
        final int crossAxisCount = (constraints.maxWidth / 120).floor();

        return GridView.builder(
          padding: const EdgeInsets.all(Spacing.base),
          gridDelegate: SliverGridDelegateWithFixedCrossAxisCount(
            crossAxisCount: crossAxisCount > 0 ? crossAxisCount : 1,
            childAspectRatio: 0.85,
            crossAxisSpacing: Spacing.base,
            mainAxisSpacing: Spacing.base,
          ),
          itemCount: entries.length,
          itemBuilder: (context, index) {
            final entry = entries[index];
            return _FileItem(entry: entry, onTap: () => onEntryTap(entry));
          },
        );
      },
    );
  }
}

class _FileItem extends StatefulWidget {
  final FileSystemEntry entry;
  final VoidCallback onTap;

  const _FileItem({required this.entry, required this.onTap});

  @override
  State<_FileItem> createState() => _FileItemState();
}

class _FileItemState extends State<_FileItem> {
  bool _isHovered = false;

  @override
  Widget build(BuildContext context) {
    return MouseRegion(
      onEnter: (_) => setState(() => _isHovered = true),
      onExit: (_) => setState(() => _isHovered = false),
      child: GestureDetector(
        // Double tap to open/navigate
        onDoubleTap: widget.onTap,
        // Single tap just selects (visual feedback for now)
        onTap: () {},
        child: Container(
          decoration: BoxDecoration(
            color: _isHovered
                ? PiccoloTheme.cobalt600.withValues(alpha: 0.1)
                : Colors.transparent,
            borderRadius: BorderRadius.circular(Radii.sm),
          ),
          padding: const EdgeInsets.all(Spacing.md),
          child: Column(
            mainAxisAlignment: MainAxisAlignment.center,
            children: [
              Icon(
                widget.entry.isDirectory ? PiccoloIcons.folder : PiccoloIcons.file,
                size: 48,
                color: widget.entry.isDirectory
                    ? PiccoloTheme.cobalt500
                    : PiccoloTheme.inkMuted,
              ),
              const SizedBox(height: Spacing.md),
              Text(
                widget.entry.name,
                textAlign: TextAlign.center,
                maxLines: 2,
                overflow: TextOverflow.ellipsis,
                style: PiccoloTheme.textTheme.bodyMedium,
              ),
            ],
          ),
        ),
      ),
    );
  }
}
