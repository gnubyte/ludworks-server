# Configuration

Ludworks has two kinds of configuration:

* **Bootstrap configuration** (this page): where the data lives, which address to listen on, TLS, trusted
  reverse proxies and logging. It comes from a JSON file, environment variables and command-line flags, and
  is read once at start-up.
* **Runtime settings**: everything an administrator can change while the server runs — registration,
  sessions, SMTP/IMAP, upload limits, ffmpeg, TV feeds… They are stored in the database and edited on the
  admin page or through `PATCH /api/v1/admin/settings`. See the [settings reference](../admin/settings.md).

## Precedence

From lowest to highest priority:

1. built-in defaults for the operating system,
2. the JSON configuration file,
3. `LUDWORKS_*` environment variables,
4. command-line flags of `ludworks serve`.

## Configuration file

| Platform | Default location |
|---|---|
| Linux | `/etc/ludworks/config.json` |
| Windows | `<data dir>\config.json`, i.e. `C:\ProgramData\Ludworks\config.json` |
| Other | `<data dir>/config.json` |

Use `--config FILE` or `LUDWORKS_CONFIG` to read another file. The default file is optional; a file named
explicitly must exist. The file is parsed **strictly**: an unknown key or trailing garbage is a start-up
error, so that a typo in a security-relevant key (say `trusted_proxy`) cannot be silently ignored.
A UTF-8 byte-order mark (as saved by Notepad) is tolerated.

A complete example with every key:

```json
{
  "data_dir": "/var/lib/ludworks",
  "listen": ":8443",
  "tls": {
    "mode": "self-signed",
    "cert_file": "",
    "key_file": "",
    "acme_domains": [],
    "acme_email": "",
    "redirect_listen": ""
  },
  "trusted_proxies": [],
  "log": {
    "level": "info",
    "format": "text"
  }
}
```

| Key | Default | Meaning |
|---|---|---|
| `data_dir` | Linux `/var/lib/ludworks`, Windows `%ProgramData%\Ludworks`, others `~/.ludworks` | Where everything is stored (see [data directory](#data-directory-layout)). |
| `listen` | `:8443` | `host:port` of the main listener. `:8443` = all interfaces; `127.0.0.1:8443` = this machine only (use behind a reverse proxy). |
| `tls.mode` | `self-signed` | `self-signed`, `files`, `acme` or `off` — see [TLS](#tls). |
| `tls.cert_file`, `tls.key_file` | — | PEM certificate chain and private key for `files` mode. |
| `tls.acme_domains` | — | Host names to request certificates for in `acme` mode. |
| `tls.acme_email` | — | Contact email for the ACME account (optional but recommended). |
| `tls.redirect_listen` | `""` (off) | Optional plain-HTTP listener (e.g. `:8080` or `:80`) that only redirects to HTTPS. Not allowed with `tls.mode` `off`. |
| `trusted_proxies` | `[]` | CIDRs or single IPs of reverse proxies whose `X-Forwarded-For` / `X-Forwarded-Proto` headers are trusted. |
| `log.level` | `info` | `debug`, `info`, `warn` or `error`. |
| `log.format` | `text` | `text` or `json` (structured, for log shippers). |

## Environment variables

| Variable | Overrides |
|---|---|
| `LUDWORKS_CONFIG` | path of the configuration file |
| `LUDWORKS_DATA_DIR` | `data_dir` |
| `LUDWORKS_LISTEN` | `listen` |
| `LUDWORKS_TLS_MODE` | `tls.mode` |
| `LUDWORKS_TLS_CERT_FILE` | `tls.cert_file` |
| `LUDWORKS_TLS_KEY_FILE` | `tls.key_file` |
| `LUDWORKS_TLS_ACME_DOMAINS` | `tls.acme_domains` (comma, space or semicolon separated) |
| `LUDWORKS_TLS_ACME_EMAIL` | `tls.acme_email` |
| `LUDWORKS_TLS_REDIRECT_LISTEN` | `tls.redirect_listen` (`off` disables it) |
| `LUDWORKS_TRUSTED_PROXIES` | `trusted_proxies` (comma, space or semicolon separated) |
| `LUDWORKS_LOG_LEVEL` | `log.level` |
| `LUDWORKS_LOG_FORMAT` | `log.format` |

## Command-line flags

`ludworks serve` accepts `--data DIR`, `--listen ADDR`, `--tls MODE`, `--config FILE` and
`--log-level LEVEL`. For example, a throw-away development server:

```bash
ludworks serve --data ~/ludworks-dev --listen 127.0.0.1:18080 --tls off
```

## Data directory layout

| Path | Contents |
|---|---|
| `ludworks.db` | SQLite database (WAL mode, so also `ludworks.db-wal` and `ludworks.db-shm` while running): users, settings, file index, metadata, notes, vault ciphertext, workbooks, audit log |
| `secret.key` | 32 random bytes (mode 0600) used to encrypt secrets at rest (SMTP/IMAP passwords, TMDB key, TOTP secrets) and to sign cursors. **Back it up with the database** — without it those secrets cannot be decrypted. |
| `setup-token.txt` | One-time first-run token (mode 0600); deleted after setup |
| `storage/` | Your files: one directory per space |
| `tmp/` | Partial uploads (`tmp/uploads/<id>.part`) and scratch files — must be on the same volume as `storage/` |
| `thumbs/` | Cached thumbnails and posters (safe to delete; regenerated) |
| `backups/` | Database backups created from the admin page (`ludworks-<timestamp>.db`) |
| `tls/` | Generated self-signed certificate and key, ACME account and certificate cache |
| `config.json` | Configuration file (Windows and non-Linux defaults only) |
| `ludworks.log` | Log file of the Windows service (rotated to `ludworks.log.1` at 10 MiB); on Linux logs go to the journal |

All directories are created with owner-only permissions. On Windows a newly created data directory gets an
access list limited to SYSTEM, Administrators and the account running Ludworks, because `%ProgramData%`
is readable by every user by default.

## TLS

### `self-signed` (default)

On first start Ludworks generates an ECDSA P-256 certificate (valid 825 days) for the host name,
`localhost` and every local interface IP, and stores it as `<data>/tls/self-signed.crt` and
`<data>/tls/self-signed.key`. Its **SHA-256 fingerprint** (uppercase hex pairs separated by colons) is logged on
every start and shown on the admin page and by `GET /api/v1/setup/status`.

Browsers will warn that the certificate is not trusted. That is expected — compare the fingerprint the browser
shows (certificate details → SHA-256) with the one in the server log before accepting it. The apps and the
Python SDK use **trust on first use (TOFU)**: on first connection they show the fingerprint and, once you
confirm it matches the server log, pin it — any later certificate change is refused instead of silently
accepted.

The certificate is regenerated automatically at start-up when it is missing, expires within 30 days, or no
longer covers the host name or one of the IPv4 addresses of the machine's real interfaces (for example after
renaming the host or changing its address). Container and VM bridges (`docker0`, `br-…`, `veth…`, `virbr…`, …)
and IPv6 addresses are not included, because they come and go and would change the certificate on almost every
restart; a specific IPv6 `listen` address is still included.

Renewal is announced: from 60 days before the certificate expires, the server keeps its successor in
`self-signed-next.crt` / `self-signed-next.key` and publishes it in `GET /api/v1/setup/status`
(`tls_next_fingerprint_sha256`, `tls_next_certificate`). Ludworks Browser reads it over the connection it already
trusts and trusts the successor ahead of the switch, so a renewal needs no confirmation.

To force a new certificate, stop the server, delete `self-signed.crt`, `self-signed.key` and any
`self-signed-next.*` and start it again. An unannounced new certificate has a **new fingerprint**: apps that
pinned the old one will refuse to connect until you verify and re-pin the new fingerprint — that is the
protection working, not a bug.

### `files`

Use a certificate you manage yourself (internal CA, or a public certificate renewed by another tool):

```json
{ "tls": { "mode": "files", "cert_file": "/etc/ludworks/tls/fullchain.pem", "key_file": "/etc/ludworks/tls/privkey.pem" } }
```

The key must be readable by the `ludworks` user. The files are re-read automatically when they change, so a
renewal by another tool (certbot, an internal CA agent) needs no restart.

### `acme`

Obtain and renew certificates automatically from Let's Encrypt. Only the listed domains are ever requested.
The CA must be able to reach the server from the internet for validation: either on port 443 (TLS-ALPN-01,
answered by the HTTPS listener — so `listen` must be `:443`, or port 443 forwarded to it) or on port 80
(HTTP-01, answered by the redirect listener when `redirect_listen` is `:80`). Binding ports below 1024 on Linux
needs `CAP_NET_BIND_SERVICE` (add `AmbientCapabilities=CAP_NET_BIND_SERVICE` with `systemctl edit ludworks`) or
a firewall port forward. The ACME account and certificates are cached in `<data>/tls/`.

```json
{ "listen": ":443", "tls": { "mode": "acme", "acme_domains": ["nas.example.com"], "acme_email": "you@example.com", "redirect_listen": ":80" } }
```

Exposing a NAS directly to the internet is a serious decision — read the [security model](../security.md) first.

### `off`

Plain HTTP. **Only** use this behind a reverse proxy that terminates TLS, with Ludworks listening on
`127.0.0.1` (or a private interface reachable only by the proxy). Never expose `tls.mode: off` to a network:
passwords and session cookies would travel in clear text.

All TLS modes use TLS 1.2 or newer with modern cipher suites. HSTS is sent whenever the request arrived
over TLS.

## Reverse proxy and trusted proxies

To run Ludworks behind nginx, Caddy, Traefik or similar:

1. Set `tls.mode` to `off` and `listen` to `127.0.0.1:8443`.
2. Set `trusted_proxies` to the proxy's address, e.g. `["127.0.0.1", "::1"]`. Only requests from these
   addresses may set the client IP (`X-Forwarded-For`) and scheme (`X-Forwarded-Proto`); from anywhere else
   the headers are ignored, so a client cannot spoof its IP to dodge rate limits or forge the audit log.
3. Make the proxy forward the real client address and `X-Forwarded-Proto: https`; Ludworks then marks the
   session cookie `Secure` and sends HSTS.
4. Do **not** let the proxy buffer request bodies: resumable uploads stream large chunks, and video streaming
   needs Range requests to pass through untouched.
5. Set `general.public_url` in the admin settings to the external URL (used in emails and TV feed URLs).

nginx:

```nginx
server {
    listen 443 ssl;
    http2 on;
    server_name nas.example.com;
    ssl_certificate     /etc/letsencrypt/live/nas.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/nas.example.com/privkey.pem;

    client_max_body_size 0;            # tus chunks and simple uploads
    proxy_request_buffering off;       # stream uploads
    proxy_buffering off;               # stream video and live TV
    proxy_read_timeout 1h;
    proxy_send_timeout 1h;

    location / {
        proxy_pass http://127.0.0.1:8443;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

Caddy (automatic HTTPS, streams by default):

```
nas.example.com {
    reverse_proxy 127.0.0.1:8443
}
```

Ludworks compares the `Origin` header of cookie-authenticated requests with its own origin, so the proxy must
pass the original `Host` header through unchanged.
