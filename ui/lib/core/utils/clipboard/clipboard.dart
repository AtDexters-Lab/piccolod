export 'clipboard_stub.dart'
    if (dart.library.js_interop) 'clipboard_web.dart'
    if (dart.library.io) 'clipboard_io.dart';
