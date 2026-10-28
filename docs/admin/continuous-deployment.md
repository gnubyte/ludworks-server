# Continuous deployment from Gitea

If you keep a Gitea server that mirrors the Ludworks repositories from GitHub, it can deploy every
change of the server repository to your Ludworks machine on its own, safely:

```text
GitHub ──pull mirror (every 10 min)──▶ Gitea ──push event──▶ Actions runner
                                                              │ test, build the .deb
                                                              ▼
                                   ssh (deploy key, forced command) ──▶ Ludworks machine
                                                                         ludworks-deploy:
                                                                         verify → back up → install
                                                                         → health check → or roll back
```

* **Mirrors.** Gitea pulls every repository (`ludworks-server`, `-python`, `-browser`, `-android`,
  `-desktop`) from GitHub on a schedule. A sync that moves `main` of `ludworks-server` is a push event, and
  `.gitea/workflows/deploy.yml` runs (GitHub ignores that directory).
* **Every deploy is tested first:** `go vet`, all Go tests, the web UI tests and the API and docs checks
  must pass, then the packages are built exactly like a release (`scripts/package.sh`).
* **The deploy key can do one thing.** On the Ludworks machine its only command is
  `sudo -n /usr/local/sbin/ludworks-deploy` (`restrict`: no shell, no forwarding). It runs as the
  password-less user `ludworks-deploy`, and one sudo rule allows exactly that script. CI pins the
  machine's SSH host key.
* **Safe updates.** `ludworks-deploy` checks the package's SHA-256, name and architecture and that the new
  program runs. It keeps the current program, unit file and database, stops the service only for the
  database copy and the install, then waits until `/api/v1/health` reports the new version. If anything
  fails after the service stopped, it puts the previous program, unit file and database back and starts
  them again, and the workflow fails. It keeps the last five releases and backups in
  `/var/lib/ludworks-deploy`. Only one deploy runs at a time.

## Requirements

* A current Gitea (this was written against 1.27) with Actions enabled and a runner with the
  `ubuntu-latest` label (the default of `act_runner`). The runner must reach the internet (Go, Node and the
  pinned GitHub actions are downloaded) and the Ludworks machine over SSH.
* The Ludworks machine runs Debian or Ubuntu with Ludworks installed from the `.deb` (or as a systemd
  service running a `ludworks` binary), plus `sudo`, `curl` and `python3`.

## One-time setup

You do these steps yourself, because they need your passwords and tokens.

### 1. Gitea: mirrors and deploy secrets

Create two tokens:

* **Gitea:** *Settings → Applications → Generate New Token* with the scopes *repository: Read and Write* and
  *user: Read*.
* **GitHub:** a [fine-grained token](https://github.com/settings/personal-access-tokens) limited to the
  Ludworks repositories with *Contents: Read-only*. Gitea keeps it to pull. When it expires, put a new one
  in each mirror's settings.

Then, on your computer, from a checkout of `ludworks-server`:

```sh
bash deploy/setup-gitea-mirrors.sh --gitea https://gitea.example.lan --deploy-host 192.0.2.10
```

The script asks you to confirm Gitea's TLS certificate (and pins it), asks for both tokens without showing
them, and then:

* creates the five private pull mirrors;
* turns Actions on for `ludworks-server` only (the client repositories' GitHub workflows need runners
  Gitea does not have);
* creates the deploy key `~/.ssh/ludworks-ci` and pins the Ludworks machine's SSH host key after you
  confirm its fingerprint;
* stores the secrets `DEPLOY_SSH_KEY` and `DEPLOY_KNOWN_HOSTS` and the variables `DEPLOY_HOST`,
  `DEPLOY_PORT` and `DEPLOY_USER`;
* writes the public key to `./ludworks-ci.pub`.

Options: `--owner` (a Gitea organisation instead of your user), `--interval` (sync interval),
`--deploy-port`, `--key`.

### 2. The Ludworks machine

Copy three files to it and run the setup with sudo:

```sh
scp deploy/ludworks-deploy deploy/setup-deploy-target.sh ludworks-ci.pub you@192.0.2.10:
ssh you@192.0.2.10
sudo bash setup-deploy-target.sh --key-file ludworks-ci.pub --from <runner address>
```

It finds the installation (service, program, configuration, data directory, health URL) and shows it with
the result of a health check. After you confirm, it installs `ludworks-deploy`, the `ludworks-deploy` user
with the restricted key, and the sudo rule. It then checks the whole chain and prints the machine's SSH host
key fingerprints: they must match the one you confirmed in step 1. `--from` (optional) accepts the key only
from your runner's address. Run the script again to change the key.

### 3. First deploy

In Gitea open *ludworks-server → Actions → Deploy → Run workflow*, or wait for the next sync that brings a
new commit. The log shows the tests, the target's status before and after, and every deploy step.

## Day to day

* **See what is installed:** `sudo ludworks-deploy status` on the machine (or `ssh ludworks-deploy@host
  status` with the deploy key).
* **A failed deploy** leaves the previous version running (the workflow log and
  `/var/lib/ludworks-deploy/deploy.log` say why). Fix the problem and push again.
* **Deploy history:** `/var/lib/ludworks-deploy/releases/` keeps the last five packages and `backups/<time>/`
  the program, unit file and database from before each deploy.
* **Going back by hand** to an older release:

  ```sh
  sudo systemctl stop ludworks
  sudo cp -a /var/lib/ludworks-deploy/backups/<time>/ludworks.db /var/lib/ludworks/   # only if the newer
  sudo dpkg -i /var/lib/ludworks-deploy/releases/ludworks_<version>_amd64.deb          # version migrated it
  sudo systemctl start ludworks
  ```

  A database copy is from the moment of that deploy: anything changed since is lost, so only restore one
  when the older version cannot open the current database.
* **Turn deploys off:** disable the workflow in Gitea (*Actions → Deploy → ⋯ → Disable*), or remove the key
  from `/var/lib/ludworks-deploy-user/.ssh/authorized_keys` on the machine.

## Security notes

* Whoever can push to `main` on GitHub (or run the workflow in Gitea) can deploy code to the machine.
  Protect the branch and the Gitea repository accordingly.
* The deploy key is stored as an encrypted Actions secret and is written to the runner only while the job
  runs. On the machine it is limited by the forced command, `restrict` and optionally `from=`. Its
  `authorized_keys` file belongs to root, so the deploy user cannot change it.
* `ludworks-deploy` treats its input as untrusted. It accepts only `status` and `deploy <sha256>`, reads its
  settings from `/etc/ludworks-deploy.conf` without executing them, caps the upload at 256 MiB and checks
  it before stopping anything.
* Installing a `.deb` runs its maintainer scripts as root; the workflow only ever sends packages it built
  from the mirrored commit.
