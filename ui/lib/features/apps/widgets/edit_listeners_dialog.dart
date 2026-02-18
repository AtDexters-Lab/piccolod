import 'package:flutter/material.dart';
import 'package:piccolo_os/core/models/app_models.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class EditListenersDialog extends StatefulWidget {

  const EditListenersDialog({
    required this.initialListeners, required this.onSave, super.key,
  });
  final List<AppListener> initialListeners;
  final void Function(List<AppListener>) onSave;

  @override
  State<EditListenersDialog> createState() => _EditListenersDialogState();
}

class _EditListenersDialogState extends State<EditListenersDialog> {
  late List<AppListener> _listeners;
  final _formKey = GlobalKey<FormState>();

  @override
  void initState() {
    super.initState();
    _listeners = List.from(widget.initialListeners);
  }

  void _addListener() {
    setState(() {
      _listeners.add(AppListener(
        name: 'service-${_listeners.length + 1}',
        guestPort: 8080,
      ));
    });
  }

  void _removeListener(int index) {
    setState(() {
      _listeners.removeAt(index);
    });
  }

  void _updateListener(int index, AppListener updated) {
    setState(() {
      _listeners[index] = updated;
    });
  }

  @override
  Widget build(BuildContext context) {
    return AlertDialog(
      title: const Text('Edit Listeners'),
      content: SizedBox(
        width: 700,
        height: 500,
        child: Column(
          children: [
            Container(
              padding: const EdgeInsets.all(Spacing.md),
              decoration: BoxDecoration(
                color: PiccoloTheme.mist,
                borderRadius: BorderRadius.circular(Radii.sm),
              ),
              child: const Row(
                children: [
                  Icon(PiccoloIcons.info, size: 16, color: PiccoloTheme.cobalt600),
                  SizedBox(width: Spacing.sm),
                  Expanded(
                    child: Text(
                      'Updating listeners may cause the application container to recreate.',
                      style: TextStyle(
                        color: PiccoloTheme.inkMuted,
                        fontSize: 13,
                      ),
                    ),
                  ),
                ],
              ),
            ),
            const SizedBox(height: Spacing.base),
            Expanded(
              child: Form(
                key: _formKey,
                child: ListView.separated(
                  itemCount: _listeners.length,
                  separatorBuilder: (context, index) => const Divider(),
                  itemBuilder: (context, index) {
                    final l = _listeners[index];
                    return _ListenerRow(
                      listener: l,
                      onDelete: () => _removeListener(index),
                      onChange: (updated) => _updateListener(index, updated),
                    );
                  },
                ),
              ),
            ),
            const SizedBox(height: Spacing.base),
            OutlinedButton.icon(
              onPressed: _addListener,
              icon: const Icon(PiccoloIcons.add),
              label: const Text('Add Listener'),
            ),
          ],
        ),
      ),
      actions: [
        TextButton(
          onPressed: () => Navigator.of(context).pop(),
          child: const Text('Cancel'),
        ),
        FilledButton(
          onPressed: () {
            if (_formKey.currentState!.validate()) {
              widget.onSave(_listeners);
              Navigator.of(context).pop();
            }
          },
          child: const Text('Save Changes'),
        ),
      ],
    );
  }
}

class _ListenerRow extends StatelessWidget {

  const _ListenerRow({
    required this.listener,
    required this.onDelete,
    required this.onChange,
  });
  final AppListener listener;
  final VoidCallback onDelete;
  final ValueChanged<AppListener> onChange;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.symmetric(vertical: Spacing.sm),
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          Expanded(
            flex: 3,
            child: TextFormField(
              initialValue: listener.name,
              decoration: const InputDecoration(
                labelText: 'Name',
                isDense: true,
                border: OutlineInputBorder(),
              ),
              onChanged: (val) => onChange(_copyWith(name: val)),
              validator: (val) =>
                  val == null || val.isEmpty ? 'Required' : null,
            ),
          ),
          const SizedBox(width: Spacing.md),
          Expanded(
            flex: 2,
            child: TextFormField(
              initialValue: listener.guestPort.toString(),
              decoration: const InputDecoration(
                labelText: 'Port',
                isDense: true,
                border: OutlineInputBorder(),
              ),
              keyboardType: TextInputType.number,
              onChanged: (val) {
                final port = int.tryParse(val);
                if (port != null) onChange(_copyWith(guestPort: port));
              },
              validator: (val) {
                final p = int.tryParse(val ?? '');
                return p == null || p <= 0 || p > 65535 ? 'Invalid' : null;
              },
            ),
          ),
          const SizedBox(width: Spacing.md),
          Expanded(
            flex: 2,
            child: DropdownButtonFormField<String>(
              initialValue: listener.protocol,
              decoration: const InputDecoration(
                labelText: 'Protocol',
                isDense: true,
                border: OutlineInputBorder(),
              ),
              items: const [
                DropdownMenuItem(value: 'raw', child: Text('Raw (TCP/UDP)')),
                DropdownMenuItem(value: 'http', child: Text('HTTP')),
                DropdownMenuItem(value: 'websocket', child: Text('WebSocket')),
              ],
              onChanged: (val) {
                if (val != null) onChange(_copyWith(protocol: val));
              },
            ),
          ),
          const SizedBox(width: Spacing.md),
           Expanded(
            flex: 2,
            child: DropdownButtonFormField<String>(
              initialValue: listener.flow,
              decoration: const InputDecoration(
                labelText: 'Flow',
                isDense: true,
                border: OutlineInputBorder(),
              ),
              items: const [
                DropdownMenuItem(value: 'tcp', child: Text('TCP')),
                DropdownMenuItem(value: 'tls', child: Text('TLS')),
              ],
              onChanged: (val) {
                if (val != null) onChange(_copyWith(flow: val));
              },
            ),
          ),
          const SizedBox(width: Spacing.sm),
          IconButton(
            icon: const Icon(PiccoloIcons.delete, color: PiccoloTheme.critical),
            onPressed: onDelete,
            tooltip: 'Remove Listener',
          ),
        ],
      ),
    );
  }

  AppListener _copyWith({
    String? name,
    int? guestPort,
    String? flow,
    String? protocol,
  }) {
    return AppListener(
      name: name ?? listener.name,
      guestPort: guestPort ?? listener.guestPort,
      flow: flow ?? listener.flow,
      protocol: protocol ?? listener.protocol,
      remotePorts: listener.remotePorts,
      middleware: listener.middleware,
    );
  }
}
