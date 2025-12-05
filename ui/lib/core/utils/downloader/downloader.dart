export 'downloader_stub.dart'
    if (dart.library.js_interop) 'downloader_web.dart'
    if (dart.library.html) 'downloader_web.dart';
