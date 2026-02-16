import 'package:flutter/material.dart';

/// Small colored circle optionally paired with a text label.
///
/// Used for health indicators, online/offline dots, and app status badges.
class StatusDot extends StatelessWidget {
  final Color color;
  final double size;
  final String? label;
  final TextStyle? labelStyle;

  const StatusDot({
    super.key,
    required this.color,
    this.size = 8,
    this.label,
    this.labelStyle,
  });

  @override
  Widget build(BuildContext context) {
    final dot = Container(
      width: size,
      height: size,
      decoration: BoxDecoration(
        color: color,
        shape: BoxShape.circle,
      ),
    );

    if (label == null) return dot;

    return Row(
      mainAxisSize: MainAxisSize.min,
      children: [
        dot,
        const SizedBox(width: 6),
        Text(label!, style: labelStyle),
      ],
    );
  }
}
