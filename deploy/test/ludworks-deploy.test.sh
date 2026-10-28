#!/bin/bash
# Tests for deploy/ludworks-deploy, run unprivileged: the script works on a temporary tree
# (LUDWORKS_DEPLOY_TEST_ROOT) with fake systemctl/dpkg/dpkg-query/curl on PATH and real .deb files
# built with dpkg-deb. Needs bash, dpkg-deb, GNU coreutils/findutils, flock.
#
#   bash deploy/test/ludworks-deploy.test.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
script="$here/../ludworks-deploy"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fails=0
pass() { printf 'ok   %s\n' "$1"; }
fail() {
	printf 'FAIL %s\n' "$1"
	fails=$((fails + 1))
}

# ------------------------------------------------------------------------------------ fake tools

fakebin="$tmp/bin"
mkdir -p "$fakebin"

# The fake service: "start" runs the installed program's health line (# health=ok|broken) and, for
# "# migrate=yes", changes the database like a schema migration would.
cat >"$fakebin/systemctl" <<'EOF'
#!/bin/bash
R="$LUDWORKS_DEPLOY_TEST_ROOT"
case "$1" in
stop) rm -f "$R/run/fake-running"; echo stop >>"$R/run/fake-calls" ;;
start)
	echo start >>"$R/run/fake-calls"
	prog="$R/usr/bin/ludworks"
	if grep -q '^# migrate=yes' "$prog"; then echo migrated >>"$R/var/lib/ludworks/ludworks.db"; fi
	if grep -q '^# health=ok' "$prog"; then "$prog" version | awk '{print $2}' >"$R/run/fake-running"; fi
	;;
daemon-reload) ;;
is-active) [[ -f "$R/run/fake-running" ]] && echo active || echo inactive ;;
show) echo /usr/lib/systemd/system/ludworks.service ;;
*) echo "fake systemctl: $*" >&2; exit 1 ;;
esac
EOF
cat >"$fakebin/curl" <<'EOF'
#!/bin/bash
R="$LUDWORKS_DEPLOY_TEST_ROOT"
[[ -f "$R/run/fake-running" ]] || exit 7
printf '{"status":"ok","time":"2026-01-01T00:00:00Z","version":"%s"}' "$(cat "$R/run/fake-running")"
EOF
cat >"$fakebin/dpkg" <<'EOF'
#!/bin/bash
R="$LUDWORKS_DEPLOY_TEST_ROOT"
case "$1" in
--print-architecture) echo amd64 ;;
-i)
	deb="${@: -1}"
	dpkg-deb --fsys-tarfile "$deb" | tar -xOf - ./usr/bin/ludworks >"$R/usr/bin/ludworks"
	chmod 0755 "$R/usr/bin/ludworks"
	dpkg-deb -f "$deb" Version >"$R/var/lib/fake-dpkg-version"
	;;
*) echo "fake dpkg: $*" >&2; exit 1 ;;
esac
EOF
cat >"$fakebin/dpkg-query" <<'EOF'
#!/bin/bash
cat "$LUDWORKS_DEPLOY_TEST_ROOT/var/lib/fake-dpkg-version"
EOF
chmod +x "$fakebin"/*

# A fake ludworks program reporting version $1, with health $2 and migration flag $3.
program() {
	printf '#!/bin/sh\n# health=%s\n# migrate=%s\n[ "$1" = version ] && echo "ludworks %s"\nexit 0\n' "$2" "$3" "$1"
}

# A real .deb with that program (arch $4, default amd64).
build_deb() { # version health migrate [arch] -> path
	local v="$1" stage="$tmp/stage-$1-$2-${4:-amd64}"
	rm -rf "$stage"
	mkdir -p "$stage/DEBIAN" "$stage/usr/bin"
	program "$v" "$2" "$3" >"$stage/usr/bin/ludworks"
	chmod 0755 "$stage/usr/bin/ludworks"
	printf 'Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: test <t@example.com>\nDescription: test\n' \
		"${PKG:-ludworks}" "${v//-/\~}" "${4:-amd64}" >"$stage/DEBIAN/control"
	dpkg-deb --root-owner-group --build "$stage" "$tmp/$v-$2-${4:-amd64}.deb" >/dev/null
	printf '%s\n' "$tmp/$v-$2-${4:-amd64}.deb"
}

# A fresh installation running 0.1.0, in MODE $1.
setup_root() {
	R="$tmp/root"
	rm -rf "$R"
	mkdir -p "$R/etc" "$R/usr/bin" "$R/usr/lib/systemd/system" "$R/var/lib/ludworks" "$R/var/lib/ludworks-deploy" "$R/run/lock"
	program 0.1.0 ok no >"$R/usr/bin/ludworks"
	chmod 0755 "$R/usr/bin/ludworks"
	echo "[Service]" >"$R/usr/lib/systemd/system/ludworks.service"
	echo "original-db" >"$R/var/lib/ludworks/ludworks.db"
	echo 0.1.0 >"$R/var/lib/fake-dpkg-version"
	echo 0.1.0 >"$R/run/fake-running"
	cat >"$R/etc/ludworks-deploy.conf" <<EOF
# test
MODE=$1
UNIT=ludworks.service
BINARY=/usr/bin/ludworks
DATA_DIR=/var/lib/ludworks
HEALTH_URL=https://127.0.0.1:8444/api/v1/health
EOF
}

run() { # SSH_ORIGINAL_COMMAND [stdin file]
	local in="${2:-/dev/null}"
	env PATH="$fakebin:$PATH" LUDWORKS_DEPLOY_TEST_ROOT="$R" LUDWORKS_DEPLOY_TEST_HEALTH_TIMEOUT=2 \
		SSH_ORIGINAL_COMMAND="$1" bash "$script" <"$in" >"$tmp/out" 2>"$tmp/err"
}

sha() { sha256sum "$1" | cut -d' ' -f1; }

# ------------------------------------------------------------------------------------------ tests

for mode in binary deb; do
	setup_root "$mode"
	if run status && grep -qx "mode=$mode" "$tmp/out" && grep -qx 'arch=amd64' "$tmp/out" &&
		grep -qx 'running_version=0.1.0' "$tmp/out" && grep -qx 'active=active' "$tmp/out"; then
		pass "$mode: status"
	else
		fail "$mode: status"; cat "$tmp/out" "$tmp/err"
	fi

	deb="$(build_deb 0.2.0-rc.1 ok yes)"
	if run "deploy $(sha "$deb")" "$deb" && grep -qx 'running_version=0.2.0-rc.1' <(run status && cat "$tmp/out"); then
		snap="$(find "$R/var/lib/ludworks-deploy/backups" -mindepth 1 -maxdepth 1 | head -n 1)"
		if [[ "$(cat "$snap/ludworks.db")" == original-db ]] && grep -q 'ludworks 0.1.0' <(sh "$snap/program" version) &&
			[[ -f "$R/var/lib/ludworks-deploy/releases/ludworks_0.2.0~rc.1_amd64.deb" ]] &&
			grep -q ' 0.2.0-rc.1 ' "$R/var/lib/ludworks-deploy/last-deploy" &&
			grep -q '^migrated$' "$R/var/lib/ludworks/ludworks.db"; then
			pass "$mode: deploy installs, keeps the previous program and database, records the release"
		else
			fail "$mode: deploy bookkeeping"; ls -R "$R/var/lib/ludworks-deploy"; cat "$tmp/err"
		fi
	else
		fail "$mode: deploy"; cat "$tmp/err"
	fi
	if [[ "$mode" == deb ]] && [[ "$(cat "$R/var/lib/fake-dpkg-version")" == "0.2.0~rc.1" ]]; then
		pass "deb: installed through dpkg"
	elif [[ "$mode" == deb ]]; then
		fail "deb: installed through dpkg"
	fi
done

# An unhealthy release that migrated the database is rolled back: program, unit and database.
setup_root binary
deb="$(build_deb 0.3.0 broken yes)"
if run "deploy $(sha "$deb")" "$deb"; then
	fail "unhealthy release: deploy should fail"
else
	if grep -q 'ludworks 0.1.0' <(sh "$R/usr/bin/ludworks" version) &&
		[[ "$(cat "$R/var/lib/ludworks/ludworks.db")" == original-db ]] &&
		[[ "$(cat "$R/run/fake-running")" == 0.1.0 ]] && grep -q 'rollback done' "$tmp/err"; then
		pass "unhealthy release: rolled back to the previous program and database, running again"
	else
		fail "unhealthy release: rollback"; cat "$tmp/err"
	fi
fi

# Rejected before anything is touched: checksum, package name, architecture, garbage.
setup_root binary
deb="$(build_deb 0.4.0 ok no)"
expect_rejected() { # name command stdin pattern
	rm -f "$R/run/fake-calls"
	if run "$2" "$3"; then
		fail "$1: accepted"
	elif grep -q "$4" "$tmp/err" && [[ ! -f "$R/run/fake-calls" ]] && [[ "$(cat "$R/run/fake-running")" == 0.1.0 ]]; then
		pass "$1: rejected, service untouched"
	else
		fail "$1"; cat "$tmp/err"
	fi
}
expect_rejected "checksum mismatch" "deploy $(printf '0%.0s' {1..64})" "$deb" "SHA-256 mismatch"
other="$(PKG=evil build_deb 0.4.1 ok no)"
expect_rejected "other package" "deploy $(sha "$other")" "$other" "not ludworks"
arm="$(build_deb 0.4.2 ok no arm64)"
expect_rejected "wrong architecture" "deploy $(sha "$arm")" "$arm" "is for arm64"
echo "not a deb" >"$tmp/junk"
expect_rejected "not a package" "deploy $(sha "$tmp/junk")" "$tmp/junk" "not a Debian package"
: >"$tmp/empty"
expect_rejected "empty upload" "deploy $(sha "$tmp/empty")" "$tmp/empty" "no package received"

# The forced command accepts nothing but the two commands.
for cmd in "status; id" "deploy \$(id)" "deploy ../../etc/passwd" "rollback" "bash" ""; do
	rm -f "$R/run/fake-calls"
	if run "$cmd" && [[ -n "$cmd" ]]; then
		fail "command '$cmd' accepted"
	elif grep -q usage "$tmp/err" && [[ ! -f "$R/run/fake-calls" ]]; then
		pass "command '$cmd' refused"
	else
		fail "command '$cmd'"; cat "$tmp/err"
	fi
done

# The settings file is parsed, never sourced.
setup_root binary
echo 'HEALTH_URL=$(touch /tmp/pwned)' >>"$R/etc/ludworks-deploy.conf"
if ! run status && grep -q "unsafe value" "$tmp/err"; then pass "unsafe settings refused"; else fail "unsafe settings"; fi

# One deploy at a time.
setup_root binary
deb="$(build_deb 0.5.0 ok no)"
exec 8>"$R/run/lock/ludworks-deploy.lock"
flock -n 8
if ! run "deploy $(sha "$deb")" "$deb" && grep -q "another deploy is running" "$tmp/err"; then
	pass "concurrent deploy refused"
else
	fail "concurrent deploy"; cat "$tmp/err"
fi
exec 8>&-

# Old releases and backups are pruned to the newest five.
setup_root binary
for i in 1 2 3 4 5 6 7; do
	deb="$(build_deb "0.6.$i" ok no)"
	run "deploy $(sha "$deb")" "$deb" || { fail "prune: deploy 0.6.$i"; cat "$tmp/err"; }
	sleep 1 # distinct timestamps
done
n_rel="$(find "$R/var/lib/ludworks-deploy/releases" -name '*.deb' | wc -l)"
n_bak="$(find "$R/var/lib/ludworks-deploy/backups" -mindepth 1 -maxdepth 1 | wc -l)"
if [[ "$n_rel" -eq 5 && "$n_bak" -eq 5 ]] && [[ -f "$R/var/lib/ludworks-deploy/releases/ludworks_0.6.7_amd64.deb" ]]; then
	pass "pruned to five releases and five backups"
else
	fail "prune: $n_rel releases, $n_bak backups"
fi

if ((fails)); then
	echo "$fails test(s) failed"
	exit 1
fi
echo "all tests passed"
