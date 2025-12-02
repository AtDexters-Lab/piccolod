import 'package:flutter/material.dart';

/// Piccolo OS "Cobalt Neutral" Design System.
/// 
/// Mapped from `ui-next/docs/theme-brief.md`.
class PiccoloTheme {
  // -- Neutrals --
  static const Color mist = Color(0xFFF4F6FB); // --sys-surface
  static const Color porcelain = Color(0xFFFFFFFF); // --sys-surface-variant
  static const Color ink = Color(0xFF141821); // --sys-ink
  static const Color inkMuted = Color(0xFF6B7380); // --sys-ink-muted

  // -- Accent (Cobalt) Family --
  static const Color cobalt700 = Color(0xFF254BDD); // Pressed
  static const Color cobalt600 = Color(0xFF2F5AF3); // Base / --sys-accent
  static const Color cobalt500 = Color(0xFF3D66FF); // Hover Top
  static const Color cobalt400 = Color(0xFF5F80FF); // Hover Bottom
  
  // -- Semantic --
  static const Color success = Color(0xFF10B981);
  static const Color warning = Color(0xFFF59E0B);
  static const Color critical = Color(0xFFEF4444);
  static const Color info = Color(0xFF3B82F6);

  // -- Text Styles --
  // Note: 'Comfortaa' and 'Inter' need to be added to pubspec.yaml later.
  // For now we use standard system fonts that mimic the weights.
  
  static TextTheme get textTheme => const TextTheme(
    displayLarge: TextStyle(
      fontFamily: 'Comfortaa',
      fontSize: 32,
      fontWeight: FontWeight.w400, // Regular
      color: ink,
    ),
    bodyLarge: TextStyle(
      fontFamily: 'Inter',
      fontSize: 16,
      height: 1.5, // 24px / 16px
      color: ink,
    ),
    bodyMedium: TextStyle(
      fontFamily: 'Inter',
      fontSize: 14,
      color: ink,
    ),
    labelSmall: TextStyle(
      fontFamily: 'Inter',
      fontSize: 12,
      letterSpacing: 0.2,
      color: inkMuted,
    ),
  );

  static ThemeData get lightTheme {
    return ThemeData(
      useMaterial3: true,
      brightness: Brightness.light,
      scaffoldBackgroundColor: mist,
      colorScheme: const ColorScheme(
        brightness: Brightness.light,
        primary: cobalt600,
        onPrimary: Colors.white,
        secondary: cobalt500,
        onSecondary: Colors.white,
        error: critical,
        onError: Colors.white,
        surface: mist,
        onSurface: ink,
        surfaceContainerHighest: porcelain, 
      ),
      textTheme: textTheme,
      // Component Themes
      appBarTheme: const AppBarTheme(
        backgroundColor: mist, // Transparent/Mist
        elevation: 0,
        foregroundColor: ink,
      ),
    );
  }
  
  // Dark theme can be added here later mapping to the brief's dark values.
}
