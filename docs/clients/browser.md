# Ludworks Browser

Ludworks Browser is a full web browser for **Windows, macOS, Linux** (repository `ludworks-browser`, folder
`desktop/`) and **Android** (repository `ludworks-android`, module `:browser`). It signs in to your Ludworks server
and keeps your **passwords, passkeys, bookmarks, history, open tabs and settings** in sync across your devices —
end-to-end encrypted with your vault master password, so the server only ever stores ciphertext.

* Desktop: Chromium 152 with Widevine DRM (castlabs Electron for Content Security), H.264/HEVC/AV1/VP9, WebRTC and
  the game-streaming APIs (Gamepad, Keyboard Lock, WebCodecs, WebTransport, WebGPU).
* Android: the system WebView (Chromium, updated through Google Play) with `androidx.webkit`.

## Signing in

Use the account button in the toolbar: enter your server address, your Ludworks username or email and your account
password (and the two-step code if you use one). For a server with its own (self-signed) certificate the browser
shows the certificate's SHA-256 fingerprint first — compare it with **Admin → System** — and remembers it; nothing is
sent until you confirm it, and a changed certificate is refused until you confirm again.

The browser keeps a **device token** limited to the vault scopes (`vault:read vault:write`, 180 days); it appears in
**Settings → Sessions & tokens** as *Ludworks Browser (…)* and can be revoked there. Your full login session is
revoked right after the token is issued.

Then unlock with your **master password** (the one that encrypts your vault — not your account password). If your
account has no vault yet, the browser offers to create it.

## What syncs

| Data | Where it is stored | Encryption |
|---|---|---|
| Passwords, cards, identities, notes, **passkeys** | Your vault (the same items the web app, extension and apps use) | Vault key ([VAULT-CRYPTO §3](../spec/VAULT-CRYPTO.md)) |
| Bookmarks, history (90 days), open tabs, settings | Browser sync ([API §16](../api/browser-sync.md)) | Keys derived from the vault key ([VAULT-CRYPTO §9](../spec/VAULT-CRYPTO.md)) |

Each kind of browser data can be switched off under **Ludworks sync** in the browser. Private windows never sync or
record history.

## Passwords and passkeys

* **Autofill**: focus a login field and pick a login from the browser's own dropdown (pages cannot read or overlay it).
  Logins are matched with the same rules as everywhere else in Ludworks (registrable domain by default).
* **Save prompts**: after you sign in somewhere new, the browser offers to save the login (bound to that exact host).
* **Passkeys**: websites that support passkeys can create and use them directly in your vault — they then work on
  all your devices. *Use another device or security key* falls back to Windows Hello / security keys.

## Bitwarden and other password managers

*Passwords & passkeys → Import* reads Bitwarden JSON (also password-protected, PBKDF2 or Argon2id) and CSV exports —
including Bitwarden **passkeys** — plus KeePass (.kdbx / XML) and Chrome, Edge or Firefox password CSV files.
*Export* writes Bitwarden JSON (password-protected recommended), Bitwarden CSV or a KeePass database; Bitwarden can
import the JSON file back, passkeys included.

Bookmarks and history can be imported directly from Edge, Chrome, Brave or Vivaldi on the same computer (Settings →
Import data), or from any browser's bookmark HTML export.

## Installing

* **Windows**: run `LudworksBrowserSetup-<version>-win-x64.exe` (per-user install, no administrator rights). Set it
  as your default browser from its welcome page or **Settings → Default browser**. Re-running a newer installer
  updates in place; uninstall from *Settings → Apps*.
* **Linux**: `sudo apt install ./ludworks-browser_<version>_amd64.deb` (or unpack the `.tar.gz`).
* **macOS**: open the `.dmg` and drag *Ludworks Browser* to Applications.
* **Android**: install `ludworks-browser-<version>.apk` and choose it as default browser in Android settings.

## Updates

The browser engine is pinned by version and SHA-256 in `desktop/electron.lock.json`; a scheduled workflow proposes an
update whenever castlabs publishes a newer Electron build with Chromium security fixes. Install the new release
over the old one — your profile is kept.
