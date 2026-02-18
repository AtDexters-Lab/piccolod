import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/remote_models.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class RemotePreflightList extends StatelessWidget {

  const RemotePreflightList({required this.checks, super.key});
  final List<RemotePreflightCheck> checks;

  @override
  Widget build(BuildContext context) {
    if (checks.isEmpty) return const SizedBox.shrink();

    return Container(
      decoration: BoxDecoration(
        border: Border.all(color: PiccoloTheme.hairline),
        borderRadius: BorderRadius.circular(Radii.sm),
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
              icon = PiccoloIcons.success;
              color = PiccoloTheme.success;
            case 'warn':
              icon = PiccoloIcons.warning;
              color = PiccoloTheme.warning;
            case 'fail':
            default:
              icon = PiccoloIcons.error;
              color = PiccoloTheme.critical;
          }

          return ListTile(
            leading: Icon(icon, color: color),
            title: Text(check.name),
            subtitle: check.detail != null ? Text(check.detail!) : null,
            trailing: check.status == 'fail' ? const Chip(label: Text('Failed'), backgroundColor: PiccoloTheme.critical, labelStyle: TextStyle(color: PiccoloTheme.porcelain)) : null,
          );
        },
      ),
    );
  }
}
