import 'package:flutter/material.dart';
import 'package:intl/intl.dart';
import 'package:piccolo_os/core/models/remote_models.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/remote_controller.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class RemoteCertificatesCard extends StatelessWidget {

  const RemoteCertificatesCard({required this.controller, super.key});
  final RemoteController controller;
  static final DateFormat _dateFormat = DateFormat.yMMMd();

  @override
  Widget build(BuildContext context) {
    final certs = controller.certificates;

    return Card(
      child: Padding(
        padding: const EdgeInsets.all(Spacing.base),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text('Certificates', style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
            const SizedBox(height: Spacing.base),
            if (certs.isEmpty)
              const Text('No certificates issued yet.', style: TextStyle(color: PiccoloTheme.inkMuted))
            else
              ListView.separated(
                shrinkWrap: true,
                physics: const NeverScrollableScrollPhysics(),
                itemCount: certs.length,
                separatorBuilder: (c, i) => const Divider(),
                itemBuilder: (context, index) => _buildCertItem(context, certs[index]),
              ),
          ],
        ),
      ),
    );
  }

  Widget _buildCertItem(BuildContext context, RemoteCertificate cert) {
    IconData icon;
    Color iconColor;
    String subtitle;
    Color subtitleColor;
    var actionLabel = 'Renew';

    if (cert.status == 'error') {
      icon = PiccoloIcons.error;
      iconColor = PiccoloTheme.critical;
      subtitle = _buildErrorSubtitle(cert);
      subtitleColor = PiccoloTheme.critical;
      actionLabel = 'Retry';
    } else if (cert.status == 'pending') {
      icon = PiccoloIcons.hourglass;
      iconColor = PiccoloTheme.warning;
      subtitle = 'Issuing...';
      subtitleColor = PiccoloTheme.inkMuted;
    } else {
      icon = PiccoloIcons.shieldCheck;
      iconColor = PiccoloTheme.cobalt600;
      final expires = cert.expiresAt;
      final isExpiring = expires != null && expires.difference(DateTime.now()).inDays < 7;
      subtitle = expires != null ? 'Expires ${_formatDate(expires)}' : 'Active';
      subtitleColor = isExpiring ? PiccoloTheme.warning : PiccoloTheme.inkMuted;
    }

    return Row(
      children: [
        Icon(icon, color: iconColor),
        const SizedBox(width: Spacing.base),
        Expanded(
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text(cert.domains.join(', '), style: const TextStyle(fontWeight: FontWeight.bold)),
              Text(subtitle, style: TextStyle(color: subtitleColor, fontSize: 12)),
            ],
          ),
        ),
        TextButton(
          onPressed: () => controller.renewCertificate(cert.id),
          child: Text(actionLabel),
        ),
      ],
    );
  }

  /// Builds an actionable error subtitle with retry ETA when available.
  String _buildErrorSubtitle(RemoteCertificate cert) {
    final reason = cert.failureReason ?? 'Unknown error';
    final retryAt = cert.retryAt;
    if (retryAt == null) return reason;

    final now = DateTime.now();
    if (retryAt.isBefore(now)) return reason;

    final diff = retryAt.difference(now);
    String eta;
    if (diff.inDays > 0) {
      eta = 'retrying ${_formatDate(retryAt)}';
    } else if (diff.inHours > 0) {
      eta = 'retrying in ${diff.inHours}h';
    } else {
      final mins = diff.inMinutes;
      eta = mins > 0 ? 'retrying in ${mins}m' : 'retrying shortly';
    }
    return '$reason · $eta';
  }

  String _formatDate(DateTime dt) => _dateFormat.format(dt.toLocal());
}
