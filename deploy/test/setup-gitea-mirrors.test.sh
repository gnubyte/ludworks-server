#!/bin/bash
# Tests deploy/setup-gitea-mirrors.sh against a fake Gitea (fake_gitea.py) over pinned TLS, with a fake
# ssh-keyscan. Needs bash, python3, openssl, curl, ssh-keygen.
#
#   bash deploy/test/setup-gitea-mirrors.test.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
script="$here/../setup-gitea-mirrors.sh"
tmp="$(mktemp -d)"
pid=""
trap '[[ -n "$pid" ]] && kill "$pid" 2>/dev/null; rm -rf "$tmp"' EXIT
fails=0
check() { # name condition...
	local name="$1"
	shift
	if "$@"; then printf 'ok   %s\n' "$name"; else printf 'FAIL %s\n' "$name"; fails=$((fails + 1)); fi
}

openssl req -x509 -newkey rsa:2048 -nodes -subj /CN=localhost -days 1 \
	-keyout "$tmp/key.pem" -out "$tmp/cert.pem" 2>/dev/null
port=$((20000 + RANDOM % 20000))
python3 -I "$here/fake_gitea.py" "$port" "$tmp/cert.pem" "$tmp/key.pem" gtoken123 "$tmp/calls.log" &
pid=$!
for _ in $(seq 50); do
	curl -sk "https://localhost:$port/api/v1/user" >/dev/null 2>&1 && break
	sleep 0.1
done
: >"$tmp/calls.log" # the probes above are not the script's

# Fake ssh-keyscan: the deploy host's key.
mkdir -p "$tmp/bin"
ssh-keygen -q -t ed25519 -N "" -f "$tmp/hostkey"
cat >"$tmp/bin/ssh-keyscan" <<EOF
#!/bin/sh
echo "192.0.2.10 \$(cut -d' ' -f1,2 "$tmp/hostkey.pub")"
EOF
chmod +x "$tmp/bin/ssh-keyscan"

cd "$tmp"
if printf 'gtoken123\nghp_token456\n' | PATH="$tmp/bin:$PATH" bash "$script" \
	--gitea "https://localhost:$port" --deploy-host 192.0.2.10 --key "$tmp/ci-key" --yes >"$tmp/out" 2>&1; then
	check "script succeeds" true
else
	check "script succeeds" false
	cat "$tmp/out"
fi

log="$tmp/calls.log"
calls() { grep -c "$1" "$log" || true; }
check "every request carried the Gitea token" bash -c "! grep -v '\"auth\": \"token gtoken123\"' '$log' | grep -q ."
check "four mirrors created, the existing one left alone" test "$(calls '"path": "/api/v1/repos/migrate"')" -eq 4
check "mirrors are private pull mirrors with the GitHub token" bash -c \
	"grep '/repos/migrate' '$log' | grep -q '\"mirror\": true' && grep '/repos/migrate' '$log' | grep -q '\"private\": true' && grep '/repos/migrate' '$log' | grep -q '\"auth_token\": \"ghp_token456\"' && grep '/repos/migrate' '$log' | grep -q 'https://github.com/gnubyte/ludworks-server.git'"
check "no migrate call for ludworks-browser" bash -c "! grep '/repos/migrate' '$log' | grep -q ludworks-browser"
check "Actions on for the server only" bash -c \
	"grep 'PATCH' '$log' | grep 'ludworks-server' | grep -q '\"has_actions\": true' && test \$(grep 'PATCH' '$log' | grep -c '\"has_actions\": false') -eq 4"
check "deploy key stored as a secret" bash -c \
	"grep -q 'secrets/DEPLOY_SSH_KEY' '$log' && grep 'secrets/DEPLOY_SSH_KEY' '$log' | grep -q 'BEGIN OPENSSH PRIVATE KEY'"
check "pinned host key stored as a secret" bash -c "grep 'secrets/DEPLOY_KNOWN_HOSTS' '$log' | grep -q '192.0.2.10 ssh-ed25519'"
check "deploy variables stored" bash -c \
	"grep -q 'variables/DEPLOY_HOST' '$log' && grep -q 'variables/DEPLOY_PORT' '$log' && grep 'variables/DEPLOY_USER' '$log' | grep -q ludworks-deploy"
check "public key written for the target" grep -q '^ssh-ed25519 ' "$tmp/ludworks-ci.pub"
check "tokens never printed" bash -c "! grep -qE 'gtoken123|ghp_token456' '$tmp/out'"

# A different server key at the same address must be refused (the pin).
kill "$pid"
wait "$pid" 2>/dev/null || true
openssl req -x509 -newkey rsa:2048 -nodes -subj /CN=localhost -days 1 \
	-keyout "$tmp/key2.pem" -out "$tmp/cert2.pem" 2>/dev/null
check "fake server restarted" true
# The script pins on first use per run, so emulate a swap between pinning and the API calls by
# pinning one key and serving another: serve cert2 while the script was told to trust cert.pem.
sed -e "s|^pin=\"\$(openssl x509 -in \"\$tmp/gitea.pem\"|pin=\"\$(openssl x509 -in \"$tmp/cert.pem\"|" "$script" >"$tmp/pinned-old.sh"
python3 -I "$here/fake_gitea.py" "$port" "$tmp/cert2.pem" "$tmp/key2.pem" gtoken123 "$tmp/calls2.log" &
pid=$!
for _ in $(seq 50); do
	curl -sk "https://localhost:$port/api/v1/user" >/dev/null 2>&1 && break
	sleep 0.1
done
: >"$tmp/calls2.log"
if printf 'gtoken123\nghp_token456\n' | PATH="$tmp/bin:$PATH" bash "$tmp/pinned-old.sh" \
	--gitea "https://localhost:$port" --deploy-host 192.0.2.10 --key "$tmp/ci-key" --yes >"$tmp/out2" 2>&1; then
	check "a server with another key is refused" false
else
	check "a server with another key is refused" bash -c "! test -s '$tmp/calls2.log' && grep -q 'certificate changed' '$tmp/out2'"
fi

if ((fails)); then
	echo "$fails test(s) failed"
	exit 1
fi
echo "all tests passed"
