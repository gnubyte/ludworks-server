/**
 * Error type shared by every vault library module.
 *
 * `code` is a stable machine-readable string; UIs should branch on it, never on `message`.
 *   invalid_argument  caller passed something unusable (wrong type/length/range)
 *   bad_password      vault unlock failed (wrong master password)
 *   decrypt_failed    AEAD/MAC verification failed for a sealed value
 *   unsupported_version  sealed string has an unknown version prefix (not "v1")
 *   malformed         a sealed string / encoded value could not be parsed
 *   password_required import file is password protected and no password was given
 *   unsupported       format feature or version this library does not implement
 *   bad_credentials   KDBX: wrong password and/or key file
 *   bad_keyfile       KDBX: key file is structurally invalid (e.g. v2 hash mismatch)
 *   corrupt           file is damaged / truncated / fails integrity checks
 *   limit_exceeded    input exceeds a safety limit (size, depth, KDF cost)
 *   xml_invalid       XML is not well-formed or uses forbidden constructs
 */
export class VaultError extends Error {
  /**
   * @param {string} code
   * @param {string} [message]
   * @param {{cause?: unknown}} [options]
   */
  constructor(code, message, options) {
    super(message || code, options);
    this.name = 'VaultError';
    this.code = code;
  }
}

/** @param {string} msg */
export const invalidArg = (msg) => new VaultError('invalid_argument', msg);
