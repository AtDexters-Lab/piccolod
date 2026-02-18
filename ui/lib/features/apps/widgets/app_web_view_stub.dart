import 'package:flutter/material.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'package:url_launcher/url_launcher.dart';

class AppWebView extends StatelessWidget {

  const AppWebView({required this.url, super.key});
  final String url;

  @override
  Widget build(BuildContext context) {
    return Center(
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          const Icon(PiccoloIcons.webAsset, size: 48, color: PiccoloTheme.inkMuted),
          const SizedBox(height: Spacing.base),
          const Text('In-app browser not supported on Desktop.'),
          const SizedBox(height: Spacing.sm),
          TextButton.icon(
            onPressed: () => launchUrl(Uri.parse(url)),
            icon: const Icon(PiccoloIcons.openExternal),
            label: const Text('Open in External Browser'),
          ),
        ],
      ),
    );
  }
}
