// WebAuthn bridge for Flutter Web — exposes navigator.credentials to Dart.
window.piccoloWebAuthn = {
  isAvailable: function() {
    return window.PublicKeyCredential !== undefined && window.isSecureContext === true;
  },

  createCredential: async function(optionsJson) {
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
  },

  getCredential: async function(optionsJson) {
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
  }
};

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
