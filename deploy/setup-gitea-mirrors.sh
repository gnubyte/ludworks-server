#!/bin/bash
# One-time setup on the Gitea side (docs/admin/continuous-deployment.md). Run it on your own computer
# (Linux, macOS, WSL or Git Bash; needs curl, openssl, ssh-keygen, ssh-keyscan):
#
#   bash setup-gitea-mirrors.sh --gitea https://gitea.example.lan --deploy-host 192.0.2.10
#
# It asks for two tokens (typed by you, never stored by this script or shown on screen):
#   * a Gitea access token  — Settings → Applications → Generate token, scopes
#                              "repository: Read and Write" and "user: Read"
#   * a GitHub token        — fine-grained, only the Ludworks repositories, "Contents: Read-only"
#                              (Gitea uses it to pull; renew it in the mirror settings before it expires)
# and then:
#   1. pins the Gitea server's TLS key after you confirm its fingerprint (trust on first use)
#   2. creates a private pull mirror of every repository (synced every --interval, default 10m);
#      existing repositories are left alone
#   3. turns Gitea Actions on for the server repository (its .gitea/workflows/deploy.yml deploys) and
#      off for the client repositories (their GitHub workflows need runners Gitea does not have)
#   4. creates the CI deploy key (ed25519, --key, default ~/.ssh/ludworks-ci) if it does not exist,
#      pins the deploy host's SSH host key after you confirm it, and stores DEPLOY_SSH_KEY and
#      DEPLOY_KNOWN_HOSTS as Actions secrets and DEPLOY_HOST/DEPLOY_PORT/DEPLOY_USER as variables
#   5. prints the public key for setup-deploy-target.sh
set -euo pipefail

gitea="" deploy_host="" deploy_port=22 deploy_user=ludworks-deploy interval=10m
github_owner=gnubyte owner="" key_file="$HOME/.ssh/ludworks-ci" yes=0
repos=(ludworks-server ludworks-python ludworks-browser ludworks-android ludworks-desktop)
server_repo=ludworks-server

die() {
	echo "setup-gitea-mirrors: $*" >&2
	exit 1
}
usage() {
	sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'
	exit "${1:-0}"
}

while (($#)); do
	case "$1" in
	--gitea) gitea="${2%/}"; shift 2 ;;
	--deploy-host) deploy_host="$2"; shift 2 ;;
	--deploy-port) deploy_port="$2"; shift 2 ;;
	--deploy-user) deploy_user="$2"; shift 2 ;;
	--owner) owner="$2"; shift 2 ;;
	--github-owner) github_owner="$2"; shift 2 ;;
	--interval) interval="$2"; shift 2 ;;
	--key) key_file="$2"; shift 2 ;;
	--yes) yes=1; shift ;;
	-h | --help) usage 0 ;;
	*) echo "unknown argument: $1" >&2; usage 2 ;;
	esac
done
[[ "$gitea" =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?$ ]] || die "--gitea https://host[:port] is required (HTTPS only)"
[[ "$deploy_host" =~ ^[A-Za-z0-9.:-]+$ ]] || die "--deploy-host is required"
[[ "$deploy_port" =~ ^[0-9]+$ && "$deploy_user" =~ ^[a-z_][a-z0-9_-]*$ ]] || die "bad --deploy-port or --deploy-user"
[[ "$interval" =~ ^[0-9]+[hm]$ ]] || die "--interval like 10m or 1h"
for tool in curl openssl ssh-keygen ssh-keyscan; do command -v "$tool" >/dev/null || die "$tool is required"; done

confirm() {
	((yes)) && return 0
	local answer
	read -r -p "$1 [y/N] " answer
	[[ "$answer" == [yY]* ]]
}

tmp="$(mktemp -d)"
chmod 700 "$tmp"
trap 'rm -rf "$tmp"' EXIT

# JSON string literal (the values here are tokens, names and PEM text).
json() {
	local s="$1"
	s="${s//\\/\\\\}"
	s="${s//\"/\\\"}"
	s="${s//$'\n'/\\n}"
	s="${s//$'\r'/}"
	s="${s//$'\t'/\\t}"
	printf '"%s"' "$s"
}

# ---------------------------------------------------------------------------------- 1. TLS pinning

hostport="${gitea#https://}"
host="${hostport%%:*}"
port=443
[[ "$hostport" == *:* ]] && port="${hostport##*:}"
openssl s_client -connect "$host:$port" -servername "$host" </dev/null 2>/dev/null |
	openssl x509 >"$tmp/gitea.pem" 2>/dev/null || die "cannot read the TLS certificate of $gitea"
fingerprint="$(openssl x509 -in "$tmp/gitea.pem" -noout -fingerprint -sha256 | cut -d= -f2)"
pin="$(openssl x509 -in "$tmp/gitea.pem" -noout -pubkey | openssl pkey -pubin -outform der |
	openssl dgst -sha256 -binary | openssl enc -base64)"
echo "Gitea at $gitea presents a certificate for: $(openssl x509 -in "$tmp/gitea.pem" -noout -subject | sed 's/^subject=//')"
echo "  SHA-256 fingerprint: $fingerprint"
echo "  (on the Gitea host: openssl x509 -in <its certificate> -noout -fingerprint -sha256)"
confirm "Is this your Gitea server's certificate?" || die "cancelled"

# ------------------------------------------------------------------------------------------ tokens

read -r -s -p "Gitea access token: " gitea_token
echo
[[ "$gitea_token" =~ ^[A-Za-z0-9_]+$ ]] || die "that does not look like a Gitea token"
read -r -s -p "GitHub token (read-only contents of the $github_owner repositories): " github_token
echo
[[ "$github_token" =~ ^[A-Za-z0-9_]+$ ]] || die "that does not look like a GitHub token"
printf 'Authorization: token %s\n' "$gitea_token" >"$tmp/auth"
unset gitea_token

# api METHOD PATH [BODY-FILE] -> HTTP status in $status, body in $tmp/body (not a subshell, so
# $status reaches the caller). The token travels in a header file, never on the command line; the
# server must present the pinned key.
status=""
api() {
	local args=(-sS -k --pinnedpubkey "sha256//$pin" -H @"$tmp/auth" -H 'Accept: application/json'
		-X "$1" -o "$tmp/body" -w '%{http_code}' --max-time 300)
	[[ -n "${3-}" ]] && args+=(-H 'Content-Type: application/json' --data-binary @"$3")
	status="$(curl "${args[@]}" "$gitea/api/v1$2")" || die "request to $gitea failed (certificate changed?)"
}

api GET /user
[[ "$status" == 200 ]] || die "the Gitea token was refused ($status)"
user_json="$(cat "$tmp/body")"
login="$(sed -n 's/.*"login"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' <<<"$user_json")"
[[ "$login" =~ ^[A-Za-z0-9._-]+$ ]] || die "could not read the Gitea user name"
owner="${owner:-$login}"
[[ "$owner" =~ ^[A-Za-z0-9._-]+$ ]] || die "bad --owner"
echo "Signed in to Gitea as $login; mirrors go to $owner."

# --------------------------------------------------------------------------------------- 2. mirrors

for repo in "${repos[@]}"; do
	api GET "/repos/$owner/$repo"
	if [[ "$status" == 200 ]]; then
		if grep -Eq '"mirror"[[:space:]]*:[[:space:]]*true' "$tmp/body"; then
			echo "  $owner/$repo: mirror exists"
		else
			echo "  $owner/$repo: exists but is NOT a mirror — left alone" >&2
		fi
		continue
	fi
	{
		printf '{"clone_addr":%s,' "$(json "https://github.com/$github_owner/$repo.git")"
		printf '"repo_name":%s,"repo_owner":%s,' "$(json "$repo")" "$(json "$owner")"
		printf '"service":"github","auth_token":%s,' "$(json "$github_token")"
		printf '"mirror":true,"mirror_interval":%s,"private":true,' "$(json "$interval")"
		printf '"description":%s,' "$(json "Mirror of github.com/$github_owner/$repo")"
		printf '"wiki":false,"issues":false,"pull_requests":false,"releases":false,"labels":false,"milestones":false,"lfs":false}'
	} >"$tmp/migrate.json"
	api POST /repos/migrate "$tmp/migrate.json"
	rm -f "$tmp/migrate.json"
	[[ "$status" == 201 ]] || die "creating the mirror $owner/$repo failed ($status): $(head -c 300 "$tmp/body")"
	echo "  $owner/$repo: mirror created (syncs every $interval)"
done
unset github_token

# ------------------------------------------------------------------------------- 3. Actions per repo

for repo in "${repos[@]}"; do
	on=false
	[[ "$repo" == "$server_repo" ]] && on=true
	printf '{"has_actions":%s}' "$on" >"$tmp/edit.json"
	api PATCH "/repos/$owner/$repo" "$tmp/edit.json"
	[[ "$status" == 200 ]] || die "setting Actions on $owner/$repo failed ($status)"
done
echo "Gitea Actions: on for $server_repo, off for the client repositories."

# -------------------------------------------------------------------------------- 4. deploy secrets

if [[ ! -f "$key_file" ]]; then
	mkdir -p "$(dirname "$key_file")"
	ssh-keygen -q -t ed25519 -N "" -C "ludworks-ci@$host" -f "$key_file"
	echo "Created the deploy key $key_file (keep it private; Gitea holds a copy as a secret)."
fi
chmod 600 "$key_file"

ssh-keyscan -T 10 -p "$deploy_port" -t ed25519 "$deploy_host" 2>/dev/null >"$tmp/known_hosts"
[[ -s "$tmp/known_hosts" ]] || die "cannot read the SSH host key of $deploy_host:$deploy_port"
echo "Deploy host $deploy_host:$deploy_port has SSH host key:"
ssh-keygen -lf "$tmp/known_hosts" | sed 's/^/  /'
echo "  (setup-deploy-target.sh prints the same fingerprint on that machine)"
confirm "Pin this host key for deployments?" || die "cancelled"

set_secret() { # name file
	printf '{"data":%s}' "$(json "$(cat "$2")")" >"$tmp/secret.json"
	api PUT "/repos/$owner/$server_repo/actions/secrets/$1" "$tmp/secret.json"
	rm -f "$tmp/secret.json"
	[[ "$status" == 201 || "$status" == 204 ]] || die "storing the secret $1 failed ($status)"
}
set_variable() { # name value
	printf '{"value":%s}' "$(json "$2")" >"$tmp/var.json"
	api POST "/repos/$owner/$server_repo/actions/variables/$1" "$tmp/var.json"
	if [[ "$status" != 201 && "$status" != 204 ]]; then
		api PUT "/repos/$owner/$server_repo/actions/variables/$1" "$tmp/var.json"
	fi
	[[ "$status" == 201 || "$status" == 204 ]] || die "storing the variable $1 failed ($status)"
}
set_secret DEPLOY_SSH_KEY "$key_file"
set_secret DEPLOY_KNOWN_HOSTS "$tmp/known_hosts"
set_variable DEPLOY_HOST "$deploy_host"
set_variable DEPLOY_PORT "$deploy_port"
set_variable DEPLOY_USER "$deploy_user"
echo "Stored the deploy key, the pinned host key and DEPLOY_HOST/PORT/USER on $owner/$server_repo."

# ------------------------------------------------------------------------------------- 5. next step

cp "$key_file.pub" ./ludworks-ci.pub
cat <<EOF

Next, on $deploy_host (as a user with sudo):
  1. copy deploy/ludworks-deploy, deploy/setup-deploy-target.sh and ./ludworks-ci.pub there, e.g.
       scp deploy/ludworks-deploy deploy/setup-deploy-target.sh ludworks-ci.pub <you>@$deploy_host:
  2. run: sudo bash setup-deploy-target.sh --key-file ludworks-ci.pub
     (add --from <runner address> to accept the key only from your Actions runner)
Then run the "Deploy" workflow once from $gitea/$owner/$server_repo/actions, or wait for the next sync.
EOF
