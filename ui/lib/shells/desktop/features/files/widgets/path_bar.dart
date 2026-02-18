import 'package:flutter/material.dart';
import 'package:piccolo_os/shells/desktop/features/files/files_controller.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class PathBar extends StatelessWidget {

  const PathBar({required this.controller, super.key});
  final FilesController controller;

  @override
  Widget build(BuildContext context) {
    return Container(
      height: 56,
      padding: const EdgeInsets.symmetric(horizontal: Spacing.base),
      decoration: const BoxDecoration(
        color: PiccoloTheme.porcelain,
        border: Border(bottom: BorderSide(color: PiccoloTheme.hairline)),
      ),
      child: Row(
        children: [
          // Navigation Controls
          IconButton(
            icon: const Icon(PiccoloIcons.arrowBack),
            onPressed: controller.canGoBack ? controller.goBack : null,
            splashRadius: 20,
          ),
          IconButton(
            icon: const Icon(PiccoloIcons.arrowForward),
            onPressed: controller.canGoForward ? controller.goForward : null,
            splashRadius: 20,
          ),
          IconButton(
            icon: const Icon(PiccoloIcons.arrowUp),
            onPressed: controller.currentPath != '/' ? controller.goUp : null,
            splashRadius: 20,
          ),
          const SizedBox(width: Spacing.base),

          // Address Bar
          Expanded(
            child: Container(
              height: 36,
              padding: const EdgeInsets.symmetric(horizontal: Spacing.md),
              decoration: BoxDecoration(
                color: PiccoloTheme.mist,
                borderRadius: BorderRadius.circular(Radii.sm),
              ),
              alignment: Alignment.centerLeft,
              child: Text(
                controller.currentPath,
                style: PiccoloTheme.textTheme.bodyMedium,
              ),
            ),
          ),
        ],
      ),
    );
  }
}
