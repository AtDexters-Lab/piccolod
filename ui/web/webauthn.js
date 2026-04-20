// WebAuthn bridge for Flutter Web — exposes navigator.credentials to Dart.
window.piccoloWebAuthn = {
  isAvailable: function() {
    return window.PublicKeyCredential !== undefined && window.isSecureContext === true;
  },

  // signalApiSupported reports whether the WebAuthn Signal API (Chrome 126+,
  // Safari 18+) is present. Frontend uses this to decide between SnackBar
  // copy variants (E6).
  signalApiSupported: function() {
    return typeof window.PublicKeyCredential !== 'undefined' &&
      typeof window.PublicKeyCredential.signalUnknownCredential === 'function';
  },

  // signalUnknownCredential tells the OS picker to forget a credential we no
  // longer recognize. Used after deletes (D-9) and after server-side login
  // failures with a known DB miss (D-14). Idempotent — safe to fire multiple
  // times for the same id (D-13). Returns true on success, false on
  // unsupported / thrown error. Fire-and-forget at call sites.
  signalUnknownCredential: async function(rpId, credentialIdB64Url) {
    if (!this.signalApiSupported()) return false;
    try {
      await window.PublicKeyCredential.signalUnknownCredential({
        rpId: rpId,
        credentialId: credentialIdB64Url,
      });
      return true;
    } catch (e) {
      console.warn('signalUnknownCredential failed', e);
      return false;
    }
  },

  createCredential: async function(optionsJson) {
    // Wrap the entire body so parse / decode failures also surface as a
    // normalized `Name: message` — otherwise a malformed server response
    // would bubble a raw SyntaxError that skips the classifier on the Dart
    // side and falls through to the generic "unexpected error" bucket.
    try {
      var options = JSON.parse(optionsJson);
      options.challenge = _b64ToBuffer(options.challenge);
      options.user.id = _b64ToBuffer(options.user.id);
      if (options.excludeCredentials) {
        options.excludeCredentials = options.excludeCredentials.map(function(c) {
          return Object.assign({}, c, { id: _b64ToBuffer(c.id) });
        });
      }
      var cred = await navigator.credentials.create({ publicKey: options });
      return JSON.stringify({
        id: cred.id,
        rawId: _bufferToB64(cred.rawId),
        type: cred.type,
        response: {
          attestationObject: _bufferToB64(cred.response.attestationObject),
          clientDataJSON: _bufferToB64(cred.response.clientDataJSON)
        }
      });
    } catch (e) {
      throw _normalizeWebAuthnError(e);
    }
  },

  getCredential: async function(optionsJson) {
    try {
      var options = JSON.parse(optionsJson);
      options.challenge = _b64ToBuffer(options.challenge);
      if (options.allowCredentials) {
        options.allowCredentials = options.allowCredentials.map(function(c) {
          return Object.assign({}, c, { id: _b64ToBuffer(c.id) });
        });
      }
      var cred = await navigator.credentials.get({ publicKey: options });
      return JSON.stringify({
        id: cred.id,
        rawId: _bufferToB64(cred.rawId),
        type: cred.type,
        response: {
          authenticatorData: _bufferToB64(cred.response.authenticatorData),
          clientDataJSON: _bufferToB64(cred.response.clientDataJSON),
          signature: _bufferToB64(cred.response.signature),
          userHandle: cred.response.userHandle ? _bufferToB64(cred.response.userHandle) : null
        }
      });
    } catch (e) {
      throw _normalizeWebAuthnError(e);
    }
  }
};

// Normalizes browser WebAuthn errors into a stable "Name: message" string
// that Dart-side `friendlyPasskeyError` can reliably pattern-match against.
// Without this, DOMException / TypeError Dart-side toString() can drop the
// error name, leaving the UI to fall through to a generic "unexpected error".
function _normalizeWebAuthnError(e) {
  var name = (e && e.name) ? e.name : 'Error';
  var msg = (e && e.message) ? e.message : String(e);
  console.warn('WebAuthn error', name, msg, e);
  return new Error(name + ': ' + msg);
}

function _b64ToBuffer(b64url) {
  var b64 = b64url.replace(/-/g, '+').replace(/_/g, '/');
  var pad = b64.length % 4;
  if (pad) b64 += '='.repeat(4 - pad);
  var bin = atob(b64);
  var bytes = new Uint8Array(bin.length);
  for (var i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return bytes.buffer;
}

function _bufferToB64(buf) {
  var bytes = new Uint8Array(buf);
  var bin = '';
  for (var i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}
