import 'package:flutter/material.dart';
import 'package:url_launcher/url_launcher.dart';

class AppWebView extends StatelessWidget {
  final String url;

  const AppWebView({super.key, required this.url});

  @override
  Widget build(BuildContext context) {
    return Center(
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          const Icon(Icons.web_asset_off, size: 48, color: Colors.grey),
          const SizedBox(height: 16),
          const Text("In-app browser not supported on Desktop."),
          const SizedBox(height: 8),
          TextButton.icon(
            onPressed: () => launchUrl(Uri.parse(url)),
            icon: const Icon(Icons.open_in_new),
            label: const Text("Open in External Browser"),
          ),
        ],
      ),
    );
  }
}
