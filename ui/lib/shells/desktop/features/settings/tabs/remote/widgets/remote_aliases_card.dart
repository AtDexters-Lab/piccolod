import 'package:flutter/material.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'package:piccolo_os/core/models/remote_models.dart';
import '../remote_controller.dart';

class RemoteAliasesCard extends StatelessWidget {
  final RemoteController controller;

  const RemoteAliasesCard({super.key, required this.controller});

  @override
  Widget build(BuildContext context) {
    final aliases = controller.aliases; // [P1] Fixed

    return Card(
      child: Padding(
        padding: const EdgeInsets.all(16.0),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Row(
              mainAxisAlignment: MainAxisAlignment.spaceBetween,
              children: [
                Text("Aliases", style: PiccoloTheme.textTheme.bodyLarge?.copyWith(fontWeight: FontWeight.bold)),
                IconButton(
                  icon: const Icon(Icons.add),
                  onPressed: () => _showAddAliasDialog(context),
                ),
              ],
            ),
            const SizedBox(height: 16),
            if (aliases.isEmpty)
              const Text("No aliases configured.", style: TextStyle(color: PiccoloTheme.inkMuted))
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
    return ListTile(
      contentPadding: EdgeInsets.zero,
      leading: const Icon(Icons.link, color: PiccoloTheme.inkMuted),
      title: Text(alias.hostname),
      subtitle: Text("Points to: ${alias.listener}"),
      trailing: IconButton(
        icon: const Icon(Icons.delete_outline, color: PiccoloTheme.critical),
        onPressed: () => controller.deleteAlias(alias.id),
      ),
    );
  }

  void _showAddAliasDialog(BuildContext context) {
    final hostCtrl = TextEditingController();
    String? selectedListener;

    showDialog(
      context: context,
      builder: (context) => StatefulBuilder(
        builder: (context, setState) {
          return AlertDialog(
            title: const Text("Add Alias"),
            content: Column(
              mainAxisSize: MainAxisSize.min,
              children: [
                TextField(
                  controller: hostCtrl,
                  decoration: const InputDecoration(labelText: "Public Hostname", hintText: "app.example.com"),
                ),
                const SizedBox(height: 16),
                if (controller.services.isEmpty)
                   const Text("No services available to alias.", style: TextStyle(color: PiccoloTheme.warning))
                else
                  DropdownButtonFormField<String>(
                    decoration: const InputDecoration(labelText: "Internal Service"),
                    value: selectedListener,
                    items: controller.services.map((s) {
                      return DropdownMenuItem(
                        value: s.name,
                        child: Text("${s.name} (${s.publicPort})"),
                      );
                    }).toList(),
                    onChanged: (val) {
                      setState(() {
                        selectedListener = val;
                      });
                    },
                  ),
              ],
            ),
            actions: [
              TextButton(onPressed: () => Navigator.pop(context), child: const Text("Cancel")),
              ElevatedButton(
                onPressed: () {
                  if (hostCtrl.text.isNotEmpty && selectedListener != null) {
                    controller.addAlias(hostCtrl.text, selectedListener!);
                    Navigator.pop(context);
                  }
                },
                child: const Text("Add"),
              ),
            ],
          );
        },
      ),
    );
  }
}
