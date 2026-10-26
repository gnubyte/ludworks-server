# Browser extension

The Ludworks password-manager extension (repository `ludworks-browser`) brings your vault to **Chrome** (and
other Chromium browsers such as Edge and Brave) and **Firefox**. One source tree produces both Manifest V3
builds, and the extension has **no third-party runtime dependencies**.

## Features

* Unlock with your master password; the vault key is kept only in session memory and the extension locks
  after inactivity (15 minutes by default) or when the browser restarts.
* Fill logins on the current site — matching uses each item's URIs and match rules (registrable domain by
  default, see [Passwords](../guide/passwords.md#daily-use)) — and offer to save new or changed logins.
* Copy usernames, passwords and **TOTP codes**; generate passwords and passphrases.
* Search, view and edit items; everything is encrypted locally before it is sent.

## Installing

Build or download the extension packages from the `ludworks-browser` repository (its README lists the build
command; the build uses Node without npm packages), then:

* **Chrome / Edge / Brave** — open `chrome://extensions`, enable *Developer mode*, click *Load unpacked* and select
  the Chrome build folder (or install a packaged `.crx`/store listing if your organisation publishes one).
* **Firefox** — install the signed `.xpi`, or for testing open `about:debugging` → *This Firefox* →
  *Load Temporary Add-on* and select the Firefox build's `manifest.json`.

## Connecting to your server

1. Open the extension and enter the server URL, e.g. `https://nas.local:8443`.
2. With a self-signed certificate, first open the server once in a browser tab, verify the fingerprint against
   the server log and accept the certificate — extensions use the browser's certificate trust.
3. Log in with your account username and password (and 2FA code). The extension uses
   [token mode](../api/authentication.md#logging-in) and appears in your session list, where you can revoke
   it.
4. Unlock the vault with your **master password**.

The extension calls the API cross-origin with a bearer token only; cookies are never used. An admin can
restrict which extension IDs may connect with `security.allowed_extension_origins`
(`chrome-extension://<id>`, `moz-extension://<uuid>`).

## Privacy

The extension sends only ciphertext and the account login to your own server — no telemetry and no third-party
services. Website icons are not fetched from external services.
