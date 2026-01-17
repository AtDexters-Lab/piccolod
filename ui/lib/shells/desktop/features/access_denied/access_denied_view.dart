import 'package:flutter/material.dart';
import 'package:web/web.dart' as web;
import '../../../../core/services/api_client.dart';

class AccessDeniedView extends StatefulWidget {
  final String? next;

  const AccessDeniedView({super.key, this.next});

  @override
  State<AccessDeniedView> createState() => _AccessDeniedViewState();
}

class _AccessDeniedViewState extends State<AccessDeniedView> {
  final ApiClient _api = ApiClient();
  bool _validating = false;

  Future<void> _retry() async {
    final next = widget.next;
    if (next == null || next.isEmpty) return;

    setState(() => _validating = true);
    try {
      final resp = await _api.get(
        '/api/v1/auth/validate-next',
        queryParameters: {'next': next},
      );
      if (resp is Map && resp['valid'] == true && resp['redirect_url'] is String) {
        final redirectUrl = resp['redirect_url'] as String;
        if (redirectUrl.isNotEmpty) {
          web.window.location.href = redirectUrl;
          return;
        }
      }
    } catch (_) {
      // Ignore; stay on the page.
    } finally {
      if (mounted) setState(() => _validating = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final next = widget.next;

    return Center(
      child: ConstrainedBox(
        constraints: const BoxConstraints(maxWidth: 520),
        child: Card(
          child: Padding(
            padding: const EdgeInsets.all(24),
            child: Column(
              mainAxisSize: MainAxisSize.min,
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                const Text(
                  'Access denied',
                  style: TextStyle(fontSize: 20, fontWeight: FontWeight.w600),
                ),
                const SizedBox(height: 8),
                const Text(
                  "You don't have permission to access this app. Ask an administrator to grant access.",
                ),
                if (next != null && next.isNotEmpty) ...[
                  const SizedBox(height: 12),
                  Text(
                    'Requested: $next',
                    maxLines: 2,
                    overflow: TextOverflow.ellipsis,
                    style: Theme.of(context).textTheme.bodySmall,
                  ),
                ],
                const SizedBox(height: 16),
                Row(
                  children: [
                    FilledButton(
                      onPressed: () => web.window.location.href = '/',
                      child: const Text('Go to portal'),
                    ),
                    const SizedBox(width: 12),
                    OutlinedButton(
                      onPressed: _validating ? null : _retry,
                      child: Text(_validating ? 'Checking…' : 'Retry'),
                    ),
                  ],
                ),
              ],
            ),
          ),
        ),
      ),
    );
  }
}

