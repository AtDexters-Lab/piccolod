import 'package:flutter/material.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import '../remote_controller.dart';
import 'remote_certificates_card.dart';
import 'remote_aliases_card.dart';
import 'remote_events_card.dart';

class RemoteDashboard extends StatelessWidget {
  final RemoteController controller;

  const RemoteDashboard({super.key, required this.controller});

  @override
  Widget build(BuildContext context) {
    final status = controller.status!;
    
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        _buildHeader(context),
        const SizedBox(height: 24),
        if (status.warnings.isNotEmpty) ...[
          _buildWarnings(context, status.warnings),
          const SizedBox(height: 24),
        ],
        _buildInfoCards(context),
        const SizedBox(height: 24),
        RemoteCertificatesCard(controller: controller),
        const SizedBox(height: 24),
        RemoteAliasesCard(controller: controller),
        const SizedBox(height: 24),
        RemoteEventsCard(controller: controller), // [P2] Added
        const SizedBox(height: 32),
        const Divider(),
        const SizedBox(height: 32),
        _buildDangerZone(context),
      ],
    );
  }

  Widget _buildHeader(BuildContext context) {
    final status = controller.status!;
    Color statusColor = PiccoloTheme.success;
    String statusText = "Active";
    IconData statusIcon = Icons.check_circle;

    if (status.state == 'error' || status.warnings.isNotEmpty) {
      statusColor = PiccoloTheme.warning;
      statusText = "Degraded";
      statusIcon = Icons.warning_amber_rounded;
    }
    if (status.state == 'error') {
      statusColor = PiccoloTheme.critical;
      statusText = "Error";
      statusIcon = Icons.error_outline;
    }

    return Row(
      mainAxisAlignment: MainAxisAlignment.spaceBetween,
      children: [
        Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text("Remote Access", style: PiccoloTheme.textTheme.displayLarge?.copyWith(fontSize: 24)),
            const SizedBox(height: 8),
            Row(
              children: [
                Icon(statusIcon, color: statusColor, size: 20),
                const SizedBox(width: 8),
                Text(statusText, style: TextStyle(color: statusColor, fontWeight: FontWeight.bold)),
                if (status.latencyMs != null) ...[
                  const SizedBox(width: 16),
                  const Icon(Icons.speed, size: 16, color: PiccoloTheme.inkMuted),
                  const SizedBox(width: 4),
                  Text("${status.latencyMs}ms latency", style: PiccoloTheme.textTheme.labelSmall),
                ],
              ],
            ),
          ],
        ),
        ElevatedButton.icon(
          onPressed: controller.refresh,
          icon: const Icon(Icons.refresh),
          label: const Text("Refresh"),
        ),
      ],
    );
  }

  Widget _buildWarnings(BuildContext context, List<String> warnings) {
    return Container(
      padding: const EdgeInsets.all(16),
      decoration: BoxDecoration(
        color: PiccoloTheme.warning.withValues(alpha: 0.1),
        borderRadius: BorderRadius.circular(8),
        border: Border.all(color: PiccoloTheme.warning.withValues(alpha: 0.3)),
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: warnings.map((w) => Padding(
          padding: const EdgeInsets.only(bottom: 4),
          child: Row(
            children: [
              const Icon(Icons.warning, color: PiccoloTheme.warning, size: 16),
              const SizedBox(width: 8),
              Expanded(child: Text(w, style: const TextStyle(color: PiccoloTheme.ink))),
            ],
          ),
        )).toList(),
      ),
    );
  }

  Widget _buildInfoCards(BuildContext context) {
    final status = controller.status!;
    return Row(
      children: [
        Expanded(
          child: _InfoCard(
            label: "Portal Hostname",
            value: status.portalHostname ?? "Not Configured",
            icon: Icons.link,
          ),
        ),
        const SizedBox(width: 16),
        Expanded(
          child: _InfoCard(
            label: "Nexus Relay",
            value: status.endpoint ?? "Unknown",
            icon: Icons.router,
          ),
        ),
      ],
    );
  }

  Widget _buildDangerZone(BuildContext context) {
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Text("Danger Zone", style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold, color: PiccoloTheme.critical)),
        const SizedBox(height: 16),
        Container(
          padding: const EdgeInsets.all(16),
          decoration: BoxDecoration(
            border: Border.all(color: PiccoloTheme.critical.withValues(alpha: 0.3)),
            borderRadius: BorderRadius.circular(8),
          ),
          child: Row(
            children: [
              Expanded(
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    const Text("Disable Remote Access", style: TextStyle(fontWeight: FontWeight.bold)),
                    Text("This will immediately stop external access to your device.", style: PiccoloTheme.textTheme.labelSmall),
                  ],
                ),
              ),
              OutlinedButton(
                onPressed: () => _confirmDisable(context),
                style: OutlinedButton.styleFrom(foregroundColor: PiccoloTheme.critical),
                child: const Text("Disable"),
              ),
            ],
          ),
        ),
        const SizedBox(height: 16),
        Container(
          padding: const EdgeInsets.all(16),
          decoration: BoxDecoration(
            border: Border.all(color: PiccoloTheme.inkMuted.withValues(alpha: 0.3)),
            borderRadius: BorderRadius.circular(8),
          ),
          child: Row(
            children: [
              Expanded(
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    const Text("Rotate Device Secret", style: TextStyle(fontWeight: FontWeight.bold)),
                    Text("Invalidates the current authentication token with the Nexus relay.", style: PiccoloTheme.textTheme.labelSmall),
                  ],
                ),
              ),
              OutlinedButton(
                onPressed: () => _confirmRotate(context),
                child: const Text("Rotate Secret"),
              ),
            ],
          ),
        ),
      ],
    );
  }

  void _confirmDisable(BuildContext parentContext) {
    showDialog(
      context: parentContext,
      builder: (dialogContext) => AlertDialog(
        title: const Text("Disable Remote Access?"),
        content: const Text("You will lose access to this dashboard from the internet. You must be on the local network to re-enable it."),
        actions: [
          TextButton(onPressed: () => Navigator.pop(dialogContext), child: const Text("Cancel")),
          ElevatedButton(
            style: ElevatedButton.styleFrom(backgroundColor: PiccoloTheme.critical),
            onPressed: () async {
              final messenger = ScaffoldMessenger.of(parentContext);
              Navigator.pop(dialogContext);
              await controller.disableRemote();
              messenger.showSnackBar(
                const SnackBar(content: Text("Remote access disabled")),
              );
            },
            child: const Text("Disable"),
          ),
        ],
      ),
    );
  }

  void _confirmRotate(BuildContext parentContext) {
    showDialog(
      context: parentContext,
      builder: (dialogContext) => AlertDialog(
        title: const Text("Rotate Credentials?"),
        content: const Text("This will briefly disconnect the tunnel. Ensure your configuration is backed up."),
        actions: [
          TextButton(onPressed: () => Navigator.pop(dialogContext), child: const Text("Cancel")),
          ElevatedButton(
            onPressed: () async {
              Navigator.pop(dialogContext); // Close confirm dialog
              final secret = await controller.rotateCredentials();
              if (secret != null && parentContext.mounted) {
                _showSecretDialog(parentContext, secret, onDismiss: controller.refresh);
              }
            },
            child: const Text("Rotate"),
          ),
        ],
      ),
    );
  }

  void _showSecretDialog(BuildContext context, String secret, {VoidCallback? onDismiss}) {
    showDialog(
      context: context,
      barrierDismissible: false,
      builder: (context) => AlertDialog(
        title: const Text("New Device Secret"),
        content: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            const Text(
              "Credentials rotated successfully. You must update your Nexus Relay with this new secret to reconnect.",
              style: TextStyle(color: PiccoloTheme.ink),
            ),
            const SizedBox(height: 16),
            Container(
              padding: const EdgeInsets.all(16),
              width: double.infinity,
              decoration: BoxDecoration(
                color: PiccoloTheme.mist,
                borderRadius: BorderRadius.circular(8),
                border: Border.all(color: PiccoloTheme.cobalt600),
              ),
              child: SelectableText(
                secret,
                style: const TextStyle(fontWeight: FontWeight.bold, fontSize: 16, fontFamily: 'monospace'),
              ),
            ),
          ],
        ),
        actions: [
          ElevatedButton(
            onPressed: () {
              Navigator.pop(context);
              onDismiss?.call();
            }, 
            child: const Text("Done"),
          ),
        ],
      ),
    );
  }
}

class _InfoCard extends StatelessWidget {
  final String label;
  final String value;
  final IconData icon;

  const _InfoCard({required this.label, required this.value, required this.icon});

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.all(16),
      decoration: BoxDecoration(
        color: Colors.white,
        borderRadius: BorderRadius.circular(8),
        boxShadow: [
          BoxShadow(color: Colors.black.withValues(alpha: 0.05), blurRadius: 4, offset: const Offset(0, 2)),
        ],
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Row(
            children: [
              Icon(icon, size: 16, color: PiccoloTheme.cobalt600),
              const SizedBox(width: 8),
              Text(label, style: PiccoloTheme.textTheme.labelSmall),
            ],
          ),
          const SizedBox(height: 8),
          Text(value, style: const TextStyle(fontWeight: FontWeight.w600, fontSize: 15), overflow: TextOverflow.ellipsis),
        ],
      ),
    );
  }
}
