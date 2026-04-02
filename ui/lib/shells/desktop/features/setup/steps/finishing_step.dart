import 'package:flutter/material.dart';
import 'package:piccolo_os/shells/desktop/features/setup/setup_utils.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class FinishingStep extends StatelessWidget {
  const FinishingStep({super.key, this.phase});
  final SetupPhase? phase;

  static const Map<SetupPhase, String> _phaseLabels = {
    SetupPhase.encrypting: 'Encrypting storage\u2026',
    SetupPhase.creatingAdmin: 'Setting up your account\u2026',
    SetupPhase.generatingKey: 'Generating recovery key\u2026',
  };

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.fromLTRB(32, 24, 32, 48),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          const CircularProgressIndicator(color: PiccoloTheme.cobalt600),
          const SizedBox(height: 24),
          Text(
            _phaseLabels[phase] ?? 'Setting up\u2026',
            style: PiccoloTheme.textTheme.bodyLarge,
            textAlign: TextAlign.center,
          ),
          const SizedBox(height: 8),
          Text(
            'This may take up to a minute on some devices.',
            style: PiccoloTheme.textTheme.labelSmall,
            textAlign: TextAlign.center,
          ),
        ],
      ),
    );
  }
}
