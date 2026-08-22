# backimage

Your files, archived and encrypted inside a multi-arch OCI image. The image is
also a self-extracting program, so the machine that restores the data needs
`docker` and nothing else — no tool to install, no agent, no backup server. The
registry is the storage.

*Questa pagina è disponibile anche [in italiano](README.it.md).*

```console
backimage backup /srv/data --repo ghcr.io/me/dumps --tag daily --passphrase-file ./pass
docker run --rm --privileged -v "$PWD/restore:/restore" \
  ghcr.io/me/dumps:daily extract --out /restore
```

## Install

```console
# release archive
tar -xzf backimage_*.tar.gz && sudo install -m 0755 backimage /usr/local/bin/
backimage version

# from source (Go 1.26+)
go install github.com/manprint/backimage/cmd/backimage@latest

# container
docker run --rm ghcr.io/manprint/backimage:latest version
```

## First backup in four commands

```console
# 1. a passphrase worth having: 32 random characters, ~180 bits
umask 077 && backimage genpass > backup.pass

# 2. credentials for the registry (not the backup passphrase)
printf '%s\n' "$REGISTRY_TOKEN" | backimage login ghcr.io --username me --password-stdin

# 3. back up, encrypted, one tag per run
backimage backup /srv/data --repo ghcr.io/me/dumps --tag daily --timestamp \
  --passphrase-file ./backup.pass

# 4. prove it is intact before you ever need it
backimage verify ghcr.io/me/dumps:daily-20260822T031500Z --passphrase-file ./backup.pass
```

Lose the passphrase and the data is gone. There is no recovery path, by design.

---

## Commands

Every command takes `--json` for machine-readable output, `-q` to silence
progress, and `-v`/`-vv` for more logging. `backimage <command> --help` is the
full reference; [`docs/cli.md`](docs/cli.md) is the generated version of it.

### `backup` — archive, encrypt, push

```console
# one or more roots, archived together
backimage backup /var/lib/myapp /etc/myapp/app.conf --repo ghcr.io/me/dumps --tag app

# see the plan without writing anything
backimage backup /srv/data --repo ghcr.io/me/dumps --dry-run --json

# exclude subtrees (see "Archived path names" below for the pattern base)
backimage backup /home/alice --repo ghcr.io/me/dumps --tag home \
  --exclude 'alice/.cache/**' --exclude 'alice/Downloads/*.iso'

# a local artifact instead of a registry
backimage backup /srv/data --repo local/dumps --tag t --output oci-layout --output-path ./layout
backimage backup /srv/data --repo local/dumps --tag t --local-repo   # Docker daemon
```

| Flag | What it does |
| --- | --- |
| `--repo` | target repository, no tag (required) |
| `--tag`, `--timestamp` | tag to publish; `--timestamp` appends a UTC stamp, one tag per run |
| `--exclude` | glob to skip, repeatable |
| `--one-file-system` | do not cross mount points |
| `--compression`, `--compression-level` | `zstd` (default, levels 1–4), `gzip` (1–9), `lz4`, `xz`, `none` |
| `--max-layer-size` | layer target size, default `1GiB` |
| `--output`, `--output-path` | `registry` (default), `daemon`, `oci-layout`, `tar` |
| `--dedup` | incremental deduplication (see the warning below) |
| `--dry-run` | print the plan and exit |
| `--allow-degraded` | continue when some files cannot be read whole |
| `--created` | fixed creation time, for reproducible images |

`xz` and `lz4` use non-standard OCI media types and therefore require
`--runnable=false`; the image is then an artifact to restore with the CLI, not
something `docker run` will execute. `--dedup` reveals to anyone who can read
the registry which chunks two backups have in common — read
[`docs/dedup.md`](docs/dedup.md) before enabling it.

### `restore` — back to disk, or to a tar

```console
# extract into a directory
backimage restore ghcr.io/me/dumps:daily -x -C ./restore --passphrase-file ./pass

# only part of it
backimage restore ghcr.io/me/dumps:daily -x -C ./restore \
  --include '**/*.pdf' --exclude '**/tmp/**' --passphrase-file ./pass

# a tar file, or stdout
backimage restore ghcr.io/me/dumps:daily -o ./backup.tar --passphrase-file ./pass
backimage restore ghcr.io/me/dumps:daily -o - --passphrase-file ./pass | tar -tv

# from a local source instead of the registry
backimage restore local/dumps:t --oci-layout ./layout -x -C ./restore
```

Without `-x` or `-o` a tar goes to stdout. Each chunk's plaintext digest is
verified before anything is written; `--strict` refuses to degrade a single
metadata operation, `--continue` salvages every entry that verifies and reports
the ones lost. For ownership, device nodes, ACLs and `trusted.*` xattrs, run the
restore as root.

### `restore` without installing anything

The image restores itself. This is the point of the format:

```console
docker run --rm --privileged \
  -e BACKIMAGE_PASSPHRASE="$(cat ./pass)" \
  -v "$PWD/restore:/restore" \
  ghcr.io/me/dumps:daily extract --out /restore
```

`--privileged` is what buys full fidelity (ownership, devices, ACLs, overlayfs
xattrs). Without it the extraction still succeeds and the summary states which
metadata classes were degraded. On Docker Desktop for macOS and Windows, prefer
`tar` and extract on the host.

### `inspect`, `ls`, `find` — look before you restore

```console
backimage inspect ghcr.io/me/dumps:daily                      # public metadata, no passphrase
backimage inspect ghcr.io/me/dumps:daily --layers --json
backimage ls   ghcr.io/me/dumps:daily data/logs -l --passphrase-file ./pass
backimage find ghcr.io/me/dumps:daily '**/*.conf' --passphrase-file ./pass
```

Layout, compression and encryption settings are public. Source paths, host and
the file list live in the encrypted metadata, so they need the passphrase.
Reading the index downloads no data layer.

The `PATH` argument of `ls` and the pattern of `find` are matched against the
**archived** name, which starts at the source basename — after `backimage backup
/srv/data` the entries are `data/...`, so the path to ask for is `data/logs`, not
`/srv/data/logs`.

### `verify` — before you trust it

```console
backimage verify ghcr.io/me/dumps:daily --quick                 # metadata and layer digests
backimage verify ghcr.io/me/dumps:daily --continue --passphrase-file ./pass
```

The full check re-downloads every layer and recomputes every chunk digest, so it
costs the backup size in traffic. Exit code 5 means an integrity failure.

### `repo` — the lifecycle of what you published

```console
backimage repo tags  ghcr.io/me/dumps            # tag, digest, creation time
backimage repo stats ghcr.io/me/dumps            # unique vs shared blobs, real storage
backimage repo caps  ghcr.io                     # what this registry lets us do
backimage repo rm    ghcr.io/me/dumps:old --yes  # one manifest
```

`prune` applies a retention policy. A tag survives if **any** rule keeps it, and
is deleted only when none does; with no rule at all nothing is deleted, and a tag
without a creation timestamp is never touched.

```console
# keep the 7 newest — always look first
backimage repo prune ghcr.io/me/dumps --keep-last 7 --dry-run

# delete everything older than 3 days, keeping the 2 newest and every release-*
backimage repo prune ghcr.io/me/dumps --delete-older-than 3d \
  --keep-last 2 --keep-tag 'release-*' --yes
```

When one repository holds several families of backups (`db_1..db_N` next to
`app_1..app_N`), `--keep-last 3` alone would mean "3 in the whole repository".
Two selectors fix that:

```console
# of the database backups keep the 3 newest; leave every other tag alone
backimage repo prune ghcr.io/me/dumps --tag-regex 'db_.*' --keep-last 3 --dry-run

# keep the 3 newest of every family in one pass
backimage repo prune ghcr.io/me/dumps --group-by-regex '([a-z]+)_.*' --keep-last 3 --dry-run

# preview a selection with a command that cannot delete anything
backimage repo tags ghcr.io/me/dumps --tag-regex 'db_.*'
```

Three properties worth knowing, because they are what stops a wrong deletion:

1. **A regex is never a deletion rule.** It only narrows what the rules reach, so
   `--tag-regex 'db_.*'` on its own deletes nothing.
2. **The pattern must match the whole tag.** `db_` selects nothing, `db_.*`
   selects `db_1`. Go's unanchored default would have let `db` select `app_db_1`
   too. Syntax is RE2: no lookahead, no backreferences, `(?i)` for case folding.
3. **What a selector excludes does not consume a slot.** Out-of-scope tags are
   kept and count for no rule.

Deletion is by manifest digest, so two tags on the same manifest go together.
`prune` checks the whole plan before the first request and refuses, naming the
tags, rather than stopping halfway.

### `login`, `logout` — registry credentials

```console
printf '%s\n' "$PAT" | backimage login ghcr.io --username me --password-stdin
backimage login --list                       # accounts, never secrets
backimage logout ghcr.io
```

Several accounts on the same registry coexist; the repository namespace picks
one, and `--registry-user NAME` overrides it (`--registry-user none` forces an
anonymous request). `--registry-user` is global and applies to every command
that talks to a registry.

```console
backimage backup /srv/data --repo ghcr.io/team/dumps --registry-user me
backimage logout docker.io --user user2      # one account
backimage logout docker.io --all             # all of them
```

Credentials go to `$XDG_CONFIG_HOME/backimage/auth.json` (or
`BACKIMAGE_AUTH_FILE`), in Docker's `auths` format, mode `0600`, and are refused
if group- or world-readable. Docker's own config is the fallback.
[`docs/registries.md`](docs/registries.md) has the resolution order and the
per-vendor notes.

### `genpass` — a passphrase worth having

```console
backimage genpass                       # 32 characters, ~180 bits
backimage genpass --length 48
backimage genpass --no-symbols          # for fields that reject punctuation
```

The key file travels inside the image, so whoever holds the image can try
passphrases offline without limit. An invented 24-character sentence is worth
about 30 bits and falls in hours.

### `doctor` — check before the backup, not after

```console
backimage doctor                                   # environment only
sudo backimage doctor /srv/data /var/lib/postgresql
```

Reports whether those sources can be read whole (ownership, xattrs, ACLs, sparse
files) and gives the remedy for each capability that is missing.

### `listen-remote` — let a server do the work

```console
# server
backimage listen-remote --bind-address 0.0.0.0:7575 --tls-self-signed \
  --auth-token-file ./token --allow-repo 'ghcr.io/me/*'

# client: only the tar stream leaves the machine
backimage backup /srv/data --repo ghcr.io/me/dumps --tag daily \
  --remote backup.example:7575 --tls-pin <PIN> \
  --auth-token-file ./token --passphrase-file ./pass
```

In the default `stream` mode the server runs the whole pipeline, so the client
never holds the archive or a layer and its disk use does not grow with the
backup size (measured: 4 KiB of client spool for a 4 GiB backup). The trade-off
is explicit: **the server sees the plaintext**, because it is the one encrypting
it. Use `--remote-mode layers` when the receiver must not.

Registry credentials stay on the client, which hands the server short-lived
bearer tokens over TLS. Full setup, certificate pinning, mTLS and QUIC are in
[`docs/remote.md`](docs/remote.md).

### `version`

```console
backimage version --json
```

---

## Things worth knowing before the first real backup

### Archived path names

Each source is archived under its own **basename**: `backimage backup
/home/alice` produces entries `alice/...`, not `home/alice/...`. That base is
what `--exclude`, `--include` and `find` patterns are matched against, and what
`--strip-components` counts.

Two consequences:

- Two sources with the same basename collide and the backup refuses to start
  (`/opt/app/data` together with `/srv/app/data`). Rename or split the run.
- **Do not pass `/` as the only root.** Its basename is `/`, so entries become
  `//etc`, `//home`, and such an image does not restore. List the subtrees
  instead: `backimage backup /etc /var/lib /home`.

### Glob patterns

`*` and `?` stay inside one path segment; `**` spans any number of segments,
including none. `dir/**` covers `dir` itself and everything under it, and a bare
`dir` does the same. The same rules apply to `backup --exclude`, `restore
--include/--exclude`, `ls` and `find`. A malformed pattern is a usage error, not
a silent no-op.

### Passing secrets

Prefer `--passphrase-file`, `--passphrase-stdin`, `--password-stdin` and
`--auth-token-file`. `--password` and `--token` work but leave the secret in the
shell history and in `ps`.

### Exit codes

| Code | Meaning |
| --- | --- |
| 0 | success |
| 1 | generic failure |
| 2 | usage error |
| 3 | insufficient privileges |
| 4 | missing or wrong passphrase |
| 5 | integrity failure — the data does not match its digests |
| 6 | network or registry error |
| 7 | interrupted |

### Environment

| Variable | Effect |
| --- | --- |
| `BACKIMAGE_PASSPHRASE` | passphrase for the CLI and for the self-extracting image |
| `BACKIMAGE_AUTH_FILE` | credential file to use instead of the default |
| `BACKIMAGE_<FLAG>` | default for a flag, e.g. `BACKIMAGE_BIND_ADDRESS`, `BACKIMAGE_JSON`; an explicit flag wins |
| `XDG_CONFIG_HOME` | base for `backimage/auth.json` |
| `XDG_CACHE_HOME` | layer cache and resumable upload checkpoints |
| `TMPDIR` | spool, unless `--temp-dir` is given |

### Known limits

- `docker run` tolerates at most 118 data layers, so very large backups get very
  large layers. That is the price of keeping the image runnable.
- Extraction inside the container on Docker Desktop for macOS and Windows does
  not preserve ownership and xattrs: take the `tar` and extract on the host.
- `xz` and `lz4` produce images `docker run` cannot execute (`--runnable=false`).
- Deduplication has layer granularity, not block granularity: it saves less than
  restic or kopia for the same edits.
- Tag deletion depends on the registry — see
  [`docs/registries.md`](docs/registries.md).
- A single `/` root is not supported (see *Archived path names*).
- Losing the passphrase means losing the backup. There is no recovery.

---

## Documentation

The extended handbook, in Italian, is
[`docs/handbook.it.md`](docs/handbook.it.md): long recipes, TLS certificates,
multi-account setups, `compose.yml` for the server, maximum-fidelity procedures.

Technical reference: [backup](docs/backup.md) · [restore](docs/restore.md) ·
[fidelity](docs/FIDELITY.md) · [registries](docs/registries.md) ·
[dedup](docs/dedup.md) · [compression](docs/compression.md) ·
[remote](docs/remote.md) · [image format](docs/image-format.md) ·
[security](docs/security.md) · [CLI reference](docs/cli.md) ·
[architecture](docs/ARCHITECTURE.md)

## Development

```console
make check       # lint, tests, race tests, project gates
make build       # local binary
make embed       # two-stage build with the embedded self-extractors
make e2e PHASE=05
```

See [`docs/CONTRIBUTING.md`](docs/CONTRIBUTING.md) and
[`docs/BUILD.md`](docs/BUILD.md).

## License

See [`LICENSE`](LICENSE).
