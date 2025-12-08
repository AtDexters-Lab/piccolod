import 'package:flutter/material.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';
import 'package:piccolo_os/core/models/remote_models.dart';

class RemotePreflightList extends StatelessWidget {
  final List<RemotePreflightCheck> checks;

  const RemotePreflightList({super.key, required this.checks});

  @override
  Widget build(BuildContext context) {
    if (checks.isEmpty) return const SizedBox.shrink();

    return Container(
      decoration: BoxDecoration(
        border: Border.all(color: PiccoloTheme.inkMuted.withValues(alpha: 0.2)),
        borderRadius: BorderRadius.circular(8),
      ),
      child: ListView.separated(
        shrinkWrap: true,
        physics: const NeverScrollableScrollPhysics(),
        itemCount: checks.length,
        separatorBuilder: (c, i) => const Divider(height: 1),
        itemBuilder: (context, index) {
          final check = checks[index];
          IconData icon;
          Color color;
          
          switch(check.status) {
            case 'pass': 
              icon = Icons.check_circle; 
              color = PiccoloTheme.success;
              break;
            case 'warn':
              icon = Icons.warning;
              color = PiccoloTheme.warning;
              break;
            case 'fail':
            default:
              icon = Icons.error;
              color = PiccoloTheme.critical;
          }

          return ListTile(
            leading: Icon(icon, color: color),
            title: Text(check.name),
            subtitle: check.detail != null ? Text(check.detail!) : null,
            trailing: check.status == 'fail' ? const Chip(label: Text("Failed"), backgroundColor: Colors.redAccent, labelStyle: TextStyle(color: Colors.white)) : null,
          );
        },
      ),
    );
  }
}
