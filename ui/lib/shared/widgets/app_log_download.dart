import 'package:flutter/material.dart';
import 'package:piccolo_os/core/services/api_client.dart';
import 'package:piccolo_os/core/utils/downloader/downloader.dart';
import 'package:piccolo_os/shared/widgets/log_download_chips.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Download an app's persistent logs over a time window, COMBINED across all of
/// the app's services and interleaved by time. Mirrors the
/// diagnostic-log download's date-range UX, but deliberately diverges:
/// - It ignores the Logs tab's per-service selector and always downloads ALL
/// services. The label/helper say so.
/// - It is NOT fire-and-forget: it awaits the request and surfaces the
/// backend's distinct states — locked/unavailable (retryable), empty
/// window, or error — with a busy state on the button.
class AppLogDownload extends StatefulWidget {
  const AppLogDownload({required this.appName, super.key});

  /// App instance name (== instanceID in the URL path).
  final String appName;

  @override
  State<AppLogDownload> createState() => _AppLogDownloadState();
}

class _AppLogDownloadState extends State<AppLogDownload> {
  late DateTime _from;
  late DateTime _to;
  bool _useDateRange = false;
  bool? _activePicker; // null collapsed, true=from, false=to
  bool _busy = false;
  String? _error;
  String? _notice;

  @override
  void initState() {
    super.initState();
    final now = DateTime.now();
    _to = DateTime(now.year, now.month, now.day);
    _from = _to.subtract(const Duration(days: 2));
  }

  void _togglePicker(bool isFrom) {
    setState(() => _activePicker = (_activePicker == isFrom) ? null : isFrom);
  }

  void _onDateSelected(DateTime picked) {
    final isFrom = _activePicker ?? false;
    setState(() {
      // Clear any prior result — it described a different window.
      _error = null;
      _notice = null;
      if (isFrom) {
        _from = picked;
        if (_from.isAfter(_to)) _to = _from;
        if (_to.difference(_from).inDays > 6) {
          _to = _from.add(const Duration(days: 6));
        }
      } else {
        _to = picked;
        if (_to.isBefore(_from)) _from = _to;
        if (_to.difference(_from).inDays > 6) {
          _from = _to.subtract(const Duration(days: 6));
        }
      }
      _activePicker = null;
    });
  }

  String _formatDate(DateTime d) =>
      '${d.year}-${d.month.toString().padLeft(2, '0')}-${d.day.toString().padLeft(2, '0')}';

  DateTime _clampDate(DateTime d, DateTime earliest, DateTime latest) {
    if (d.isBefore(earliest)) return earliest;
    if (d.isAfter(latest)) return latest;
    return d;
  }

  Future<void> _download() async {
    if (_busy) return;
    setState(() {
      _busy = true;
      _error = null;
      _notice = null;
    });
    try {
      final params = <String, String>{};
      if (_useDateRange) {
        params['from'] = _formatDate(_from);
        params['to'] = _formatDate(_to);
      }
      // No `service` param: combined across all services.
      final content = await ApiClient().getText(
        '/api/v1/apps/${widget.appName}/logs/download',
        queryParameters: params.isNotEmpty ? params : null,
      );
      if (!mounted) return; // widget disposed mid-download
      // Distinguish three cases. The backend emits "# [svc] earliest available
      // log: <ts>" header lines so we can tell "no logs at all" from "logs
      // exist but start after the requested window" (rotated out / app started
      // later).
      final lines = content.split('\n');
      final hasLogLines =
          lines.any((l) => l.trim().isNotEmpty && !l.startsWith('#'));
      final headers = lines
          .where((l) => l.startsWith('#'))
          .map((l) => l.replaceFirst(RegExp(r'^#\s*'), ''))
          .toList();
      if (hasLogLines) {
        downloadTextFile(content, 'app-${widget.appName}-logs.txt');
      } else if (headers.isNotEmpty) {
        // No log lines in the window. The backend's header lines explain why —
        // a not-yet-migrated app ("restart or update…"), or logs that exist
        // only outside the window ("earliest available log: …"). Surface them;
        // there's nothing useful to download.
        setState(() => _notice = headers.join('  '));
      } else {
        setState(() => _notice = 'No logs found in this time window.');
      }
    } on ApiException catch (e) {
      if (!mounted) return;
      setState(() {
        if (e.statusCode == 503) {
          _error =
              'Log store is unavailable right now (locked or starting). Try again in a moment.';
        } else if (e.statusCode == 429) {
          _error = 'A log download for this app is already in progress.';
        } else {
          _error = 'Download failed (${e.statusCode}).';
        }
      });
    } on Object catch (e) {
      if (!mounted) return;
      setState(() => _error = 'Download failed: $e');
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final now = DateTime.now();
    final today = DateTime(now.year, now.month, now.day);
    final firstDate = today.subtract(const Duration(days: 90));

    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        Row(
          children: [
            Expanded(
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Text('Combined app logs',
                      style: PiccoloTheme.textTheme.bodyMedium
                          ?.copyWith(fontWeight: FontWeight.bold)),
                  const SizedBox(height: 4),
                  Text(
                    'Downloads all services interleaved by time — even when one is selected for the live view below.',
                    style: PiccoloTheme.textTheme.labelSmall,
                  ),
                ],
              ),
            ),
            OutlinedButton.icon(
              onPressed: _busy ? null : _download,
              icon: _busy
                  ? const SizedBox(
                      width: 16,
                      height: 16,
                      child: CircularProgressIndicator(strokeWidth: 2),
                    )
                  : const Icon(PiccoloIcons.download, size: 18),
              label: Text(_busy ? 'Preparing…' : 'Download all-service logs'),
            ),
          ],
        ),
        if (_error != null) ...[
          const SizedBox(height: 8),
          Text(_error!,
              style: PiccoloTheme.textTheme.labelSmall
                  ?.copyWith(color: PiccoloTheme.critical)),
        ],
        if (_notice != null) ...[
          const SizedBox(height: 8),
          Text(_notice!,
              style: PiccoloTheme.textTheme.labelSmall
                  ?.copyWith(color: PiccoloTheme.inkMuted)),
        ],
        const SizedBox(height: 12),
        Row(
          children: [
            LogModeChip(
              label: 'Recent',
              isActive: !_useDateRange,
              onTap: () => setState(() {
                _useDateRange = false;
                _activePicker = null;
                _error = null;
                _notice = null;
              }),
            ),
            const SizedBox(width: 8),
            LogModeChip(
              label: 'Date Range',
              isActive: _useDateRange,
              onTap: () => setState(() {
                _useDateRange = true;
                _error = null;
                _notice = null;
              }),
            ),
          ],
        ),
        if (_useDateRange) ...[
          const SizedBox(height: 12),
          Row(
            children: [
              LogDateChip(
                label: 'From',
                value: _formatDate(_from),
                isActive: _activePicker ?? false,
                onTap: () => _togglePicker(true),
              ),
              const Padding(
                padding: EdgeInsets.symmetric(horizontal: 8),
                child: Text('—',
                    style: TextStyle(color: PiccoloTheme.inkMuted)),
              ),
              LogDateChip(
                label: 'To',
                value: _formatDate(_to),
                isActive: _activePicker == false,
                onTap: () => _togglePicker(false),
              ),
              const SizedBox(width: 8),
              Text('(max 7 days)', style: PiccoloTheme.textTheme.labelSmall),
            ],
          ),
          AnimatedSize(
            duration: Motion.medium,
            curve: Curves.easeInOut,
            alignment: Alignment.topLeft,
            child: _activePicker != null
                ? Align(
                    alignment: Alignment.centerLeft,
                    child: ConstrainedBox(
                      constraints: const BoxConstraints(maxWidth: 320),
                      child: CalendarDatePicker(
                        key: ValueKey(_activePicker),
                        initialDate: _clampDate(
                          _activePicker ?? false ? _from : _to,
                          firstDate,
                          today,
                        ),
                        firstDate: firstDate,
                        lastDate: today,
                        onDateChanged: _onDateSelected,
                      ),
                    ),
                  )
                : const SizedBox.shrink(),
          ),
        ],
      ],
    );
  }
}
