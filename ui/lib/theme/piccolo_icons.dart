import 'package:flutter/widgets.dart';

// Phosphor Regular icons, self-contained. Codepoints inlined from
// phosphor-icons/core; font shipped at assets/fonts/Phosphor/PhosphorRegular.ttf
// (registered as family PhosphorRegular in pubspec.yaml). Avoids the
// phosphor_flutter package, which extends Flutter's now-final IconData class.
//
// Each IconData is declared inline rather than via a helper to keep them
// `const`: consumers stay `const Icon(...)` and Flutter's icon-font
// tree-shaker can drop unused glyphs at build time (488 KB → ~22 KB).
const String _family = 'PhosphorRegular';

class PiccoloIcons {
  PiccoloIcons._();

  // ---- Navigation ----
  static const IconData home = IconData(0xe2c2, fontFamily: _family, matchTextDirection: true);
  static const IconData settings = IconData(0xe270, fontFamily: _family, matchTextDirection: true);
  static const IconData settingsApp = IconData(0xe272, fontFamily: _family, matchTextDirection: true);
  static const IconData terminal = IconData(0xeae8, fontFamily: _family, matchTextDirection: true);
  static const IconData store = IconData(0xe470, fontFamily: _family, matchTextDirection: true);
  static const IconData arrowBack = IconData(0xe058, fontFamily: _family, matchTextDirection: true);
  static const IconData arrowForward = IconData(0xe06c, fontFamily: _family, matchTextDirection: true);
  static const IconData arrowUp = IconData(0xe08e, fontFamily: _family, matchTextDirection: true);
  static const IconData arrowForwardIos = IconData(0xe13a, fontFamily: _family, matchTextDirection: true);
  static const IconData caretDown = IconData(0xe136, fontFamily: _family, matchTextDirection: true);
  static const IconData caretUp = IconData(0xe13c, fontFamily: _family, matchTextDirection: true);

  // ---- Status ----
  static const IconData success = IconData(0xe184, fontFamily: _family, matchTextDirection: true);
  static const IconData warning = IconData(0xe4e0, fontFamily: _family, matchTextDirection: true);
  static const IconData error = IconData(0xe4e2, fontFamily: _family, matchTextDirection: true);
  static const IconData info = IconData(0xe2ce, fontFamily: _family, matchTextDirection: true);
  static const IconData wifiOff = IconData(0xe4f2, fontFamily: _family, matchTextDirection: true);

  // ---- Network ----
  static const IconData wifi = IconData(0xe4ea, fontFamily: _family, matchTextDirection: true);
  static const IconData wifiMedium = IconData(0xe4ee, fontFamily: _family, matchTextDirection: true);
  static const IconData wifiLow = IconData(0xe4ec, fontFamily: _family, matchTextDirection: true);
  static const IconData wifiNone = IconData(0xe4f0, fontFamily: _family, matchTextDirection: true);
  static const IconData ethernet = IconData(0xeb5a, fontFamily: _family, matchTextDirection: true);
  static const IconData accessPoint = IconData(0xe0f2, fontFamily: _family, matchTextDirection: true);

  // ---- Actions ----
  static const IconData close = IconData(0xe4f6, fontFamily: _family, matchTextDirection: true);
  static const IconData minimize = IconData(0xe32a, fontFamily: _family, matchTextDirection: true);
  static const IconData maximize = IconData(0xe1d0, fontFamily: _family, matchTextDirection: true);
  static const IconData restore = IconData(0xe1ce, fontFamily: _family, matchTextDirection: true);
  static const IconData check = IconData(0xe182, fontFamily: _family, matchTextDirection: true);
  static const IconData add = IconData(0xe3d4, fontFamily: _family, matchTextDirection: true);
  static const IconData addBox = IconData(0xed4a, fontFamily: _family, matchTextDirection: true);
  static const IconData delete = IconData(0xe4a6, fontFamily: _family, matchTextDirection: true);
  static const IconData edit = IconData(0xe3b4, fontFamily: _family, matchTextDirection: true);
  static const IconData search = IconData(0xe30c, fontFamily: _family, matchTextDirection: true);
  static const IconData refresh = IconData(0xe036, fontFamily: _family, matchTextDirection: true);
  static const IconData download = IconData(0xe20c, fontFamily: _family, matchTextDirection: true);
  static const IconData openExternal = IconData(0xe5de, fontFamily: _family, matchTextDirection: true);
  static const IconData copy = IconData(0xe1ca, fontFamily: _family, matchTextDirection: true);
  static const IconData play = IconData(0xe3d0, fontFamily: _family, matchTextDirection: true);
  static const IconData stop = IconData(0xe46c, fontFamily: _family, matchTextDirection: true);
  static const IconData moreVert = IconData(0xe208, fontFamily: _family, matchTextDirection: true);

  // ---- Visibility ----
  static const IconData visibility = IconData(0xe220, fontFamily: _family, matchTextDirection: true);
  static const IconData visibilityOff = IconData(0xe224, fontFamily: _family, matchTextDirection: true);

  // ---- Auth ----
  static const IconData fingerprint = IconData(0xe240, fontFamily: _family, matchTextDirection: true);

  // ---- Objects ----
  static const IconData lock = IconData(0xe2fa, fontFamily: _family, matchTextDirection: true);
  static const IconData lockKey = IconData(0xe2fe, fontFamily: _family, matchTextDirection: true);
  static const IconData clock = IconData(0xe19a, fontFamily: _family, matchTextDirection: true);
  static const IconData shield = IconData(0xe40a, fontFamily: _family, matchTextDirection: true);
  static const IconData shieldCheck = IconData(0xe40c, fontFamily: _family, matchTextDirection: true);
  static const IconData security = shieldCheck;
  static const IconData person = IconData(0xe4c2, fontFamily: _family, matchTextDirection: true);
  static const IconData people = IconData(0xe4d6, fontFamily: _family, matchTextDirection: true);
  static const IconData userGear = IconData(0xe4cc, fontFamily: _family, matchTextDirection: true);
  static const IconData email = IconData(0xe214, fontFamily: _family, matchTextDirection: true);
  static const IconData cloud = IconData(0xe1aa, fontFamily: _family, matchTextDirection: true);
  static const IconData planet = IconData(0xe652, fontFamily: _family, matchTextDirection: true);
  static const IconData cloudOff = IconData(0xe1b6, fontFamily: _family, matchTextDirection: true);
  static const IconData devices = IconData(0xeba4, fontFamily: _family, matchTextDirection: true);
  static const IconData link = IconData(0xe2e2, fontFamily: _family, matchTextDirection: true);
  static const IconData router = IconData(0xe288, fontFamily: _family, matchTextDirection: true);
  static const IconData storage = IconData(0xe29e, fontFamily: _family, matchTextDirection: true);
  static const IconData hardDrives = IconData(0xe2a0, fontFamily: _family, matchTextDirection: true);
  static const IconData folder = IconData(0xe24a, fontFamily: _family, matchTextDirection: true);
  static const IconData folderOpen = IconData(0xe256, fontFamily: _family, matchTextDirection: true);
  static const IconData file = IconData(0xe230, fontFamily: _family, matchTextDirection: true);
  static const IconData fileText = IconData(0xe23a, fontFamily: _family, matchTextDirection: true);
  static const IconData article = IconData(0xe0a8, fontFamily: _family, matchTextDirection: true);
  static const IconData webAsset = IconData(0xe0f6, fontFamily: _family, matchTextDirection: true);
  static const IconData apps = IconData(0xe464, fontFamily: _family, matchTextDirection: true);
  static const IconData usb = IconData(0xe956, fontFamily: _family, matchTextDirection: true);
  static const IconData saveToDisk = IconData(0xe248, fontFamily: _family, matchTextDirection: true);
  static const IconData lightning = IconData(0xe2de, fontFamily: _family, matchTextDirection: true);
  static const IconData hourglass = IconData(0xe2b2, fontFamily: _family, matchTextDirection: true);
  static const IconData gauge = IconData(0xe628, fontFamily: _family, matchTextDirection: true);
  static const IconData calendar = IconData(0xe108, fontFamily: _family, matchTextDirection: true);
  static const IconData circuitry = IconData(0xe9c2, fontFamily: _family, matchTextDirection: true);
  static const IconData star = IconData(0xe46a, fontFamily: _family, matchTextDirection: true);
  static const IconData verified = IconData(0xe606, fontFamily: _family, matchTextDirection: true);
  static const IconData handWaving = IconData(0xe580, fontFamily: _family, matchTextDirection: true);

  // ---- System ----
  static const IconData logout = IconData(0xe42a, fontFamily: _family, matchTextDirection: true);
  static const IconData restart = IconData(0xe038, fontFamily: _family, matchTextDirection: true);
  static const IconData power = IconData(0xe3da, fontFamily: _family, matchTextDirection: true);
  static const IconData sync = IconData(0xe094, fontFamily: _family, matchTextDirection: true);
  static const IconData systemUpdate = IconData(0xe05c, fontFamily: _family, matchTextDirection: true);
  static const IconData expandLess = caretUp;
  static const IconData expandMore = caretDown;
}
