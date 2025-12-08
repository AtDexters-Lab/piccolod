import 'package:flutter/material.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'package:piccolo_os/core/models/remote_models.dart';
import '../remote_controller.dart';
import 'package:intl/intl.dart';

class RemoteEventsCard extends StatelessWidget {
  final RemoteController controller;

  const RemoteEventsCard({super.key, required this.controller});

  @override
  Widget build(BuildContext context) {
    final events = controller.events;
    // Show only last 5 events in preview
    final previewEvents = events.take(5).toList();

    return Card(
      child: Padding(
        padding: const EdgeInsets.all(16.0),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Row(
              mainAxisAlignment: MainAxisAlignment.spaceBetween,
              children: [
                Text("Activity Log", style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
                if (events.length > 5)
                  TextButton(
                    onPressed: () => _showFullLogDialog(context),
                    child: const Text("View All"),
                  ),
              ],
            ),
            const SizedBox(height: 16),
            if (events.isEmpty)
              const Text("No recent activity.", style: TextStyle(color: PiccoloTheme.inkMuted))
            else
              Container(
                decoration: BoxDecoration(
                  color: PiccoloTheme.mist,
                  borderRadius: BorderRadius.circular(8),
                ),
                child: ListView.separated(
                  shrinkWrap: true,
                  physics: const NeverScrollableScrollPhysics(),
                  padding: const EdgeInsets.all(8),
                  itemCount: previewEvents.length,
                  separatorBuilder: (c, i) => const Divider(height: 1),
                  itemBuilder: (context, index) => _buildEventItem(previewEvents[index]),
                ),
              ),
          ],
        ),
      ),
    );
  }

  void _showFullLogDialog(BuildContext context) {
    showDialog(
      context: context,
      builder: (context) => AlertDialog(
        title: const Text("Activity Log"),
        content: SizedBox(
          width: 500,
          height: 400,
          child: ListView.separated(
            itemCount: controller.events.length,
            separatorBuilder: (c, i) => const Divider(height: 1),
            itemBuilder: (context, index) => _buildEventItem(controller.events[index]),
          ),
        ),
        actions: [
          TextButton(onPressed: () => Navigator.pop(context), child: const Text("Close")),
        ],
      ),
    );
  }

  Widget _buildEventItem(RemoteEvent event) {
    final dateFormat = DateFormat('MM/dd HH:mm:ss');
    Color levelColor = PiccoloTheme.ink;
    if (event.level == 'warn') levelColor = PiccoloTheme.warning;
    if (event.level == 'error') levelColor = PiccoloTheme.critical;

    return Padding(
      padding: const EdgeInsets.symmetric(vertical: 8),
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
           Text(dateFormat.format(event.ts.toLocal()), style: const TextStyle(fontSize: 12, color: PiccoloTheme.inkMuted, fontFamily: 'monospace')),
           const SizedBox(width: 8),
           Expanded(
             child: Column(
               crossAxisAlignment: CrossAxisAlignment.start,
               children: [
                 Text(event.message, style: TextStyle(color: levelColor, fontSize: 13)),
                 if (event.nextStep != null)
                   Padding(
                     padding: const EdgeInsets.only(top: 2),
                     child: Text("Tip: ${event.nextStep}", style: const TextStyle(fontSize: 12, color: PiccoloTheme.info, fontStyle: FontStyle.italic)),
                   ),
               ],
             ),
           ),
        ],
      ),
    );
  }
}
