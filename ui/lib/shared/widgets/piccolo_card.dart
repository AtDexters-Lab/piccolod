import 'package:flutter/material.dart';
import 'package:piccolo_os/theme/piccolo_theme.dart';

/// Unified content card used throughout the Piccolo UI.
///
/// Replaces the 4+ different Container-based card patterns with a single
/// consistent surface: porcelain background, hairline border, configurable
/// border radius and padding.
class PiccoloCard extends StatelessWidget {

  const PiccoloCard({
    required this.child, super.key,
    this.padding,
    this.boxShadow,
    this.borderRadius,
  });
  final Widget child;
  final EdgeInsetsGeometry? padding;
  final List<BoxShadow>? boxShadow;
  final double? borderRadius;

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: padding ?? const EdgeInsets.all(Spacing.lg),
      decoration: BoxDecoration(
        color: PiccoloTheme.porcelain,
        borderRadius: BorderRadius.circular(borderRadius ?? Radii.md),
        border: Border.all(color: PiccoloTheme.hairline),
        boxShadow: boxShadow ?? Elevation.elev0,
      ),
      child: child,
    );
  }
}
