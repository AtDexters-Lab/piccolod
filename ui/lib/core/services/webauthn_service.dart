import 'dart:convert';
import 'dart:js_interop';

@JS('piccoloWebAuthn.isAvailable')
external JSBoolean _jsIsAvailable();

@JS('piccoloWebAuthn.createCredential')
external JSPromise<JSString> _jsCreateCredential(JSString optionsJson);

@JS('piccoloWebAuthn.getCredential')
external JSPromise<JSString> _jsGetCredential(JSString optionsJson);

@JS('piccoloWebAuthn.signalApiSupported')
external JSBoolean _jsSignalApiSupported();

@JS('piccoloWebAuthn.signalUnknownCredential')
external JSPromise<JSBoolean> _jsSignalUnknownCredential(
  JSString rpId,
  JSString credentialId,
);

class WebAuthnService {
  static bool isAvailable() {
    try {
      return _jsIsAvailable().toDart;
    } on Object catch (_) {
      return false;
    }
  }

  static Future<Map<String, dynamic>> createCredential(
    Map<String, dynamic> options,
  ) async {
    final result = await _jsCreateCredential(jsonEncode(options).toJS).toDart;
    return jsonDecode(result.toDart) as Map<String, dynamic>;
  }

  static Future<Map<String, dynamic>> getCredential(
    Map<String, dynamic> options,
  ) async {
    final result = await _jsGetCredential(jsonEncode(options).toJS).toDart;
    return jsonDecode(result.toDart) as Map<String, dynamic>;
  }

  // signalApiSupported reports whether the WebAuthn Signal API is present.
  // Drives the SnackBar copy variants (E6).
  static bool signalApiSupported() {
    try {
      return _jsSignalApiSupported().toDart;
    } on Object catch (_) {
      return false;
    }
  }

  // signalUnknownCredential prunes a stale entry from the OS picker.
  // credentialIdB64Url is base64url (no padding) — pass server-side IDs
  // through unchanged.
  // Returns true on success, false on unsupported / thrown error.
  static Future<bool> signalUnknownCredential(
    String rpId,
    String credentialIdB64Url,
  ) async {
    try {
      final result = await _jsSignalUnknownCredential(
        rpId.toJS,
        credentialIdB64Url.toJS,
      ).toDart;
      return result.toDart;
    } on Object catch (_) {
      return false;
    }
  }
}
