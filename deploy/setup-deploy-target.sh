#!/bin/bash
# One-time setup of a machine that CI deploys Ludworks to (docs/admin/continuous-deployment.md).
# Run it ON that machine, with sudo, next to the ludworks-deploy script:
#
#   sudo bash setup-deploy-target.sh --key-file ludworks-ci.pub [--from 192.0.2.20] [--yes]
#
# It finds the existing Ludworks installation (the ludworks .deb, or a systemd service running a
# ludworks binary), shows what it found and, once confirmed:
#   * installs /usr/local/sbin/ludworks-deploy and writes /etc/ludworks-deploy.conf
#   * creates the system user ludworks-deploy (no password) whose only SSH key may run nothing but
#     `sudo -n /usr/local/sbin/ludworks-deploy` (forced command, no shell, no forwarding, optionally
#     only from the CI runner's address)
#   * allows exactly that command through /etc/sudoers.d/ludworks-deploy (checked with visudo)
#   * runs `ludworks-deploy status` as the deploy user to prove the chain works
#   * prints this machine's SSH host key fingerprints, to compare with the key CI pins
# Running it again updates everything in place (e.g. a new key).
set -euo pipefail

DEPLOY_USER=ludworks-deploy
DEPLOY_HOME=/var/lib/ludworks-deploy-user
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"

die() {
	echo "setup-deploy-target: $*" >&2
	exit 1
}

key_file="" from="" yes=0 health_override=""
while (($#)); do
	case "$1" in
	--key-file) key_file="${2-}"; shift 2 ;;
	--from) from="${2-}"; shift 2 ;;
	--health-url) health_override="${2-}"; shift 2 ;;
	--yes) yes=1; shift ;;
	*) die "unknown argument $1 (usage: --key-file FILE [--from ADDRESS] [--health-url URL] [--yes])" ;;
	esac
done
if [[ -n "$health_override" ]]; then
	[[ "$health_override" =~ ^https?://[^/]+/api/v1/health$ ]] || die "--health-url must look like https://127.0.0.1:8443/api/v1/health"
fi
[[ $EUID -eq 0 ]] || die "run with sudo"
[[ -f "$here/ludworks-deploy" ]] || die "ludworks-deploy must be in the same directory as this script"
[[ -n "$key_file" && -f "$key_file" ]] || die "--key-file: the CI deploy public key (ssh-ed25519 …) is required"
key="$(head -n 1 "$key_file" | tr -d '\r')"
[[ "$key" =~ ^ssh-ed25519\ [A-Za-z0-9+/]+={0,2}(\ [A-Za-z0-9@._-]+)?$ ]] || die "$key_file: expected one ssh-ed25519 public key line"
if [[ -n "$from" ]]; then
	[[ "$from" =~ ^[0-9A-Fa-f.:/,*]+$ ]] || die "--from: give IP addresses or CIDR ranges, comma separated"
fi
for tool in systemctl dpkg dpkg-deb curl flock visudo python3 sshd; do
	command -v "$tool" >/dev/null || die "$tool is required"
done

# ------------------------------------------------------------------------------ find the install

mode="" unit="" binary="" config="" data_dir=""
if dpkg-query -W -f='${Status}' ludworks 2>/dev/null | grep -q 'install ok installed'; then
	mode=deb
	unit=ludworks.service
fi
if [[ -z "$unit" ]]; then
	# A service whose program is a ludworks binary started with "serve".
	while read -r u _; do
		exec_start="$(systemctl show -p ExecStart --value "$u" 2>/dev/null || true)"
		if [[ "$exec_start" =~ argv\[\]=([^ ]*/ludworks)\ serve ]]; then
			unit="$u"
			break
		fi
	done < <(systemctl list-units --type=service --all --no-legend --plain 2>/dev/null)
	[[ -n "$unit" ]] && mode=binary
fi
[[ -n "$unit" ]] || die "no Ludworks installation found (neither the ludworks package nor a systemd service running 'ludworks serve')"

exec_start="$(systemctl show -p ExecStart --value "$unit")"
[[ "$exec_start" =~ argv\[\]=([^ ]+)\ ([^\;]*) ]] || die "cannot read ExecStart of $unit"
binary="${BASH_REMATCH[1]}"
args="${BASH_REMATCH[2]}"
listen_flag="" tls_flag=""
[[ "$args" =~ --config[=\ ]([^ ]+) ]] && config="${BASH_REMATCH[1]}"
[[ "$args" =~ --data[=\ ]([^ ]+) ]] && data_dir="${BASH_REMATCH[1]}"
[[ "$args" =~ --listen[=\ ]([^ ]+) ]] && listen_flag="${BASH_REMATCH[1]}"
[[ "$args" =~ --tls[=\ ]([^ ]+) ]] && tls_flag="${BASH_REMATCH[1]}"
[[ -z "$config" && "$mode" == deb ]] && config=/etc/ludworks/config.json

# Listen address, TLS mode and data directory from config.json (defaults as in the server).
read -r listen tls cfg_data < <(python3 -I - "$config" <<'EOF'
import json, sys
path = sys.argv[1]
c = {}
if path:
    try:
        with open(path) as f:
            c = json.load(f)
    except FileNotFoundError:
        pass
tls = c.get("tls", {})
mode = tls.get("mode", "self-signed") if isinstance(tls, dict) else str(tls)
print(c.get("listen") or ":8443", mode or "self-signed", c.get("data_dir") or "-")
EOF
)
[[ -z "$data_dir" && "$cfg_data" != "-" ]] && data_dir="$cfg_data"
[[ -z "$data_dir" ]] && data_dir=/var/lib/ludworks
# Command-line flags win over config.json, as in the server.
[[ -n "$listen_flag" ]] && listen="$listen_flag"
[[ -n "$tls_flag" ]] && tls="$tls_flag"
host="${listen%:*}"
port="${listen##*:}"
case "$host" in "" | 0.0.0.0 | "[::]" | "::") host=127.0.0.1 ;; esac
scheme=https
[[ "$tls" == off ]] && scheme=http
health_url="$scheme://$host:$port/api/v1/health"
[[ -n "$health_override" ]] && health_url="$health_override"
health="$(curl -fsS -k --max-time 5 "$health_url" 2>/dev/null || true)"

[[ -x "$binary" ]] || die "$binary (from $unit) is not an executable"
[[ -f "$data_dir/ludworks.db" ]] || die "no database at $data_dir/ludworks.db"

cat <<EOF
Found Ludworks:
  install       $mode
  service       $unit
  program       $binary
  config        ${config:-(none)}
  data          $data_dir
  health check  $health_url -> ${health:-NOT RESPONDING}
Deploy user     $DEPLOY_USER (key: ${key##* }${from:+, only from $from})
EOF
[[ "$health" == *'"status":"ok"'* ]] || echo "warning: the server does not answer its health check right now; deploys wait for it" >&2
if ((!yes)); then
	read -r -p "Set up deployments for this installation? [y/N] " answer
	[[ "$answer" == [yY]* ]] || die "cancelled"
fi

# ---------------------------------------------------------------------------------------- set up

install -o root -g root -m 0755 "$here/ludworks-deploy" /usr/local/sbin/ludworks-deploy
conf_tmp="$(mktemp)"
cat >"$conf_tmp" <<EOF
# Written by setup-deploy-target.sh; read by /usr/local/sbin/ludworks-deploy.
MODE=$mode
UNIT=$unit
BINARY=$binary
DATA_DIR=$data_dir
HEALTH_URL=$health_url
EOF
install -o root -g root -m 0644 "$conf_tmp" /etc/ludworks-deploy.conf
rm -f "$conf_tmp"
install -d -o root -g root -m 0700 /var/lib/ludworks-deploy

if ! getent passwd "$DEPLOY_USER" >/dev/null; then
	useradd --system --home-dir "$DEPLOY_HOME" --create-home --shell /bin/sh --comment "Ludworks CI deployments" "$DEPLOY_USER"
fi
passwd -l "$DEPLOY_USER" >/dev/null # key only, never a password

# authorized_keys belongs to root, so the deploy user cannot change its own key or restrictions.
install -d -o root -g root -m 0755 "$DEPLOY_HOME/.ssh"
opts="restrict,command=\"sudo -n /usr/local/sbin/ludworks-deploy\""
[[ -n "$from" ]] && opts="$opts,from=\"$from\""
printf '%s %s\n' "$opts" "$key" >"$DEPLOY_HOME/.ssh/authorized_keys.new"
chown root:root "$DEPLOY_HOME/.ssh/authorized_keys.new"
chmod 0644 "$DEPLOY_HOME/.ssh/authorized_keys.new"
mv -f "$DEPLOY_HOME/.ssh/authorized_keys.new" "$DEPLOY_HOME/.ssh/authorized_keys"

sudoers_tmp="$(mktemp)"
cat >"$sudoers_tmp" <<EOF
# Ludworks CI deployments (setup-deploy-target.sh): the deploy key's forced command, nothing else.
Defaults!/usr/local/sbin/ludworks-deploy env_keep += "SSH_ORIGINAL_COMMAND"
$DEPLOY_USER ALL=(root) NOPASSWD: /usr/local/sbin/ludworks-deploy
EOF
visudo -cqf "$sudoers_tmp" || die "the sudoers rule did not validate (nothing was changed in /etc/sudoers.d)"
install -o root -g root -m 0440 "$sudoers_tmp" /etc/sudoers.d/ludworks-deploy
rm -f "$sudoers_tmp"

# sshd must let the new user in.
if sshd -T 2>/dev/null | grep -qiE '^(allowusers|allowgroups) '; then
	echo "warning: sshd_config has AllowUsers/AllowGroups; add $DEPLOY_USER there or CI cannot log in" >&2
fi

# ------------------------------------------------------------------------------------------ check

echo
echo "Checking the chain (deploy user -> sudo -> ludworks-deploy status):"
if ! su -s /bin/sh -c 'SSH_ORIGINAL_COMMAND=status sudo -n /usr/local/sbin/ludworks-deploy' "$DEPLOY_USER"; then
	die "ludworks-deploy status failed as $DEPLOY_USER"
fi
echo
echo "SSH host keys of this machine (CI pins one of these; compare with what setup-gitea-mirrors.sh showed):"
for f in /etc/ssh/ssh_host_*_key.pub; do ssh-keygen -lf "$f"; done
echo
echo "Done. Deploy with: ssh $DEPLOY_USER@<this host> status   (from the machine holding the CI key)"
