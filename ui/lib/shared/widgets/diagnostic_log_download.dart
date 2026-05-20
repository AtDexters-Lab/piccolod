import 'package:flutter/material.dart';
import 'package:piccolo_os/core/config/core_config.dart';
import 'package:piccolo_os/core/utils/downloader/downloader.dart';
import 'package:piccolo_os/shared/widgets/log_download_chips.dart';
import 'package:piccolo_os/theme/piccolo_icons.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Self-contained diagnostic log download widget.
///
/// Provides "Current Boot" / "Date Range" mode toggle and optional date range
/// picker. Used by both the emergency setup wizard and the admin system tab.
class DiagnosticLogDownload extends StatefulWidget {
  const DiagnosticLogDownload({
    required this.apiPath,
    this.showTitle = true,
    this.showDescription = true,
    super.key,
  });

  /// API path, e.g. `/api/v1/system/diagnostic-log`.
  final String apiPath;

  /// Whether to show the "Diagnostic Log" heading.
  final bool showTitle;

  /// Whether to show the description text.
  final bool showDescription;

  @override
  State<DiagnosticLogDownload> createState() => _DiagnosticLogDownloadState();
}

class _DiagnosticLogDownloadState extends State<DiagnosticLogDownload> {
  late DateTime _from;
  late DateTime _to;
  bool _useDateRange = false;
  // null = collapsed, true = from picker, false = to picker
  bool? _activePicker;

  @override
  void initState() {
    super.initState();
    final now = DateTime.now();
    _to = DateTime(now.year, now.month, now.day);
    // 2-day difference = 3 inclusive calendar dates
    _from = _to.subtract(const Duration(days: 2));
  }

  void _togglePicker(bool isFrom) {
    setState(() {
      _activePicker = (_activePicker == isFrom) ? null : isFrom;
    });
  }

  void _onDateSelected(DateTime picked) {
    final isFrom = _activePicker ?? false;
    setState(() {
      if (isFrom) {
        _from = picked;
        if (_from.isAfter(_to)) _to = _from;
        // Clamp: max 7 inclusive calendar days = 6-day difference
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

  void _download() {
    final params = <String, String>{};
    if (_useDateRange) {
      params['from'] = _formatDate(_from);
      params['to'] = _formatDate(_to);
    }
    final uri =
        Uri.parse('${CoreConfig.apiBaseUrl}${widget.apiPath}')
            .replace(queryParameters: params.isNotEmpty ? params : null);
    downloadFromUrl(uri.toString(), 'piccolod-diagnostic.log');
  }

  @override
  Widget build(BuildContext context) {
    final now = DateTime.now();
    final today = DateTime(now.year, now.month, now.day);
    final firstDate = today.subtract(const Duration(days: 90));

    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        if (widget.showTitle || widget.showDescription)
          Row(
            children: [
              Expanded(
                child: Column(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                    if (widget.showTitle)
                      Text('Diagnostic Log',
                          style: PiccoloTheme.textTheme.bodyMedium
                              ?.copyWith(fontWeight: FontWeight.bold)),
                    if (widget.showTitle && widget.showDescription)
                      const SizedBox(height: 4),
                    if (widget.showDescription)
                      Text(
                        'Download a redacted system log for bug reporting.',
                        style: PiccoloTheme.textTheme.labelSmall,
                      ),
                  ],
                ),
              ),
              OutlinedButton.icon(
                onPressed: _download,
                icon: const Icon(PiccoloIcons.download, size: 18),
                label: const Text('Download'),
              ),
            ],
          ),
        if (!widget.showTitle && !widget.showDescription)
          OutlinedButton.icon(
            onPressed: _download,
            icon: const Icon(PiccoloIcons.download, size: 16),
            label: const Text('Download Diagnostic Log'),
            style: OutlinedButton.styleFrom(foregroundColor: PiccoloTheme.ink),
          ),
        const SizedBox(height: 12),
        Row(
          children: [
            LogModeChip(
              label: 'Current Boot',
              isActive: !_useDateRange,
              onTap: () => setState(() {
                _useDateRange = false;
                _activePicker = null;
              }),
            ),
            const SizedBox(width: 8),
            LogModeChip(
              label: 'Date Range',
              isActive: _useDateRange,
              onTap: () => setState(() => _useDateRange = true),
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
                child:
                    Text('\u2014', style: TextStyle(color: PiccoloTheme.inkMuted)),
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
