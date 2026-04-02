import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/utils/downloader/downloader.dart';
import 'package:piccolo_os/shared/widgets/ca_import_guide.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class SecurityStep extends StatefulWidget {
  const SecurityStep({required this.onNext, super.key});
  final VoidCallback onNext;

  @override
  State<SecurityStep> createState() => _SecurityStepState();
}

class _SecurityStepState extends State<SecurityStep> {
  bool _downloading = false;

  Future<void> _downloadCA() async {
    setState(() => _downloading = true);
    try {
      final response = await ApiClient().get('/api/v1/system/ca.crt');
      downloadTextFile(response as String, 'piccolo-ca.crt');
    } on Object catch (_) {
      if (mounted) {
        ScaffoldMessenger.of(context).showSnackBar(
          const SnackBar(
            content: Text('Download failed. You can download later from Settings > Security.'),
            duration: Duration(seconds: 3),
          ),
        );
      }
    } finally {
      if (mounted) setState(() => _downloading = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 0, 32, 32),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          Text('Secure your connection',
              style: PiccoloTheme.textTheme.bodyLarge
                  ?.copyWith(fontWeight: FontWeight.bold)),
          const SizedBox(height: 12),
          Container(
            padding: const EdgeInsets.all(12),
            decoration: BoxDecoration(
              color: PiccoloTheme.mist,
              borderRadius: BorderRadius.circular(Radii.sm),
              border: Border.all(
                color: PiccoloTheme.cobalt600.withValues(alpha: 0.35),
                width: 1.2,
              ),
            ),
            child: const Row(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                Icon(PiccoloIcons.lock, color: PiccoloTheme.inkMuted, size: 18),
                SizedBox(width: 8),
                Expanded(
                  child: Text(
                    'Your device supports HTTPS on the local network. '
                    'Download the CA certificate and import it into your browser '
                    'to access the portal securely without warnings.',
                    style: TextStyle(fontSize: 13, color: PiccoloTheme.inkMuted),
                  ),
                ),
              ],
            ),
          ),
          const SizedBox(height: Spacing.lg),
          OutlinedButton.icon(
            onPressed: _downloading ? null : _downloadCA,
            icon: const Icon(PiccoloIcons.download, size: 16),
            label: Text(_downloading ? 'Downloading...' : 'Download CA Certificate'),
            style: OutlinedButton.styleFrom(foregroundColor: PiccoloTheme.ink),
          ),
          const SizedBox(height: Spacing.lg),
          const CaImportGuide(),
          const SizedBox(height: Spacing.lg),
          FilledButton(
            onPressed: widget.onNext,
            child: const Text('Finish setup'),
          ),
        ],
      ),
    );
  }
}
