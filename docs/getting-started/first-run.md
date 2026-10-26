# First-time setup

A fresh Ludworks server has no accounts. Until the first administrator is created every API route except
the setup endpoints answers `503 setup_required`, and the web UI shows the setup screen.

To stop someone else on your network from claiming the server before you do, setup requires a **one-time setup
token** that only a person with access to the server machine can read.

## 1. Find the setup token

The token is generated on the first start with an empty database and written to `setup-token.txt` in the data
directory (readable only by the service account and administrators). The server log only says where the file
is: log readers must not be able to claim the server.

| Platform | Command |
|---|---|
| Linux `.deb` | `sudo cat /var/lib/ludworks/setup-token.txt` |
| Windows service | `Get-Content C:\ProgramData\Ludworks\setup-token.txt` in an elevated PowerShell |
| Console / custom data dir | `<data dir>/setup-token.txt` (the console shows the path) |

Lost it? Run `ludworks admin create-setup-token` (as the service user, with the same configuration) to issue a
new one. Setup attempts are rate limited per IP.

## 2. Verify the certificate fingerprint

With the default self-signed certificate your browser warns about an untrusted certificate. The server logs
the certificate's SHA-256 fingerprint on every start (next to the setup token on the first start), as a
string of 32 hexadecimal byte pairs such as `5E:3A:…:9C`.

Open the certificate details in the browser and compare the SHA-256 fingerprint before you continue. If they
differ, something between you and the server is intercepting the connection — stop. (See
[TLS](configuration.md#tls) for using your own certificate instead.)

## 3. Create the administrator

Browse to `https://<server>:8443/`. The setup screen asks for:

* the **setup token**,
* the administrator's **username**, **email** and **password** (at least 12 characters by default — use a
  long passphrase; this account controls everything),
* optionally a **server name** shown in the UI, emails and TV feeds.

Submitting creates the admin account, logs you in and invalidates the token (the file is deleted).

The same can be done through the API, which is handy for automated deployments:

```bash
curl --cacert server.pem -X POST https://nas.local:8443/api/v1/setup \
  -H 'Content-Type: application/json' \
  -d '{"setup_token":"<token>","username":"admin","email":"admin@example.com","password":"<long passphrase>","server_name":"Home NAS"}'
```

## 4. Recommended next steps

1. **Enable two-factor authentication** for the admin account in your account settings and store the
   recovery codes offline. Consider turning on `security.require_totp_for_admins`.
2. **Configure SMTP** so verification and password-reset emails work ([Mail](../admin/mail.md)).
3. **Decide on registration**: it is *off* by default. Either create accounts yourself on the admin page or
   enable self-registration, ideally with email verification ([Users & registration](../admin/users.md)).
4. **Create library spaces** for shared media and review the category taxonomy
   ([Libraries & categories](../admin/libraries.md)).
5. Install **ffmpeg** if you want video thumbnails, transcoding and live channel streams.
6. Work through the [hardening checklist](../security.md#hardening-checklist) and set up
   [backups](../admin/maintenance.md#backups).
