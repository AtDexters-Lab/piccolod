import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/event_stream_client.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'remote_controller.dart';
import 'widgets/remote_dashboard.dart';
import 'widgets/remote_setup_wizard.dart';

class RemoteTab extends StatefulWidget {
  final EventStreamClient? eventStreamClient;

  const RemoteTab({super.key, this.eventStreamClient});

  @override
  State<RemoteTab> createState() => _RemoteTabState();
}

class _RemoteTabState extends State<RemoteTab> {
  late final RemoteController _controller;

  @override
  void initState() {
    super.initState();
    _controller = RemoteController(eventStreamClient: widget.eventStreamClient);
  }

  @override
  void dispose() {
    _controller.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return ListenableBuilder(
      listenable: _controller,
      builder: (context, child) {
        if (_controller.isLoading && _controller.status == null) {
          return const Center(child: CircularProgressIndicator());
        }

        if (_controller.error != null && _controller.status == null) {
          return Center(
            child: Column(
              mainAxisSize: MainAxisSize.min,
              children: [
                const Icon(Icons.cloud_off, size: 48, color: PiccoloTheme.inkMuted),
                const SizedBox(height: 16),
                Text(
                  "Remote Access Unavailable",
                  style: PiccoloTheme.textTheme.bodyLarge,
                ),
                Text(
                  _controller.error!,
                  style: PiccoloTheme.textTheme.labelSmall?.copyWith(color: PiccoloTheme.critical),
                ),
                const SizedBox(height: 16),
                ElevatedButton(
                  onPressed: _controller.refresh,
                  child: const Text("Retry"),
                ),
              ],
            ),
          );
        }

        // [P1] Locked UI
        if (_controller.isLocked) {
           return Center(
             child: Column(
               mainAxisSize: MainAxisSize.min,
               children: [
                 const Icon(Icons.lock_outline, size: 48, color: PiccoloTheme.warning),
                 const SizedBox(height: 16),
                 Text(
                   "Storage Locked",
                   style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold),
                 ),
                 const SizedBox(height: 8),
                 Text(
                   "The secure storage volume is locked. You must unlock it to manage remote access keys.",
                   textAlign: TextAlign.center,
                   style: PiccoloTheme.textTheme.bodyMedium,
                 ),
                 // In a real app, we might have a button to trigger unlock or just tell them to use the main unlock flow.
               ],
             ),
           );
        }

        // Configuration / Provisioning Loading States
        if (_controller.isSubmittingConfig) {
          return const Center(
            child: Column(
              mainAxisSize: MainAxisSize.min,
              children: [
                CircularProgressIndicator(),
                SizedBox(height: 16),
                Text("Applying Configuration...", style: TextStyle(color: PiccoloTheme.inkMuted)),
              ],
            ),
          );
        }

        final state = _controller.status?.state ?? 'disabled';
        
        Widget content;

        // Allow wizard to handle 'provisioning' so user can resume/reset
        if (state == 'disabled' || state == 'stopped' || state == 'provisioning') {
           content = RemoteSetupWizard(controller: _controller);
        } else {
           content = RemoteDashboard(controller: _controller);
        }

        if (_controller.error != null) {
          return Column(
            children: [
              Container(
                width: double.infinity,
                margin: const EdgeInsets.only(bottom: 16),
                padding: const EdgeInsets.all(12),
                decoration: BoxDecoration(
                  color: PiccoloTheme.critical.withValues(alpha: 0.1),
                  border: Border.all(color: PiccoloTheme.critical),
                  borderRadius: BorderRadius.circular(8),
                ),
                child: Row(
                  children: [
                    const Icon(Icons.error_outline, color: PiccoloTheme.critical),
                    const SizedBox(width: 12),
                    Expanded(
                      child: Text(
                        _controller.error!,
                        style: const TextStyle(color: PiccoloTheme.critical, fontWeight: FontWeight.bold),
                      ),
                    ),
                    IconButton(
                      icon: const Icon(Icons.close, color: PiccoloTheme.critical, size: 20),
                      onPressed: () {
                         // We need a way to clear the error. 
                         // Ideally controller should have clearError() but for now we can just trigger a refresh which clears it.
                         _controller.refresh();
                      },
                    ),
                  ],
                ),
              ),
              content,
            ],
          );
        }
        
        return content;
      },
    );
  }
}
