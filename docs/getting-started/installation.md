# Installation

Ludworks is a single self-contained program. Pick the package for your platform, start it, then complete
[first-time setup](first-run.md).

**Requirements**

* 64-bit Linux (Debian 12+ / Ubuntu 22.04+ with systemd for the `.deb`; any distribution for the plain binary)
  on x86-64 (`amd64`) or 64-bit ARM (`arm64`), or 64-bit Windows 10/11 / Windows Server 2019+.
* 512 MiB RAM minimum; 1 GiB+ recommended when transcoding.
* Enough disk for your files. The data directory should be on the disk that holds your library: uploads
  are written to a temporary file **on the same volume** and then renamed into place.
* Optional: `ffmpeg` / `ffprobe` on the `PATH` for video thumbnails, transcoding, embedded subtitles and the
  continuous MPEG-TS channel streams (see [TV integrations](../guide/tv-integrations.md#ffmpeg-optional-features)).

By default the server listens on **port 8443 with HTTPS** using a self-signed certificate it generates on
first start.

## Downloads

Every release on GitHub (`gnubyte/ludworks-server`, tag `v<version>`) carries:

| File | Platform |
|---|---|
| `ludworks_<version>_amd64.deb` | Debian / Ubuntu on x86-64 |
| `ludworks_<version>_arm64.deb` | Debian / Ubuntu on 64-bit ARM (Raspberry Pi 4/5 with a 64-bit OS, ARM servers) |
| `ludworks_<version>_windows_amd64_setup.exe` | Windows installer (service, optional firewall rule) |
| `ludworks_<version>_windows_amd64.zip` | Windows without an installer: `ludworks.exe`, `install-service.ps1`, `uninstall-service.ps1`, `config.example.json`, `README-windows.txt`, `LICENSE` |
| `SHA256SUMS` | SHA-256 checksums of all of the above |

**Verify what you downloaded before installing it.** Each file has a signed
[build-provenance attestation](https://docs.github.com/en/actions/security-for-github-actions/using-artifact-attestations):
proof, signed through Sigstore with a short-lived certificate for the release workflow, that this exact file
was built by `.github/workflows/release.yml` from the tagged source. Check it with the
[GitHub CLI](https://cli.github.com/) (Linux, Windows and macOS):

```bash
gh attestation verify ludworks_0.1.0_amd64.deb --repo gnubyte/ludworks-server \
  --signer-workflow gnubyte/ludworks-server/.github/workflows/release.yml --source-ref refs/tags/v0.1.0
```

```powershell
gh attestation verify .\ludworks_0.1.0_windows_amd64_setup.exe --repo gnubyte/ludworks-server `
  --signer-workflow gnubyte/ludworks-server/.github/workflows/release.yml --source-ref refs/tags/v0.1.0
```

Use the version you downloaded in the file name and in `--source-ref`. Do not install a file for which this
fails. `SHA256SUMS` only detects a damaged download (`sha256sum --ignore-missing -c SHA256SUMS`, or
`Get-FileHash <file> -Algorithm SHA256` on Windows): it is stored in the same release as the files, so whoever
could replace a file could replace it too.

The `.deb` and `.zip` files are also reproducible: building the tag with `scripts/package.sh` (see
[Building from source](#building-from-source)) gives byte-identical files, which you can compare with
`SHA256SUMS`. The release workflow checks this on every release by building twice. The Windows programs are
not yet Authenticode-signed, so Windows SmartScreen shows "Unknown publisher"; rely on the attestation.

## Linux (.deb)

```bash
sudo apt install ./ludworks_0.1.0_amd64.deb     # or ludworks_0.1.0_arm64.deb
```

The package depends on `adduser` and recommends `ffmpeg` (`apt` installs it unless you pass
`--no-install-recommends`; Ludworks works without it). It installs:

| Path | What |
|---|---|
| `/usr/bin/ludworks` | the server binary (static, no other runtime dependencies) |
| `/usr/lib/systemd/system/ludworks.service` | the hardened systemd unit |
| `/etc/ludworks/config.json` | the [configuration file](configuration.md) (a dpkg conffile: your edits survive upgrades); mode 0640, owner `root`, group `ludworks` |
| `/usr/share/doc/ludworks/copyright` | license and third-party notices |
| `/usr/share/doc/ludworks/examples/bind-privileged-port.conf` | systemd drop-in example for serving on port 443 (see below) |

On first installation the package also

* creates the system user and group `ludworks` (home `/var/lib/ludworks`, shell `/usr/sbin/nologin`, no
  password),
* creates the data directory `/var/lib/ludworks` (mode 0750, owner `ludworks:ludworks`); the server creates
  everything inside it with owner-only permissions (directories 0700, files 0600),
* enables `ludworks.service` at boot and starts it, then prints where to find the setup token.

The default configuration is:

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

Check that it is running and read the **one-time setup token**:

```bash
systemctl status ludworks
sudo cat /var/lib/ludworks/setup-token.txt
sudo journalctl -u ludworks -n 50     # the TLS certificate fingerprint is logged here (the token is not)
```

Then open `https://<server>:8443/` in a browser and follow [first-time setup](first-run.md).

### The systemd unit

The service runs `/usr/bin/ludworks serve --config /etc/ludworks/config.json` as the unprivileged `ludworks`
user with `UMask=0077`, restarts on failure and may open 65536 files. It is sandboxed:

* `NoNewPrivileges`, an empty `CapabilityBoundingSet` (no capabilities at all), `RestrictSUIDSGID`;
* `ProtectSystem=strict` with `ReadWritePaths=/var/lib/ludworks` (the data directory is the only writable
  place), `ProtectHome=true`, `PrivateTmp`, `PrivateDevices`;
* `ProtectKernelTunables`, `ProtectKernelModules`, `ProtectKernelLogs`, `ProtectControlGroups`,
  `ProtectClock`, `ProtectHostname`, `ProtectProc=invisible`;
* `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK`, `RestrictNamespaces`, `RestrictRealtime`,
  `LockPersonality`, `MemoryDenyWriteExecute`, `RemoveIPC`;
* `SystemCallArchitectures=native` and `SystemCallFilter=@system-service` (other system calls fail with
  `EPERM`).

`systemd-analyze security ludworks` rates the unit around 1.5 ("OK"). Manage it with the usual commands:

```bash
sudo systemctl restart ludworks
sudo systemctl stop ludworks
sudo systemctl enable --now ludworks
sudo journalctl -u ludworks -f
```

Never edit the unit file itself (upgrades replace it); use a drop-in with `sudo systemctl edit ludworks`.

**Data on another disk.** Either point `data_dir` at a directory on that disk (see
[Configuration](configuration.md)) or mount the disk at `/var/lib/ludworks/storage`. When you move the data
directory, allow the sandbox to write there and give it to the service user:

```bash
sudo systemctl edit ludworks
# [Service]
# ReadWritePaths=/srv/ludworks
sudo install -d -m 0750 -o ludworks -g ludworks /srv/ludworks
```

Libraries under `/home` stay invisible to the service because of `ProtectHome=true`; add
`ProtectHome=read-only` (or `false`) in the same drop-in if you must serve files from there.

**Serving on port 443.** Port 8443 is unprivileged, so the service needs no capabilities. To bind 443 (or a
`redirect_listen` of `:80`) directly, set `"listen": ":443"` and install the shipped drop-in, which grants only
`CAP_NET_BIND_SERVICE`:

```bash
sudo install -D -m 0644 /usr/share/doc/ludworks/examples/bind-privileged-port.conf \
    /etc/systemd/system/ludworks.service.d/bind-privileged-port.conf
sudo systemctl daemon-reload
sudo systemctl restart ludworks
```

A [reverse proxy](configuration.md#reverse-proxy-and-trusted-proxies) in front of Ludworks is the alternative
that needs no capability at all.

### Upgrading and removing

```bash
sudo apt install ./ludworks_0.2.0_amd64.deb   # upgrade in place
sudo apt remove ludworks                       # stop and remove the program
sudo apt purge ludworks                        # also remove the configuration
```

* **Upgrade:** the old package stops the service; the new one restarts it if it was running before (a
  stopped service stays stopped). Database migrations run automatically on start. Take a
  [backup](../admin/maintenance.md#backups) first: downgrades after a migration are not supported.
* **Remove:** stops the service and removes the program. `/etc/ludworks`, `/var/lib/ludworks` and the
  `ludworks` account stay, so reinstalling continues where you left off.
* **Purge:** additionally deletes `/etc/ludworks`. The data directory `/var/lib/ludworks` (database, files,
  keys, backups) is **never** deleted by the package, and while it exists the `ludworks` user and group are
  kept as well, locked and expired. Deleting them would free their user and group IDs, the next system
  account created by any package would be given the same IDs, and with them ownership of all Ludworks data.
  Reinstalling reuses the kept account and continues where you left off. To remove everything, delete the
  data first: `sudo rm -rf /var/lib/ludworks` before `apt purge` removes the account too; after a purge,
  `sudo rm -rf /var/lib/ludworks && sudo deluser --system ludworks`.
* If `/var/lib/ludworks` belongs to **another** account when the package is installed (for example one that
  inherited the ID of a `ludworks` account deleted by an older package version), the package does not start
  the service and prints an error: that account could have read or changed the data. Check the data or
  restore it from a backup you trust, then `sudo chown -R ludworks:ludworks /var/lib/ludworks` and
  `sudo systemctl enable --now ludworks`. A directory whose owner no longer exists is handed to the new
  `ludworks` account automatically.

## Windows

Ludworks for Windows is a single `ludworks.exe` (with an embedded version resource and icon). It runs as a
Windows service under its own least-privilege virtual account `NT SERVICE\Ludworks`, or in a console window.

### Installer (recommended)

Run `ludworks_<version>_windows_amd64_setup.exe` and confirm the administrator prompt. The installer

* copies the program to `C:\Program Files\Ludworks` (only administrators can change files there),
* registers the **Ludworks** service (automatic delayed start, restart after a crash) and starts it,
* optionally (task "Allow other devices on the local network (same subnet) to connect") adds the inbound
  firewall rule "Ludworks (HTTPS 8443)" for TCP 8443 on private and domain networks, limited to remote
  addresses on the local subnet (see [Windows Firewall](#windows-firewall)),
* offers to show the setup token and to open `https://localhost:8443/` when it finishes.

The install folder cannot be changed. The service runs `ludworks.exe` from it, and the uninstaller runs
elevated from it, so it must be a folder only administrators can change; a folder such as `C:\Ludworks` or
`D:\Ludworks` inherits write access for every signed-in user from the drive root, which would let any of them
replace the program and run code as the service. The installer therefore refuses `/DIR=` with any other folder,
and it refuses to upgrade a **Ludworks** service registered for a program in another folder (an install by an
older version in a custom folder, or a manual `service install`). In that case remove the old service with
`sc.exe stop Ludworks` and `sc.exe delete Ludworks` in an elevated prompt (do not run programs from that
folder: other users may have changed them), delete the folder, and run the installer again; your data is kept.

Running a newer installer upgrades in place: it stops the service, replaces the program and starts it again.
Uninstall from *Settings → Apps*: the service and the firewall rule are removed, and the data directory
`C:\ProgramData\Ludworks` is **kept**. Silent installs work as usual for Inno Setup:
`ludworks_0.1.0_windows_amd64_setup.exe /VERYSILENT /SUPPRESSMSGBOXES /TASKS=firewall`.

### Windows Firewall

The rule "Ludworks (HTTPS 8443)" allows inbound TCP 8443 to `ludworks.exe` only from the local subnet (the
LAN's IPv4 subnet and its on-link IPv6 prefixes) and only on private and domain networks. Addresses that merely
reach such an interface, such as internet hosts over global IPv6, VPN peers or port-forwarded traffic, are not
allowed. Upgrades keep the rule limited to the local subnet, including a rule created by an older version.

To allow other networks on purpose, for example a Tailscale or WireGuard range, add a rule of your own next to
it; upgrades and uninstalls leave rules with other names alone:

```powershell
New-NetFirewallRule -DisplayName 'Ludworks (HTTPS 8443, VPN)' -Direction Inbound -Action Allow -Protocol TCP `
  -LocalPort 8443 -Program 'C:\Program Files\Ludworks\ludworks.exe' -RemoteAddress 100.64.0.0/10 -Profile Private, Domain
Remove-NetFirewallRule -DisplayName 'Ludworks (HTTPS 8443, VPN)'   # to remove it again
```

### Zip with the PowerShell scripts

Extract `ludworks_<version>_windows_amd64.zip`, open PowerShell **as administrator** in that folder and run:

```powershell
powershell -ExecutionPolicy Bypass -File .\install-service.ps1 -AddFirewallRule
```

`install-service.ps1` checks that it runs elevated, copies the files to `C:\Program Files\Ludworks`, runs
`ludworks.exe service install` (or stops the existing service for an upgrade), starts the service, adds the
firewall rule (local subnet only, see [Windows Firewall](#windows-firewall)) when you pass `-AddFirewallRule`,
and prints where the setup token is. Like the installer, it refuses to upgrade a **Ludworks** service that runs
a program from any other folder. Remove everything except your data with:

```powershell
powershell -ExecutionPolicy Bypass -File "C:\Program Files\Ludworks\uninstall-service.ps1"
```

### After installing

Read the one-time setup token in an elevated PowerShell (the data directory is restricted to SYSTEM,
Administrators and the service account):

```powershell
Get-Content C:\ProgramData\Ludworks\setup-token.txt
```

Open `https://localhost:8443/` and complete [first-time setup](first-run.md). The service writes its log, including
the TLS certificate fingerprint, to `C:\ProgramData\Ludworks\ludworks.log` (rotated at 10 MiB). Put settings into
`C:\ProgramData\Ludworks\config.json` (start from `config.example.json` in the program folder, see
[Configuration](configuration.md)) and restart the service with `Restart-Service Ludworks`.

### Managing the service by hand

`ludworks.exe service install`, `start`, `stop` and `uninstall` (from an elevated prompt) do the same as the
scripts. `service install` also accepts `--data`, `--config`, `--listen` and `--tls`, which are stored in the
service's command line, and locks the data directory down to the service account, SYSTEM and Administrators:

```powershell
& "C:\Program Files\Ludworks\ludworks.exe" service install --data D:\Ludworks --listen :9443
& "C:\Program Files\Ludworks\ludworks.exe" service start
```

Keep `ludworks.exe` in a folder that only administrators can write to (such as `C:\Program Files`): whoever can
replace the binary can run code as the service. Never register the service from a folder like Downloads, the
Desktop or a new folder at the root of a drive; the installer and `install-service.ps1` refuse to upgrade such
a registration.

### Try it in a console

```powershell
.\ludworks.exe serve
```

The data directory defaults to `C:\ProgramData\Ludworks` (created with an access list that only allows SYSTEM,
Administrators and the current user). The setup token is written to
`C:\ProgramData\Ludworks\setup-token.txt` (the console shows where). Stop the server with Ctrl+C. Use another directory or port with
`.\ludworks.exe serve --data D:\Ludworks --listen :9443`.

## Building from source

Ludworks is written in Go and builds as a fully static binary (no cgo). You need Go 1.27 or newer.

```bash
git clone https://github.com/gnubyte/ludworks-server.git
cd ludworks-server
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X ludworks/internal/version.Version=0.1.0-local" -o ludworks ./cmd/ludworks
./ludworks serve --data ./data --listen 127.0.0.1:8443
```

To build the release packages:

```bash
scripts/package.sh          # Linux or WSL; needs Go, python3, GNU sed and sha256sum
```

```powershell
.\scripts\package.ps1       # Windows; add -Installer to also build the Inno Setup installer
```

Both write `dist/ludworks_<version>_amd64.deb`, `dist/ludworks_<version>_arm64.deb`,
`dist/ludworks_<version>_windows_amd64.zip` and `dist/SHA256SUMS` (the version comes from the `VERSION` file).
The `.deb` files are assembled by the pure-Go `tools/mkdeb` (no `dpkg-deb` needed) and the Windows version
resource and icon by `tools/winres`. With the same commit, Go release and `SOURCE_DATE_EPOCH` (by default the
time of the last commit) the packages are byte-for-byte reproducible, so a build of a release tag can be
compared with that release's `SHA256SUMS`. See the [developer guide](../development/index.md#packaging).

## Command-line reference

| Command | Purpose |
|---|---|
| `ludworks serve [--data DIR] [--listen ADDR] [--tls MODE] [--config FILE] [--log-level LEVEL]` | Run the server |
| `ludworks version` | Print the version |
| `ludworks admin reset-password <user>` | Set a new password for a user (for example a locked-out admin); prompts twice without echo, or reads one line from stdin when scripted |
| `ludworks admin create-setup-token` | Issue a new one-time setup token while the server is not set up yet |
| `ludworks service install`, `uninstall`, `start`, `stop` | Manage the Windows service |

Run `admin` commands as the service user against the same configuration, for example on Linux:
`sudo -u ludworks ludworks admin reset-password alice` (on Windows: from an elevated prompt).
