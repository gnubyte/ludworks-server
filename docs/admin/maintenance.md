# Backups & maintenance

## Backups

A complete backup consists of four things:

| What | Where | Why |
|---|---|---|
| Database | `<data>/ludworks.db` | Accounts, settings, file index and metadata, notes, vault ciphertext, workbooks, TV lineups and channels, audit log |
| Server key | `<data>/secret.key` | Decrypts stored SMTP/IMAP passwords, the TMDB key and users' TOTP secrets. Without it 2FA must be reset for everyone. |
| Configuration | `/etc/ludworks/config.json` (Linux) or `<data>\config.json` (Windows) | Listen address, TLS, proxies |
| Files | `<data>/storage/` | Everything users uploaded |

Optional: `<data>/tls/` (to keep the same self-signed certificate fingerprint, so apps do not need to re-pin).
Not needed: `thumbs/` (regenerated), `tmp/` (partial uploads).

### Database: online backup

Never copy `ludworks.db` while the server is running — the copy may be inconsistent. Instead use
**Admin → Maintenance → Back up database** or:

```bash
curl -X POST -H "Authorization: Bearer $LUDWORKS_ADMIN_TOKEN" https://nas.local:8443/api/v1/admin/db-backup
# {"path":"/var/lib/ludworks/backups/ludworks-20261026T120000Z.db","size":73400320}
```

This writes a consistent snapshot with SQLite `VACUUM INTO` to `<data>/backups/ludworks-<timestamp>.db` while
the server keeps running. Copy that file (plus `secret.key` and the configuration) off the machine with your
backup tool, and delete old snapshots from `backups/` periodically.

A simple nightly job on Linux using a dedicated admin token with only the `admin` scope:

```bash
#!/bin/sh
set -eu
curl -fsS -X POST -H "Authorization: Bearer $(cat /root/.ludworks-backup-token)" \
  https://localhost:8443/api/v1/admin/db-backup --cacert /var/lib/ludworks/tls/self-signed.crt
rsync -a --delete /var/lib/ludworks/storage/ /mnt/backup/ludworks/storage/
rsync -a /var/lib/ludworks/backups/ /var/lib/ludworks/secret.key /etc/ludworks/config.json /mnt/backup/ludworks/
```

(With the default self-signed certificate, `self-signed.crt` is its own CA certificate, so `curl --cacert`
verifies it properly; the certificate covers `localhost`. With `files` or `acme` TLS use the public host name and
drop `--cacert`.)

### Files

`storage/` is ordinary files and folders — back it up with any tool (rsync, restic, borg, Windows File
History, a second disk). The layout is one directory per space. File metadata (categories, tags, media
information) lives in the database, so back both up together.

### Restore

1. Install the same or a newer Ludworks version and stop the service.
2. Put the backed-up database at `<data>/ludworks.db` (rename the snapshot), restore `secret.key`,
   `config.json` and `storage/`, and make sure everything is owned by the service user
   (`sudo chown -R ludworks:ludworks /var/lib/ludworks` on Linux).
3. Start the service. Database migrations run automatically.
4. If files changed on disk after the database snapshot, run a rescan (`POST /api/v1/files/rescan`) per space.

Test a restore on a spare machine at least once.

## Upgrades

* **Linux:** `sudo apt install ./ludworks_<version>_amd64.deb`. The package stops the service, replaces the
  program and starts it again if it was running.
* **Windows:** run the new `ludworks_<version>_windows_amd64_setup.exe`, or run `install-service.ps1` from the
  new zip in an elevated PowerShell. Both stop the service, replace `ludworks.exe` and start it again.

Configuration and data are kept. Schema migrations run automatically at start-up and are recorded per
module; downgrading to an older version after a migration is not supported, so **back up first**. Verify
each download's build-provenance attestation with `gh attestation verify` before installing it (see
[Installation](../getting-started/installation.md#downloads)); `SHA256SUMS` alone only detects damaged downloads.

## Housekeeping that runs automatically

| Task | Schedule |
|---|---|
| Purge trashed files older than `files.trash_retention_days` | periodically |
| Purge incomplete uploads older than `files.upload_expiry_hours` | hourly |
| Expire sessions past their idle/absolute lifetime | continuously |
| Poll the IMAP mailbox (mail-in) | every `imap.poll_minutes` |

## Command-line maintenance

| Command | Use |
|---|---|
| `ludworks admin reset-password <username>` | Set a new password for a user when nobody can log in as admin |
| `ludworks admin create-setup-token` | New one-time setup token if the original was lost before setup |
| `ludworks version` | Print the installed version |

Run them as the service user with the same configuration (`sudo -u ludworks ludworks …` on Linux, an elevated
prompt on Windows).

## Moving the data directory

Stop the server, move the whole directory (preserving ownership and permissions), set `data_dir` in the
configuration (and on Linux allow it in the systemd unit with `ReadWritePaths=`), then start the server. The
database stores space directories relative to `<data>/storage`, so nothing else needs to change.
