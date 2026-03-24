import 'dart:convert';
import 'dart:js_interop';

@JS('piccoloWebAuthn.isAvailable')
external JSBoolean _jsIsAvailable();

@JS('piccoloWebAuthn.createCredential')
external JSPromise<JSString> _jsCreateCredential(JSString optionsJson);

@JS('piccoloWebAuthn.getCredential')
external JSPromise<JSString> _jsGetCredential(JSString optionsJson);

class WebAuthnService {
  static bool isAvailable() {
    try {
      return _jsIsAvailable().toDart;
    } on Object catch (_) {
      return false;
    }
  }

  static Future<Map<String, dynamic>> createCredential(
      Map<String, dynamic> options) async {
    final result =
        await _jsCreateCredential(jsonEncode(options).toJS).toDart;
    return jsonDecode(result.toDart) as Map<String, dynamic>;
  }

  static Future<Map<String, dynamic>> getCredential(
      Map<String, dynamic> options) async {
    final result =
        await _jsGetCredential(jsonEncode(options).toJS).toDart;
    return jsonDecode(result.toDart) as Map<String, dynamic>;
  }
}
