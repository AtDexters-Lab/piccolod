import 'package:flutter/material.dart';
import 'package:piccolo_os/core/utils/downloader/downloader.dart';
import 'package:piccolo_os/shared/widgets/status_banner.dart';
import 'package:piccolo_os/shells/desktop/features/setup/setup_utils.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class RecoveryKeyStep extends StatefulWidget {
  const RecoveryKeyStep({
    required this.words,
    required this.onNext,
    this.entryContext = RecoveryKeyEntryContext.postSetup,
    this.wasRotated = false,
    super.key,
  });
  final List<String> words;
  final VoidCallback onNext;
  final RecoveryKeyEntryContext entryContext;
  final bool wasRotated;

  @override
  State<RecoveryKeyStep> createState() => _RecoveryKeyStepState();
}

class _RecoveryKeyStepState extends State<RecoveryKeyStep> {
  bool _confirmed = false;

  void _downloadKey() {
    final content =
        'PICCOLO RECOVERY KEY\n\n${widget.words.join(' ')}\n\nKeep this file safe.';
    downloadTextFile(content, 'piccolo-recovery-key.txt');

    ScaffoldMessenger.of(context).showSnackBar(
      const SnackBar(
        content: Text('Downloading recovery key...'),
        duration: Duration(seconds: 2),
      ),
    );
  }

  /// Per-context subtitle. Empty for postSetup — the stepper above the screen
  /// already names the step ("Recovery"), so an extra line would be redundant.
  /// Banner and subtitle render together when both apply: banner conveys
  /// urgency, subtitle conveys context — they occupy different semantic slots.
  String? get _contextSubtitle {
    switch (widget.entryContext) {
      case RecoveryKeyEntryContext.postSetup:
        return null;
      case RecoveryKeyEntryContext.postLogin:
        return "Save your recovery key — it's how you'll reset your "
            'password if you forget it.';
      case RecoveryKeyEntryContext.postPasskeyRegister:
        return 'Your passkey is set up. One more thing: save the recovery '
            'key below.';
    }
  }

  @override
  Widget build(BuildContext context) {
    final subtitle = _contextSubtitle;
    return Padding(
      padding: const EdgeInsets.fromLTRB(24, 0, 24, 16),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          if (widget.wasRotated) ...[
            // REC-3: silent-rotation banner. Reached when the previous key
            // was generated but never acknowledged on this device — the
            // server rotated to fresh words on this /generate, so any paper
            // saved from a prior generation is no longer valid. Operator-
            // centric copy avoids asserting "your previous key" (a referent
            // they may not remember) — instead anchors on what they can
            // observe: words on paper that no longer work.
            const StatusBanner(
              severity: BannerSeverity.warning,
              title: 'These are new recovery words',
              message:
                  'If you wrote down a previous recovery key, those words '
                  'no longer work — please use these instead.',
            ),
            const SizedBox(height: 10),
          ],
          Text(
            'Your recovery key',
            style: PiccoloTheme.textTheme.bodyLarge?.copyWith(
              fontWeight: FontWeight.bold,
            ),
          ),
          if (subtitle != null) ...[
            const SizedBox(height: 4),
            Text(
              subtitle,
              textAlign: TextAlign.center,
              style: PiccoloTheme.textTheme.bodySmall?.copyWith(
                color: PiccoloTheme.inkMuted,
              ),
            ),
          ],
          const SizedBox(height: 10),
          Container(
            padding: const EdgeInsets.all(10),
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
                Icon(PiccoloIcons.info, color: PiccoloTheme.inkMuted, size: 18),
                SizedBox(width: 8),
                Expanded(
                  child: Text(
                    "Your 24-word recovery key is below. Store it offline; you'll need it to reset your encryption password.",
                    style: TextStyle(fontSize: 13, color: PiccoloTheme.inkMuted),
                  ),
                ),
              ],
            ),
          ),
          const SizedBox(height: 12),
          Container(
            padding: const EdgeInsets.all(14),
            decoration: BoxDecoration(
              color: PiccoloTheme.porcelain,
              borderRadius: BorderRadius.circular(Radii.sm),
              border: Border.all(
                color: PiccoloTheme.ink.withValues(alpha: 0.06),
              ),
            ),
            child: SelectableText(
              widget.words
                  .asMap()
                  .entries
                  .map((e) => '${e.key + 1}. ${e.value}')
                  .join('  '),
              style: PiccoloTheme.mono.copyWith(
                fontSize: 13,
                height: 1.5,
              ),
            ),
          ),
          const SizedBox(height: 12),
          OutlinedButton.icon(
            onPressed: _downloadKey,
            icon: const Icon(PiccoloIcons.download, size: 16),
            label: const Text('Download key'),
            style: OutlinedButton.styleFrom(foregroundColor: PiccoloTheme.ink),
          ),
          const SizedBox(height: 12),
          InkWell(
            onTap: () => setState(() => _confirmed = !_confirmed),
            child: Row(
              children: [
                Checkbox(
                  value: _confirmed,
                  onChanged: (v) => setState(() => _confirmed = v ?? false),
                  activeColor: PiccoloTheme.cobalt600,
                ),
                Expanded(
                  child: Text(
                    "I've saved this recovery key somewhere safe.",
                    style: PiccoloTheme.textTheme.bodyMedium,
                  ),
                ),
              ],
            ),
          ),
          const SizedBox(height: 14),
          FilledButton(
            onPressed: _confirmed ? widget.onNext : null,
            style: FilledButton.styleFrom(
              disabledBackgroundColor: PiccoloTheme.ink.withValues(alpha: 0.1),
              disabledForegroundColor: PiccoloTheme.ink.withValues(alpha: 0.3),
            ),
            child: const Text('Continue'),
          ),
        ],
      ),
    );
  }
}
