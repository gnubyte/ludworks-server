CI/CD wired 2026-10-28, mirror + deploy verified end-to-end 2026-10-30.

Flow: GitHub `main` -> Gitea pull mirror (every 10 min, read-only token) -> push event ->
`.gitea/workflows/deploy.yml` on the deploy-01 runner: full test suite, `scripts/package.sh`,
then the .deb goes over SSH to `/usr/local/sbin/ludworks-deploy` on deploy-01 (forced-command
key, sudo rule). ludworks-deploy verifies the package, keeps the previous program/database,
installs, health-checks /api/v1/health and rolls back on failure. Keeps last five releases in
/var/lib/ludworks-deploy.

Server: https://192.168.68.68:8444. See docs/admin/continuous-deployment.md for setup and day-2 ops.

The old update.yml (manual dpkg fallback that installed the committed 0.1.1 deb on every push)
was removed on 2026-10-30: it raced the Deploy pipeline and force-downgraded the server after
any newer deploy. Use the Deploy workflow (or a redeploy of an older commit) instead.
