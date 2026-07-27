import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/capability_models.dart';
import 'package:piccolo_os/shared/widgets/piccolo_card.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class CapabilityProviderCard extends StatelessWidget {
  const CapabilityProviderCard({
    required this.capability,
    required this.status,
    required this.appInstance,
    required this.loading,
    required this.error,
    required this.isSelecting,
    required this.actionsPaused,
    required this.onSetDefault,
    required this.onRetry,
    super.key,
  });

  final String capability;
  final CapabilityStatus? status;
  final String appInstance;
  final bool loading;
  final String? error;
  final bool isSelecting;
  final bool actionsPaused;
  final Future<void> Function(CapabilityStatus status) onSetDefault;
  final VoidCallback onRetry;

  @override
  Widget build(BuildContext context) {
    final current = status;
    if (loading || current == null || error != null) {
      return PiccoloCard(
        padding: const EdgeInsets.all(Spacing.base),
        child: Row(
          children: [
            if (loading)
              const SizedBox.square(
                dimension: 18,
                child: CircularProgressIndicator(strokeWidth: 2),
              )
            else
              const Icon(PiccoloIcons.warning, color: PiccoloTheme.warning),
            const SizedBox(width: Spacing.md),
            Expanded(
              child: Text(
                loading
                    ? 'Loading provider status...'
                    : error ?? 'Provider status is unavailable.',
              ),
            ),
            if (!loading)
              TextButton(onPressed: onRetry, child: const Text('Retry')),
          ],
        ),
      );
    }

    final provider = current.providerFor(appInstance);
    final enabled = provider?.enabled ?? false;
    final isDefault = current.isDefault(appInstance);
    final disclosureAvailable = current.providerChangeDisclosure
        .trim()
        .isNotEmpty;
    final canSelect =
        enabled &&
        !isDefault &&
        !isSelecting &&
        !actionsPaused &&
        disclosureAvailable;

    return PiccoloCard(
      padding: const EdgeInsets.all(Spacing.base),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          Wrap(
            spacing: Spacing.md,
            runSpacing: Spacing.xs,
            alignment: WrapAlignment.spaceBetween,
            children: [
              Text(
                capability == 'ai.inference.openai.v1'
                    ? 'AI inference provider'
                    : capability,
                style: PiccoloTheme.textTheme.titleMedium,
              ),
              Text(
                isDefault
                    ? enabled
                          ? 'Default'
                          : 'Default · stopped'
                    : enabled
                    ? 'Available'
                    : 'Unavailable',
                style: PiccoloTheme.textTheme.labelMedium?.copyWith(
                  color: !enabled
                      ? PiccoloTheme.warning
                      : isDefault
                      ? PiccoloTheme.success
                      : PiccoloTheme.inkMuted,
                  fontWeight: FontWeight.w700,
                ),
              ),
            ],
          ),
          if (!isDefault) ...[
            const SizedBox(height: Spacing.sm),
            Text(
              current.defaultProvider.trim().isEmpty
                  ? 'No default provider selected.'
                  : 'Current default: ${current.defaultProvider}',
              style: PiccoloTheme.textTheme.bodySmall,
            ),
            if (!enabled || !disclosureAvailable) ...[
              const SizedBox(height: Spacing.sm),
              Text(
                !enabled
                    ? 'Start this app before selecting it.'
                    : 'Provider-change details are unavailable.',
                style: PiccoloTheme.textTheme.bodySmall?.copyWith(
                  color: PiccoloTheme.warning,
                ),
              ),
            ],
            const SizedBox(height: Spacing.md),
            Align(
              alignment: Alignment.centerRight,
              child: disclosureAvailable
                  ? FilledButton(
                      onPressed: canSelect
                          ? () => unawaited(_confirm(context, current))
                          : null,
                      child: Text(
                        isSelecting ? 'Setting default...' : 'Set as default',
                      ),
                    )
                  : TextButton(
                      onPressed: onRetry,
                      child: const Text('Refresh'),
                    ),
            ),
          ],
        ],
      ),
    );
  }

  Future<void> _confirm(
    BuildContext context,
    CapabilityStatus current,
  ) async {
    final confirmed = await showDialog<bool>(
      context: context,
      builder: (context) => AlertDialog(
        scrollable: true,
        title: Text('Set $appInstance as default?'),
        content: Text(current.providerChangeDisclosure),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(context).pop(false),
            child: const Text('Cancel'),
          ),
          FilledButton(
            onPressed: () => Navigator.of(context).pop(true),
            child: const Text('Set as default'),
          ),
        ],
      ),
    );
    if (confirmed ?? false) await onSetDefault(current);
  }
}
