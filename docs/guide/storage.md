# Storage & system

The **System** app shows how storage is used. Everybody sees their own numbers; administrators also see the
disks and every user.

## For every user

* **Your usage** against your quota (or "unlimited"), with a breakdown **by category** — movies, TV, music,
  images, games, documents, software, other, uncategorised — and how much your **trash** still occupies.
* **Spaces** you can read, each with its size and number of files.

Usage counts the files you own (in your home space and files you uploaded to libraries), excluding the trash.
When you reach your quota, uploads and copies fail with *quota exceeded* until you free space; empty the trash
to reclaim space immediately.

## For administrators

* **Volumes** — total, used and free space of the disk(s) holding the data directory.
* **Users** — usage and quota of every account, to spot who fills the disk.
* **Pending uploads** — space held by unfinished resumable uploads (purged after `files.upload_expiry_hours`).
* **Server information** — version, Go version, OS/architecture, host name, uptime, database size, number of
  users and files, whether ffmpeg is available (and its version), and the TLS mode, certificate fingerprint and
  expiry date.

The same data is available from `GET /api/v1/system/storage` and (admins only) `GET /api/v1/system/info`, for
example to feed a monitoring system:

```bash
curl -s -H "Authorization: Bearer $TOKEN" https://nas.local:8443/api/v1/system/storage | jq '.volumes'
```

## Keeping enough free space

Uploads are checked **before** they start: the server requires the full size of the upload plus 1 GiB of
headroom to be free on the disk, so a disk never fills up halfway through a large transfer. Keep an eye on the
volume view, set per-user quotas (Admin → Users), and lower `files.trash_retention_days` if trash grows large.
