import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:piccolo_os/core/models/app_models.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

class EditListenersDialog extends StatefulWidget {
  const EditListenersDialog({
    required this.initialListeners,
    super.key,
    this.errorMessage,
  });
  final List<AppListener> initialListeners;
  final String? errorMessage;

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
      _listeners.add(
        AppListener(
          name: 'service-${_listeners.length + 1}',
          guestPort: 8080,
        ),
      );
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
            if (widget.errorMessage != null &&
                widget.errorMessage!.isNotEmpty) ...[
              Container(
                padding: const EdgeInsets.all(Spacing.md),
                decoration: BoxDecoration(
                  color: PiccoloTheme.critical.withValues(alpha: 0.08),
                  borderRadius: BorderRadius.circular(Radii.sm),
                  border: Border.all(
                    color: PiccoloTheme.critical.withValues(alpha: 0.2),
                  ),
                ),
                child: Row(
                  children: [
                    const Icon(
                      PiccoloIcons.error,
                      size: 16,
                      color: PiccoloTheme.critical,
                    ),
                    const SizedBox(width: Spacing.sm),
                    Expanded(
                      child: Text(
                        widget.errorMessage!,
                        style: const TextStyle(
                          color: PiccoloTheme.critical,
                          fontSize: 13,
                        ),
                      ),
                    ),
                  ],
                ),
              ),
              const SizedBox(height: Spacing.base),
            ],
            Container(
              padding: const EdgeInsets.all(Spacing.md),
              decoration: BoxDecoration(
                color: PiccoloTheme.mist,
                borderRadius: BorderRadius.circular(Radii.sm),
              ),
              child: const Row(
                children: [
                  Icon(
                    PiccoloIcons.info,
                    size: 16,
                    color: PiccoloTheme.cobalt600,
                  ),
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
                      key: ValueKey(
                        '$index-${l.protocol}-${l.flow}-${classifyAccess(l.auth)}',
                      ),
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
              Navigator.of(context).pop(List<AppListener>.from(_listeners));
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
    super.key,
  });
  final AppListener listener;
  final VoidCallback onDelete;
  final ValueChanged<AppListener> onChange;

  bool get _showAccessDropdown =>
      (listener.protocol == 'http' || listener.protocol == 'websocket') &&
      listener.flow == 'tcp';

  bool get _showPortClaim => listener.flow == 'tcp' || listener.flow == 'udp';

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
              isExpanded: true,
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
                if (val != null) {
                  onChange(
                    _copyWith(
                      protocol: val,
                      clearAuth: val == 'raw',
                    ),
                  );
                }
              },
            ),
          ),
          const SizedBox(width: Spacing.md),
          Expanded(
            flex: 2,
            child: DropdownButtonFormField<String>(
              isExpanded: true,
              initialValue: listener.flow,
              decoration: const InputDecoration(
                labelText: 'Flow',
                isDense: true,
                border: OutlineInputBorder(),
              ),
              items: const [
                DropdownMenuItem(value: 'tcp', child: Text('TCP')),
                DropdownMenuItem(value: 'tls', child: Text('TLS')),
                DropdownMenuItem(value: 'udp', child: Text('UDP')),
              ],
              onChanged: (val) {
                if (val != null) {
                  onChange(
                    _copyWith(
                      flow: val,
                      clearAuth: val != 'tcp',
                      clearPortClaim: val == 'tls',
                      protocol: val == 'udp' ? 'raw' : null,
                    ),
                  );
                }
              },
            ),
          ),
          if (_showAccessDropdown) ...[
            const SizedBox(width: Spacing.md),
            Expanded(
              flex: 2,
              child: _buildAccessDropdown(),
            ),
          ],
          if (_showPortClaim) ...[
            const SizedBox(width: Spacing.md),
            SizedBox(
              width: 80,
              child: TextFormField(
                initialValue: listener.portClaim?.toString() ?? '',
                decoration: const InputDecoration(
                  labelText: 'Claim',
                  hintText: 'Port',
                  isDense: true,
                  border: OutlineInputBorder(),
                ),
                keyboardType: TextInputType.number,
                inputFormatters: [FilteringTextInputFormatter.digitsOnly],
                onChanged: (val) {
                  final port = val.isEmpty ? null : int.tryParse(val);
                  onChange(
                    _copyWith(portClaim: port, clearPortClaim: val.isEmpty),
                  );
                },
                validator: (val) {
                  if (val == null || val.isEmpty) return null; // optional
                  final p = int.tryParse(val);
                  return p == null || p <= 0 || p > 65535 ? 'Invalid' : null;
                },
              ),
            ),
          ],
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

  Widget _buildAccessDropdown() {
    final access = classifyAccess(listener.auth);
    final isCustom = access == 'custom';

    final dropdown = DropdownButtonFormField<String>(
      isExpanded: true,
      initialValue: access,
      decoration: const InputDecoration(
        labelText: 'Access',
        isDense: true,
        border: OutlineInputBorder(),
      ),
      items: [
        const DropdownMenuItem(value: 'private', child: Text('Private')),
        const DropdownMenuItem(value: 'public', child: Text('Public')),
        if (isCustom)
          const DropdownMenuItem(value: 'custom', child: Text('Custom')),
      ],
      onChanged: isCustom
          ? null
          : (val) {
              if (val == 'public') {
                onChange(_copyWith(auth: publicAuthPayload()));
              } else if (val == 'private') {
                onChange(_copyWith(clearAuth: true));
              }
            },
    );

    if (isCustom) {
      return Tooltip(
        message:
            'This listener has custom path-based auth rules configured via the app definition.',
        child: dropdown,
      );
    }
    return dropdown;
  }

  AppListener _copyWith({
    String? name,
    int? guestPort,
    String? flow,
    String? protocol,
    Map<String, dynamic>? auth,
    bool clearAuth = false,
    int? portClaim,
    bool clearPortClaim = false,
  }) {
    return AppListener(
      name: name ?? listener.name,
      guestPort: guestPort ?? listener.guestPort,
      flow: flow ?? listener.flow,
      protocol: protocol ?? listener.protocol,
      remotePorts: listener.remotePorts,
      middleware: listener.middleware,
      auth: clearAuth ? null : (auth ?? listener.auth),
      portClaim: clearPortClaim ? null : (portClaim ?? listener.portClaim),
    );
  }
}
