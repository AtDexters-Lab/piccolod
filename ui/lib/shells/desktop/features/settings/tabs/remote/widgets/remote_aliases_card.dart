import 'dart:async';

import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/remote_models.dart';
import 'package:piccolo_os/shells/desktop/features/settings/tabs/remote/remote_controller.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class RemoteAliasesCard extends StatelessWidget {

  const RemoteAliasesCard({required this.controller, super.key});
  final RemoteController controller;

  @override
  Widget build(BuildContext context) {
    final aliases = controller.aliases; // [P1] Fixed

    return Card(
      child: Padding(
        padding: const EdgeInsets.all(Spacing.base),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Row(
              mainAxisAlignment: MainAxisAlignment.spaceBetween,
              children: [
                Text('Aliases', style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
                IconButton(
                  icon: const Icon(PiccoloIcons.add),
                  onPressed: () => _showAddAliasDialog(context),
                ),
              ],
            ),
            const SizedBox(height: Spacing.base),
            if (aliases.isEmpty)
              const Text('No aliases configured.', style: TextStyle(color: PiccoloTheme.inkMuted))
            else
              ListView.separated(
                shrinkWrap: true,
                physics: const NeverScrollableScrollPhysics(),
                itemCount: aliases.length,
                separatorBuilder: (c, i) => const Divider(),
                itemBuilder: (context, index) => _buildAliasItem(context, aliases[index]),
              ),
          ],
        ),
      ),
    );
  }

  Widget _buildAliasItem(BuildContext context, RemoteAlias alias) {
    final label = alias.listener == '__portal' ? 'Portal' : alias.listener;
    return ListTile(
      contentPadding: EdgeInsets.zero,
      leading: const Icon(PiccoloIcons.link, color: PiccoloTheme.inkMuted),
      title: Text(alias.hostname),
      subtitle: Text('Points to: $label'),
      trailing: IconButton(
        icon: const Icon(PiccoloIcons.delete, color: PiccoloTheme.critical),
        onPressed: () => controller.deleteAlias(alias.id),
      ),
    );
  }

  void _showAddAliasDialog(BuildContext context) {
    final hostCtrl = TextEditingController();
    String? selectedListener;

    // Filter to services with a derived host label (HTTP/WS listeners only).
    final routableServices = controller.services
        .where((s) => s.derivedHostLabel != null && s.derivedHostLabel!.isNotEmpty)
        .toList();

    unawaited(showDialog<void>(
      context: context,
      builder: (context) => StatefulBuilder(
        builder: (context, setState) {
          return AlertDialog(
            title: const Text('Add Alias'),
            content: Column(
              mainAxisSize: MainAxisSize.min,
              children: [
                TextField(
                  controller: hostCtrl,
                  decoration: const InputDecoration(labelText: 'Public Hostname', hintText: 'app.example.com'),
                ),
                const SizedBox(height: Spacing.base),
                DropdownButtonFormField<String>(
                  decoration: const InputDecoration(labelText: 'Route To'),
                  initialValue: selectedListener,
                  items: [
                    const DropdownMenuItem(value: '__portal', child: Text('Portal')),
                    ...routableServices.map((s) {
                      return DropdownMenuItem(
                        value: s.derivedHostLabel,
                        child: Text('${s.name} (${s.app})'),
                      );
                    }),
                  ],
                  onChanged: (val) {
                    setState(() {
                      selectedListener = val;
                    });
                  },
                ),
              ],
            ),
            actions: [
              TextButton(onPressed: () => Navigator.pop(context), child: const Text('Cancel')),
              FilledButton(
                onPressed: () {
                  if (hostCtrl.text.isNotEmpty && selectedListener != null) {
                    unawaited(controller.addAlias(hostCtrl.text, selectedListener!));
                    Navigator.pop(context);
                  }
                },
                child: const Text('Add'),
              ),
            ],
          );
        },
      ),
    ));
  }
}
