import 'package:flutter/material.dart';
import '../../../../../theme/piccolo_theme.dart';
import '../files_controller.dart';

class PathBar extends StatelessWidget {
  final FilesController controller;

  const PathBar({super.key, required this.controller});

  @override
  Widget build(BuildContext context) {
    return Container(
      height: 56,
      padding: const EdgeInsets.symmetric(horizontal: 16),
      decoration: BoxDecoration(
        color: Colors.white,
        border: Border(bottom: BorderSide(color: PiccoloTheme.mist)),
      ),
      child: Row(
        children: [
          // Navigation Controls
          IconButton(
            icon: const Icon(Icons.arrow_back),
            onPressed: controller.canGoBack ? controller.goBack : null,
            splashRadius: 20,
          ),
          IconButton(
            icon: const Icon(Icons.arrow_forward),
            onPressed: controller.canGoForward ? controller.goForward : null,
            splashRadius: 20,
          ),
          IconButton(
            icon: const Icon(Icons.arrow_upward),
            onPressed: controller.currentPath != '/' ? controller.goUp : null,
            splashRadius: 20,
          ),
          const SizedBox(width: 16),
          
          // Address Bar
          Expanded(
            child: Container(
              height: 36,
              padding: const EdgeInsets.symmetric(horizontal: 12),
              decoration: BoxDecoration(
                color: PiccoloTheme.mist,
                borderRadius: BorderRadius.circular(8),
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
