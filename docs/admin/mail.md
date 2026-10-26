# Mail: SMTP, IMAP and mail-in

Ludworks can **send** email through any SMTP server (for verification links, password resets and test
messages) and **receive** email from an IMAP mailbox to turn messages into notes ("mail-in"). Both are
optional and off by default. Passwords for both are encrypted at rest with the server key and are never
shown again after saving.

## SMTP (outgoing)

Admin → Settings → Mail → SMTP:

| Field | Typical value |
|---|---|
| Enabled | on |
| Host / Port | `smtp.example.com` / `587` |
| Security | `starttls` (port 587), `tls` (implicit TLS, port 465) or `none` (only for a relay on localhost) |
| Username / Password | the mailbox or an app password; leave the username empty for a relay without authentication |
| From address / name | `nas@example.com` / `Ludworks` |

Save, then use **Send test email** (`POST /api/v1/admin/settings/smtp/test` with `{"to": "you@example.com"}`).
A failure returns `502 smtp_failed` with the server's reason, for example an authentication error.

Providers with 2FA (Gmail, Outlook, iCloud, Fastmail) require an *app password* rather than your normal
password.

## IMAP (incoming) and mail-in

Mail-in lets users create notes by emailing them to a dedicated mailbox the server polls — handy for saving
receipts, articles or quick thoughts from a phone.

1. Create a dedicated mailbox, e.g. `notes@example.com`. Do not use your personal inbox.
2. Admin → Settings → Mail → IMAP: host, port (`993`), security (`tls`), username, password, mailbox
   (`INBOX`), polling interval (`poll_minutes`, default 5), whether to mark processed messages as seen, and
   whether to save attachments.
3. Click **Test connection** (`POST /api/v1/admin/settings/imap/test`); it lists the mailboxes and the number
   of unseen messages, or fails with `502 imap_failed`.
4. Name the mail server that receives mail for the mailbox in `mail_in.trusted_authserv_ids` (see
   [Sender authentication](#sender-authentication) below). **Until you do, nothing is imported**: the
   mailbox is not even opened and the server log says so.
5. Each user who wants mail-in turns it on under Account → Profile (*mail-in*, `mail_in_enabled`). Their email
   address must be **verified**.

How messages are processed:

* Only **unread** messages are considered.
* The sender must be **authenticated**: the trusted receiving server must report `dmarc=pass` for the
  domain of the `From` address. Messages without that are skipped.
* A message becomes a note only if its (single) `From` address equals the **verified email** of a user who
  enabled mail-in. Everything else is ignored.
* The subject becomes the note title and the plain-text body becomes the Markdown body.
* With `save_attachments` on, attachments are stored in the user's home space under `Mail Attachments/`.
* With `mark_seen` on, processed messages are marked as read.

### Sender authentication

Anyone can put any address in `From`, and the `From` address decides whose account a message is written
to. Mail-in therefore trusts a message only when the mail server that **received** it vouches for the
sender: that server adds an `Authentication-Results` header (RFC 8601) with its DMARC verdict, and mail-in
imports the message only when

* the topmost `Authentication-Results` headers come from a server listed in `mail_in.trusted_authserv_ids`
  (the *authserv-id* is the first word of the header, e.g. `mx.google.com`; headers below the first one
  from any other server came with the message and are ignored), and
* they report `dmarc=pass` with `header.from` equal to the domain of the `From` address, and no other DMARC
  result.

A missing header, `dmarc=none`, `temperror`, `permerror`, `bestguesspass` or `fail`, a pass for a different
domain, or a verdict from a server you did not list means the message is skipped (logged as *sender not
authenticated* with the reason).

To find the authserv-id, open a message in the mailbox, view its original source and read the topmost
`Authentication-Results:` line. Examples: Gmail `mx.google.com`; Fastmail `*.messagingengine.com` (it stamps
from several hosts); a self-hosted Postfix with OpenDMARC or Rspamd uses the host name you configured
(`authserv_id`/`AuthservID`). Requirements:

* The receiving server must evaluate DMARC and must **remove incoming `Authentication-Results` headers that
  claim its own authserv-id** (RFC 8601 §5); large providers do, check your own MTA's configuration.
  Otherwise a sender could forge a `dmarc=pass` that looks like it came from your server.
* Users' sending domains must publish a DMARC record (`p=none` is enough) and their mail must pass SPF or
  DKIM aligned with the `From` domain. Mail from a domain without DMARC is never imported.
* Microsoft 365 / Exchange Online writes `Authentication-Results` without an authserv-id, so it cannot be
  trusted this way.

`mail_in.allow_unauthenticated_senders` turns the check off (only an explicit topmost `dmarc=fail` is still
skipped). Use it only for a mailbox that receives mail exclusively from sources you control: with it,
anyone who can send to the mailbox can create notes, and save files, in the account of any user whose
address they write in `From`.

!!! warning
    Even with sender authentication, keep the mailbox address unpublished: mail-in is a convenience for
    your users, and every authenticated message from their address is imported.

## Settings keys

All fields map to the `smtp`, `imap` and `mail_in` namespaces in the [settings reference](settings.md#smtp).
