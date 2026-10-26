# Passwords

The **Passwords** app is an end-to-end-encrypted password manager. Your items are encrypted and decrypted only
on your own devices; the server stores ciphertext it cannot read. See the [security model](../security.md#zero-knowledge-password-vault)
for what that means and [Vault cryptography](../api/vault-crypto.md) for the details.

## Setting up your vault

The first time you open Passwords you choose a **master password**. It is different from your account
password, and it never leaves your device:

* Make it long and memorable — a passphrase of five or more random words is ideal.
* **Nobody can recover it.** Not the admin, not Ludworks. If it is forgotten, the only option is
  **Reset vault**, which permanently deletes all items (it asks for your *account* password to confirm).
* Export an encrypted KeePass backup now and then (see below) and keep it somewhere safe.

Your browser derives a key from the master password (PBKDF2-SHA256, 600 000 iterations by default), generates a
random vault key, and uploads only the vault key wrapped (encrypted) with the derived key.

## Daily use

* **Unlock** with the master password; the vault locks again after a period of inactivity or when you log out.
  Signing out in one tab or window signs out (and locks) every other tab of the same browser at once.
* **Item types**: logins (username, password, website URIs, TOTP), secure notes, payment cards and identities,
  each with custom fields (optionally hidden) and a password history.
* **Folders** and **favorites** to organise; instant search over all decrypted items (search runs locally).
* **Password generator** — random passwords (8–128 characters, choosing character sets and excluding ambiguous
  characters) or word passphrases.
* **TOTP codes** — store a 2FA secret or `otpauth://` URI with a login and the app shows the current
  6-digit code, ready to copy.
* **Change master password** re-wraps the vault key; your items are not re-encrypted, so it is instant. It keeps
  your key-derivation work factor (it never lowers an iteration count you chose on another device).
* **Rotate vault key** (Vault settings, or *Also replace the vault key* when changing the master password)
  generates a new vault key and re-encrypts every item and folder with it. Do this if your master password, a
  device or a server backup may have been compromised: a password change alone does not lock out someone who
  already has an old copy of the encrypted vault key and the old password. Other devices have to unlock again.
* If the vault is reset or its key rotated on another device, an open Passwords tab locks itself instead of
  saving anything under the old key.

Website matching for autofill uses the item's URIs: by default the same registrable domain, computed with the
[Public Suffix List](https://publicsuffix.org/) (`login.example.com` matches `example.com`; `www.example.co.uk`
matches `example.co.uk`; but `alice.github.io` and `bob.github.io`, or `mynas.duckdns.org` and
`other.duckdns.org`, are different sites), or per URI exact host, prefix, exact URL, regular expression or never.

## Import and export

Import and export happen **in your browser** — files are parsed and encrypted locally, never uploaded in
plaintext.

| Format | Import | Export |
|---|---|---|
| KeePass 2 database (`.kdbx`, KDBX 4.x; 3.1 import) | yes — with password and/or key file | yes — KDBX 4 (AES-256, Argon2d), optionally with a key file |
| KeePass 2 XML | yes | yes (plaintext) |
| Bitwarden JSON (unencrypted) | yes | yes (plaintext) |
| Bitwarden JSON (password-protected export) | yes | — |
| Bitwarden CSV | yes | — |

KeePass groups become folders (nested groups as `Parent/Child`), standard fields map to the login, custom
strings become custom fields, `otp` / `TimeOtp-Secret-Base32` become TOTP, and entry history becomes password
history. Bitwarden logins, notes, cards, identities, custom fields, TOTP and password history are preserved.

!!! warning
    XML, JSON and CSV exports contain **all your passwords in plain text**. Prefer the encrypted KDBX export,
    and delete plaintext exports as soon as you no longer need them.

## Browser extensions and apps

* **Chrome and Firefox** — the Ludworks extension fills logins, saves new ones, generates passwords and shows
  TOTP codes. See [Browser extension](../clients/browser-extension.md).
* **Android** — the Passwords app with biometric unlock and Android autofill. See
  [Android apps](../clients/android.md).
* **Windows** — the desktop app includes the vault. See [Desktop apps](../clients/desktop.md).

All of them talk to the same encrypted vault and sync changes with revision numbers, so edits made offline on
one device are not silently overwritten by another.
