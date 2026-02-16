import 'package:flutter/material.dart';

/// Piccolo OS "Cobalt Neutral" Design System.
///
/// Mapped from `ui/docs/theme-brief.md`.

// ---------------------------------------------------------------------------
// Spacing scale
// ---------------------------------------------------------------------------
class Spacing {
  Spacing._();
  static const double xs = 4;
  static const double sm = 8;
  static const double md = 12;
  static const double base = 16;
  static const double lg = 24;
  static const double xl = 32;
  static const double xxl = 40;
}

// ---------------------------------------------------------------------------
// Radius scale
// ---------------------------------------------------------------------------
class Radii {
  Radii._();
  static const double xxs = 4;
  static const double xs = 6;
  static const double sm = 10;
  static const double md = 14;
  static const double lg = 20;
  static const double xl = 28;
  static const double pill = 999;
}

// ---------------------------------------------------------------------------
// Elevation / shadow presets
// ---------------------------------------------------------------------------
class Elevation {
  Elevation._();

  static const List<BoxShadow> elev0 = [];

  static const List<BoxShadow> elev1 = [
    BoxShadow(
      color: Color(0x0F000000), // rgba(0,0,0,.06)
      blurRadius: 2,
      offset: Offset(0, 1),
    ),
  ];

  static const List<BoxShadow> elev2 = [
    BoxShadow(
      color: Color(0x140E1322), // rgba(14,19,34,.08)
      blurRadius: 8,
      offset: Offset(0, 2),
    ),
    BoxShadow(
      color: Color(0x0F0E1322), // rgba(14,19,34,.06)
      blurRadius: 2,
      offset: Offset(0, 1),
    ),
  ];

  static const List<BoxShadow> elev3 = [
    BoxShadow(
      color: Color(0x1F0E1322), // rgba(14,19,34,.12)
      blurRadius: 20,
      offset: Offset(0, 8),
    ),
    BoxShadow(
      color: Color(0x140E1322), // rgba(14,19,34,.08)
      blurRadius: 6,
      offset: Offset(0, 2),
    ),
  ];

  static const List<BoxShadow> elev4 = [
    BoxShadow(
      color: Color(0x290E1322), // rgba(14,19,34,.16)
      blurRadius: 40,
      offset: Offset(0, 20),
    ),
    BoxShadow(
      color: Color(0x140E1322), // rgba(14,19,34,.08)
      blurRadius: 8,
      offset: Offset(0, 4),
    ),
  ];
}

// ---------------------------------------------------------------------------
// Motion constants
// ---------------------------------------------------------------------------
class Motion {
  Motion._();
  static const Duration fast = Duration(milliseconds: 120);
  static const Duration medium = Duration(milliseconds: 180);
  static const Duration slow = Duration(milliseconds: 240);
  static const Curve standard = Cubic(0.2, 0, 0, 1);
  static const Curve emphasized = Cubic(0.16, 1, 0.3, 1);
}

// ---------------------------------------------------------------------------
// Color tokens & theme data
// ---------------------------------------------------------------------------
class PiccoloTheme {
  PiccoloTheme._();

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
  static const Color cobalt300 = Color(0xFF7EA2FF); // Tints

  // -- Semantic --
  static const Color success = Color(0xFF10B981);
  static const Color warning = Color(0xFFF59E0B);
  static const Color critical = Color(0xFFEF4444);
  static const Color info = Color(0xFF3B82F6);

  // -- Surface modifiers --
  static const Color outline = Color(0x24141821); // rgba(20,24,33,.14)
  static const Color hairline = Color(0x14141821); // rgba(20,24,33,.08)
  static const Color overlay = Color(0x0F141821); // rgba(20,24,33,.06)
  static const Color scrim = Color(0x66000000); // rgba(0,0,0,.40)
  static const Color disabledBg = Color(0x0F141821); // rgba(20,24,33,.06)
  static const Color disabledFg = Color(0x61141821); // rgba(20,24,33,.38)

  // -- Special --
  static const Color terminalBg = Color(0xFF1E1E1E);

  // -- Monospace text helper --
  static TextStyle get mono => const TextStyle(
    fontFamily: 'JetBrainsMono',
    fontSize: 12,
    color: ink,
  );

  // -- Text Theme --
  static TextTheme get textTheme => const TextTheme(
    // Comfortaa display / headline slots
    displayLarge: TextStyle(
      fontFamily: 'Comfortaa',
      fontSize: 32,
      fontWeight: FontWeight.w400,
      color: ink,
    ),
    headlineLarge: TextStyle(
      fontFamily: 'Comfortaa',
      fontSize: 28,
      fontWeight: FontWeight.w400,
      height: 1.3,
      color: ink,
    ),
    headlineMedium: TextStyle(
      fontFamily: 'Comfortaa',
      fontSize: 24,
      fontWeight: FontWeight.w400,
      height: 1.3,
      color: ink,
    ),
    headlineSmall: TextStyle(
      fontFamily: 'Comfortaa',
      fontSize: 20,
      fontWeight: FontWeight.w400,
      height: 1.3,
      color: ink,
    ),

    // Inter title / body / label slots
    titleMedium: TextStyle(
      fontFamily: 'Inter',
      fontSize: 16,
      fontWeight: FontWeight.w600,
      color: ink,
    ),
    titleSmall: TextStyle(
      fontFamily: 'Inter',
      fontSize: 14,
      fontWeight: FontWeight.w600,
      color: ink,
    ),
    bodyLarge: TextStyle(
      fontFamily: 'Inter',
      fontSize: 16,
      height: 1.5,
      color: ink,
    ),
    bodyMedium: TextStyle(
      fontFamily: 'Inter',
      fontSize: 14,
      color: ink,
    ),
    bodySmall: TextStyle(
      fontFamily: 'Inter',
      fontSize: 13,
      color: inkMuted,
    ),
    labelLarge: TextStyle(
      fontFamily: 'Inter',
      fontSize: 14,
      fontWeight: FontWeight.w500,
      color: ink,
    ),
    labelMedium: TextStyle(
      fontFamily: 'Inter',
      fontSize: 12,
      fontWeight: FontWeight.w500,
      color: ink,
    ),
    labelSmall: TextStyle(
      fontFamily: 'Inter',
      fontSize: 12,
      letterSpacing: 0.2,
      color: inkMuted,
    ),
  );

  // ---------------------------------------------------------------------------
  // Light Theme
  // ---------------------------------------------------------------------------
  static ThemeData get lightTheme {
    final tt = textTheme;

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
        outline: outline,
      ),

      textTheme: tt,

      // ---- Component Themes ----

      appBarTheme: const AppBarTheme(
        backgroundColor: mist,
        elevation: 0,
        foregroundColor: ink,
      ),

      filledButtonTheme: FilledButtonThemeData(
        style: FilledButton.styleFrom(
          backgroundColor: cobalt600,
          foregroundColor: Colors.white,
          disabledBackgroundColor: disabledBg,
          disabledForegroundColor: disabledFg,
          padding: const EdgeInsets.symmetric(
            horizontal: Spacing.base,
            vertical: Spacing.md,
          ),
          shape: RoundedRectangleBorder(
            borderRadius: BorderRadius.circular(Radii.md),
          ),
          textStyle: tt.labelLarge,
        ),
      ),

      elevatedButtonTheme: ElevatedButtonThemeData(
        style: ElevatedButton.styleFrom(
          backgroundColor: cobalt600,
          foregroundColor: Colors.white,
          disabledBackgroundColor: disabledBg,
          disabledForegroundColor: disabledFg,
          elevation: 0,
          padding: const EdgeInsets.symmetric(
            horizontal: Spacing.base,
            vertical: Spacing.md,
          ),
          shape: RoundedRectangleBorder(
            borderRadius: BorderRadius.circular(Radii.md),
          ),
          textStyle: tt.labelLarge,
        ),
      ),

      outlinedButtonTheme: OutlinedButtonThemeData(
        style: OutlinedButton.styleFrom(
          foregroundColor: ink,
          padding: const EdgeInsets.symmetric(
            horizontal: Spacing.base,
            vertical: Spacing.md,
          ),
          side: const BorderSide(color: outline),
          shape: RoundedRectangleBorder(
            borderRadius: BorderRadius.circular(Radii.md),
          ),
          textStyle: tt.labelLarge,
        ),
      ),

      textButtonTheme: TextButtonThemeData(
        style: TextButton.styleFrom(
          foregroundColor: cobalt600,
          textStyle: tt.labelLarge,
        ),
      ),

      inputDecorationTheme: InputDecorationTheme(
        filled: true,
        fillColor: porcelain,
        border: OutlineInputBorder(
          borderRadius: BorderRadius.circular(Radii.sm),
          borderSide: const BorderSide(color: outline),
        ),
        enabledBorder: OutlineInputBorder(
          borderRadius: BorderRadius.circular(Radii.sm),
          borderSide: const BorderSide(color: outline),
        ),
        focusedBorder: OutlineInputBorder(
          borderRadius: BorderRadius.circular(Radii.sm),
          borderSide: const BorderSide(color: cobalt600, width: 2),
        ),
        errorBorder: OutlineInputBorder(
          borderRadius: BorderRadius.circular(Radii.sm),
          borderSide: const BorderSide(color: critical),
        ),
        focusedErrorBorder: OutlineInputBorder(
          borderRadius: BorderRadius.circular(Radii.sm),
          borderSide: const BorderSide(color: critical, width: 2),
        ),
        contentPadding: const EdgeInsets.symmetric(
          horizontal: Spacing.base,
          vertical: Spacing.md,
        ),
      ),

      cardTheme: CardThemeData(
        elevation: 0,
        color: porcelain,
        shape: RoundedRectangleBorder(
          borderRadius: BorderRadius.circular(Radii.md),
          side: const BorderSide(color: hairline),
        ),
        margin: EdgeInsets.zero,
      ),

      dialogTheme: DialogThemeData(
        shape: RoundedRectangleBorder(
          borderRadius: BorderRadius.circular(Radii.lg),
        ),
        surfaceTintColor: Colors.transparent,
      ),

      popupMenuTheme: PopupMenuThemeData(
        shape: RoundedRectangleBorder(
          borderRadius: BorderRadius.circular(Radii.sm),
        ),
        elevation: 2,
        color: porcelain,
      ),

      tabBarTheme: TabBarThemeData(
        labelColor: cobalt600,
        unselectedLabelColor: inkMuted,
        indicatorColor: cobalt600,
      ),

      snackBarTheme: SnackBarThemeData(
        behavior: SnackBarBehavior.floating,
        shape: RoundedRectangleBorder(
          borderRadius: BorderRadius.circular(Radii.sm),
        ),
      ),

      dividerTheme: const DividerThemeData(
        color: hairline,
        thickness: 1,
      ),

      tooltipTheme: TooltipThemeData(
        decoration: BoxDecoration(
          color: ink,
          borderRadius: BorderRadius.circular(Radii.xs),
        ),
        textStyle: const TextStyle(
          fontFamily: 'Inter',
          fontSize: 12,
          color: Colors.white,
        ),
      ),

      listTileTheme: const ListTileThemeData(
        contentPadding: EdgeInsets.symmetric(horizontal: Spacing.lg),
      ),
    );
  }
}
