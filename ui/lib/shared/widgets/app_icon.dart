import 'package:flutter/material.dart';
import 'package:flutter_svg/flutter_svg.dart';
import '../../theme/piccolo_theme.dart';

/// A widget that displays an app icon, supporting both raster images and SVGs.
///
/// Icons are loaded from the proxy URL (e.g., `/api/v1/catalog/nextcloud/icon`),
/// which provides SSRF protection and caching. SVG detection is based on the
/// original icon URL's file extension.
class AppIcon extends StatelessWidget {
  /// The proxy URL to load the icon from (e.g., '/api/v1/catalog/nextcloud/icon').
  final String? proxyUrl;

  /// The original icon URL from the catalog, used for SVG detection.
  /// Check if this ends with '.svg' to render as SVG.
  final String? originalIconUrl;

  /// The size of the icon (width and height).
  final double size;

  /// The border radius for the icon container.
  final double borderRadius;

  /// Text to display as fallback (typically the first letter of the app name).
  final String fallbackText;

  /// Optional icon to display as fallback instead of text.
  final IconData? fallbackIcon;

  /// Background color for the fallback container.
  final Color? fallbackBackgroundColor;

  const AppIcon({
    super.key,
    this.proxyUrl,
    this.originalIconUrl,
    this.size = 48,
    this.borderRadius = 12,
    this.fallbackText = '?',
    this.fallbackIcon,
    this.fallbackBackgroundColor,
  });

  /// Returns true if the original icon URL indicates an SVG file.
  bool get _isSvg {
    if (originalIconUrl == null) return false;
    final uri = Uri.tryParse(originalIconUrl!);
    if (uri == null) return false;
    return uri.path.toLowerCase().endsWith('.svg');
  }

  @override
  Widget build(BuildContext context) {
    // If no proxy URL, show fallback
    if (proxyUrl == null || proxyUrl!.isEmpty) {
      return _buildFallback();
    }

    final padding = size * 0.1;
    final innerSize = size - padding * 2;

    // Build the appropriate image widget based on type
    if (_isSvg) {
      return ClipRRect(
        borderRadius: BorderRadius.circular(borderRadius),
        child: Container(
          width: size,
          height: size,
          color: Colors.white,
          padding: EdgeInsets.all(padding),
          child: SvgPicture.network(
            proxyUrl!,
            width: innerSize,
            height: innerSize,
            fit: BoxFit.contain,
            placeholderBuilder: (context) => _buildLoadingPlaceholder(),
            errorBuilder: (context, error, stackTrace) => _buildFallback(),
          ),
        ),
      );
    }

    // Raster image (PNG, JPG, etc.)
    return ClipRRect(
      borderRadius: BorderRadius.circular(borderRadius),
      child: Container(
        width: size,
        height: size,
        color: Colors.white,
        padding: EdgeInsets.all(padding),
        child: Image.network(
          proxyUrl!,
          width: innerSize,
          height: innerSize,
          fit: BoxFit.contain,
          loadingBuilder: (context, child, loadingProgress) {
            if (loadingProgress == null) return child;
            return _buildLoadingPlaceholder();
          },
          errorBuilder: (context, error, stackTrace) => _buildFallback(),
        ),
      ),
    );
  }

  Widget _buildLoadingPlaceholder() {
    return Container(
      width: size,
      height: size,
      decoration: BoxDecoration(
        color: fallbackBackgroundColor ?? PiccoloTheme.mist,
        borderRadius: BorderRadius.circular(borderRadius),
      ),
      child: Center(
        child: SizedBox(
          width: size * 0.4,
          height: size * 0.4,
          child: const CircularProgressIndicator(
            strokeWidth: 2,
            color: PiccoloTheme.cobalt600,
          ),
        ),
      ),
    );
  }

  Widget _buildFallback() {
    return Container(
      width: size,
      height: size,
      decoration: BoxDecoration(
        color: fallbackBackgroundColor ?? PiccoloTheme.mist,
        borderRadius: BorderRadius.circular(borderRadius),
      ),
      child: Center(
        child: fallbackIcon != null
            ? Icon(
                fallbackIcon,
                size: size * 0.5,
                color: PiccoloTheme.inkMuted,
              )
            : Text(
                fallbackText.isNotEmpty ? fallbackText[0].toUpperCase() : '?',
                style: TextStyle(
                  fontSize: size * 0.5,
                  fontWeight: FontWeight.bold,
                  color: PiccoloTheme.cobalt600,
                ),
              ),
      ),
    );
  }
}
