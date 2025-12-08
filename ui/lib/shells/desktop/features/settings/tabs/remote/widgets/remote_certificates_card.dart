import 'package:flutter/material.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'package:piccolo_os/core/models/remote_models.dart';
import '../remote_controller.dart';

class RemoteCertificatesCard extends StatelessWidget {
  final RemoteController controller;

  const RemoteCertificatesCard({super.key, required this.controller});

  @override
  Widget build(BuildContext context) {
    final certs = controller.certificates; // [P1] Fixed

    return Card(
      child: Padding(
        padding: const EdgeInsets.all(16.0),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Row(
              mainAxisAlignment: MainAxisAlignment.spaceBetween,
              children: [
                Text("Certificates", style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
              ],
            ),
            const SizedBox(height: 16),
            if (certs.isEmpty)
              const Text("No certificates issued yet.", style: TextStyle(color: PiccoloTheme.inkMuted))
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
    final expires = cert.expiresAt;
    final isExpiring = expires != null && expires.difference(DateTime.now()).inDays < 7;
    
    return Row(
      children: [
        const Icon(Icons.verified_user, color: PiccoloTheme.cobalt600),
        const SizedBox(width: 16),
        Expanded(
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text(cert.domains.join(", "), style: const TextStyle(fontWeight: FontWeight.bold)),
              Text(
                expires != null ? "Expires: ${expires.toLocal().toString().split(' ')[0]}" : "No expiry",
                style: TextStyle(
                  color: isExpiring ? PiccoloTheme.warning : PiccoloTheme.inkMuted,
                  fontSize: 12,
                ),
              ),
            ],
          ),
        ),
        TextButton(
          onPressed: () => controller.renewCertificate(cert.id),
          child: const Text("Renew"),
        ),
      ],
    );
  }
}
