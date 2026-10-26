# Python SDK and CLI

The `ludworks` package (repository `ludworks-python`) is the official Python client: a typed SDK for scripts
and a `ludworks` command-line tool. Its core uses **only the Python standard library**; the password vault
needs the optional `cryptography` dependency.

```bash
pip install ludworks              # SDK + CLI (Python 3.10+)
pip install 'ludworks[vault]'     # + password vault support
```

The SDK's own README and docstrings are the complete reference; this page is a quick start.

## Connecting securely

With the default self-signed certificate, pin its fingerprint (trust on first use) instead of disabling
verification:

```python
import ludworks

fp = ludworks.fetch_fingerprint("https://nas.local:8443")
print(fp)   # compare with the fingerprint in the server log before trusting it!

with ludworks.Ludworks("https://nas.local:8443", fingerprint=fp) as lw:
    lw.login("alice", "correct horse battery staple", totp=lambda: input("2FA code: "))
    print(lw.whoami().username)
```

Other options: `ca_file="my-ca.pem"` for an internal CA, or nothing at all for a publicly trusted certificate.
`login()` uses token mode, so the client holds an `lws_` bearer token (`lw.token`). For unattended scripts use a
[personal access token](../api/authentication.md#personal-access-tokens) with minimal scopes:

```python
lw = ludworks.Ludworks("https://nas.local:8443", token="lwp_…", fingerprint=fp)
# or from LUDWORKS_URL, LUDWORKS_TOKEN, LUDWORKS_FINGERPRINT / LUDWORKS_CA_FILE:
lw = ludworks.Ludworks.from_env()
```

Errors are raised as exceptions mirroring the API error codes (`ludworks.NotFound`, `ludworks.Conflict`,
`ludworks.ValidationFailed`, `ludworks.RateLimited`, `ludworks.QuotaExceeded`, …), all subclasses of
`ludworks.LudworksError`. Rate-limited and transient failures are retried with back-off.

## Files

```python
# Resumable upload with progress; resumes automatically if the script is restarted.
entry = lw.files.upload("Night of the Living Dead (1968).mp4", path="Movies",
                        category="movie", subcategory="feature", tags=["horror"],
                        progress=lambda done, total: print(f"{done/total:.0%}", end="\r"))

lw.files.upload_dir("./Pictures/2026", path="Photos/2026", skip_existing=True)
for e in lw.files.iter_search("holiday", category="image"):
    print(e.path, e.size)
lw.files.download(entry.id, "./downloads/")        # Range-resumable, verifies SHA-256
```

## Notes

```python
note = lw.notes.create("# Shopping\n\n- [ ] coffee\n- [ ] HDMI cable", tags=["home"])
for n in lw.notes.search("rsync"):
    print(n["title"])
lw.notes.import_markdown("./obsidian-vault/")       # folder, .zip or .md
lw.notes.export("notes-backup.zip")
```

## TV lineups and channels

```python
from ludworks import LineupItem

lineup = lw.tv.lineups.create("Saturday Night Horror", items=[
    LineupItem.show("The Twilight Zone", order="random", limit=4),
    LineupItem.query(category="movie", genre="Horror", order="random", limit=1),
])
channel = lw.tv.channels.create("Fright TV", lineup["id"], shuffle=True, shuffle_mode="per_cycle")
print(lw.tv.channels.now(channel["id"])["program"]["title"])
```

## Sheets

```python
wb = lw.sheets.find("NAS budget") or lw.sheets.create("NAS budget")
lw.sheets.set_range(wb["id"], 0, "A1", [["Item", "Cost"], ["Disk", 189.99], ["Case", 74.5], ["Total", "=SUM(B2:B3)"]])
lw.sheets.style_range(wb["id"], 0, "A1:B1", b=True, bg="#e8f0fe")
print(lw.sheets.get_range(wb["id"], 0, "A1:B4"))
lw.sheets.export(wb["id"], "xlsx", "budget.xlsx")
```

Sheets can be referenced by name, ID or 0-based index.

## Password vault

```python
vault = lw.vault.unlock("my master passphrase")      # decrypts locally; needs ludworks[vault]
for item in vault.items(search="github"):
    print(item.name)
vault.create_login("Router", username="admin", password="…", uris=["https://192.168.1.1"])
```

The master password and decrypted items stay in your process; only ciphertext is sent.

## Command line

```bash
ludworks login https://nas.local:8443 -u alice          # shows the fingerprint and asks you to confirm it
ludworks ls Home:Movies -l
ludworks upload ./season1/ --to Media:TV/Show/Season\ 1 --category tv --subcategory episode
ludworks download Movies/film.mkv -o ./
ludworks notes import ./my-notes/
ludworks sheets get "NAS budget" 0 A1:D20
ludworks tv now 13
ludworks vault list
```

Credentials are saved per profile in the user's configuration directory (override with
`LUDWORKS_CONFIG_DIR`); run `ludworks --help` for every command.
