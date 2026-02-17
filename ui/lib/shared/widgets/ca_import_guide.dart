import 'package:flutter/material.dart';
import '../../theme/piccolo_theme.dart';

/// Compact per-browser instructions for importing the Piccolo CA certificate.
///
/// Used in the setup wizard security step and the Settings > Security tab.
class CaImportGuide extends StatelessWidget {
  const CaImportGuide({super.key});

  static const _browsers = [
    _BrowserEntry(
      name: 'Chrome',
      url: 'chrome://certificate-manager',
      steps: ' \u2192 Installed by you \u2192 Import',
    ),
    _BrowserEntry(
      name: 'Edge',
      url: 'edge://certificate-manager',
      steps: ' \u2192 Installed by you \u2192 Import',
    ),
    _BrowserEntry(
      name: 'Firefox',
      url: 'about:preferences#privacy',
      steps: ' \u2192 View Certificates \u2192 Authorities \u2192 Import',
    ),
    _BrowserEntry(
      name: 'Safari',
      url: null,
      steps: 'Double-click the file \u2192 Trust in Keychain Access',
    ),
  ];

  @override
  Widget build(BuildContext context) {
    final tt = PiccoloTheme.textTheme;
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text('How to import', style: tt.titleSmall),
        const SizedBox(height: Spacing.sm),
        for (final b in _browsers)
          Padding(
            padding: const EdgeInsets.only(bottom: Spacing.sm),
            child: Row(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                SizedBox(
                  width: 56,
                  child: Text(
                    b.name,
                    style: tt.labelMedium,
                  ),
                ),
                Expanded(
                  child: SelectableText.rich(
                    TextSpan(children: [
                      if (b.url != null)
                        TextSpan(
                          text: b.url,
                          style: tt.labelSmall?.copyWith(
                            color: PiccoloTheme.cobalt600,
                            fontWeight: FontWeight.w500,
                          ),
                        ),
                      TextSpan(text: b.steps, style: tt.labelSmall),
                    ]),
                  ),
                ),
              ],
            ),
          ),
      ],
    );
  }
}

class _BrowserEntry {
  final String name;
  final String? url;
  final String steps;
  const _BrowserEntry({required this.name, this.url, required this.steps});
}
