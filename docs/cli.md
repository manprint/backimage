# Riferimento CLI

_File generato da `go run ./cmd/gendocs`; non modificare a mano._

## backimage

store backups inside runnable, encrypted OCI images

### Synopsis

backimage archives, compresses, encrypts and stores your files inside a multi-arch OCI image that can be restored with plain docker run.

### Options

```
      --config string          config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
  -h, --help                   help for backimage
      --json                   structured JSON output on stdout
      --no-color               disable ANSI colors (auto-detected)
  -q, --quiet                  suppress progress output
      --registry-user string   login to use when a registry holds several accounts (default: the repository namespace); none forces an unauthenticated request
  -v, --verbose count          log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage backup]()	 - archive, encrypt and push a backup to an OCI registry
* [backimage doctor]()	 - check privileges and runtime environment before a backup
* [backimage find]()	 - search archived paths by glob pattern
* [backimage inspect]()	 - show the public metadata of a backup image
* [backimage listen-remote]()	 - receive encrypted backup streams and publish them to a registry
* [backimage login]()	 - store registry credentials for backimage
* [backimage logout]()	 - remove stored registry credentials
* [backimage ls]()	 - list the files archived inside a backup image
* [backimage repo]()	 - inspect and clean up an OCI backup repository
* [backimage restore]()	 - restore a backup image to disk or to a tar file
* [backimage verify]()	 - check that a backup image is complete and intact
* [backimage version]()	 - print version information


## backimage backup

archive, encrypt and push a backup to an OCI registry

### Synopsis

archive, encrypt and push a backup to an OCI registry.

PATH is one or more files or directories; they are archived together in
the order given. The result is a multi-arch OCI image that restores
itself with a plain `docker run`.

Sizes accept binary units (512MiB, 2GiB); a bare number means bytes.

  # local pipeline, encrypted, timestamped tag
  backimage backup /srv/data --repo ghcr.io/me/dumps --tag daily --timestamp \
    --passphrase-file ./pass

  # delegate archiving and push to a remote server (little local disk)
  backimage backup /srv/data --repo ghcr.io/me/dumps --tag nightly \
    --remote backup.example:7575 --tls-pin <PIN> --passphrase-file ./pass

  # see the plan without writing anything
  backimage backup /srv/data --repo ghcr.io/me/dumps --dry-run

```
backimage backup <PATH...> --repo IMAGE [flags]
```

### Options

```
      --age-identity string        age identity file used to reuse a deduplication key
      --allow-degraded             continue despite unreadable files
      --auth-token string          pre-shared remote authentication token
      --auth-token-file string     read the remote authentication token from a file
      --compression string         layer codec: zstd|gzip|xz|lz4|none (xz and lz4 require --runnable=false) (default "zstd")
      --compression-level int      codec compression level; 0 = codec default, higher = smaller and slower
      --created string             fixed image creation time in RFC3339, e.g. 2026-08-10T03:15:00Z (reproducible builds)
      --dedup                      enable content-defined incremental deduplication (reveals chunk equality)
      --dedup-chunk-avg string     advanced CDC average chunk size, e.g. 1MiB (default: codec choice)
      --dedup-chunk-max string     advanced CDC maximum chunk size, e.g. 4MiB (default: codec choice)
      --dedup-chunk-min string     advanced CDC minimum chunk size, e.g. 256KiB (default: codec choice)
      --dedup-polynomial string    advanced Rabin polynomial (0x...) for CDC
      --dry-run                    print the plan and exit without writing
      --encrypt                    encrypt chunks (default) (default true)
      --exclude strings            glob pattern to exclude (repeatable)
  -h, --help                       help for backup
      --jobs int                   number of concurrent blob uploads (default 3)
      --local-repo                 output to the Docker daemon instead of a registry
      --max-layer-size string      target size of each OCI layer, e.g. 512MiB, 2GiB (default "1GiB")
      --no-encrypt                 disable encryption (exclusive with --encrypt)
      --no-metadata                omit source paths from labels
      --numeric-owner              do not resolve user/group names
      --one-file-system            do not cross mount points
      --output string              registry|daemon|oci-layout|tar (default "registry")
      --output-path string         destination for oci-layout/tar
      --passphrase-file string     read the passphrase from a file
      --passphrase-stdin           read the passphrase from stdin
      --password string            passphrase (visible in shell history and process listings)
      --platform strings           self-extract platforms (repeatable) (default [linux/amd64,linux/arm64])
      --recipient strings          age public key (repeatable)
      --remote string              delegate the backup to a remote backimage server, HOST:PORT
      --remote-mode string         stream: the server runs the whole pipeline (default); layers: legacy client-side pipeline (default "stream")
      --repo string                target repository without a tag, e.g. ghcr.io/me/dumps (required)
      --resume                     resume from the checkpoint if present (default true)
      --runnable                   build runnable images (false allows non-standard codecs) (default true)
      --server-side-compress       deprecated alias of --remote-mode stream (already the default)
      --tag string                 tag to publish; combine with --timestamp for one tag per run (default "latest")
      --temp-dir string            spool directory (default $TMPDIR)
      --timestamp                  append a UTC timestamp to --tag, e.g. daily-20260810T031500Z
      --timestamp-format string    Go time layout used by --timestamp (reference date 2006-01-02 15:04:05) (default "20060102T150405Z")
      --tls-ca string              PEM CA bundle for the remote server
      --tls-cert string            PEM client certificate for mTLS
      --tls-key string             PEM client private key for mTLS
      --tls-pin string             remote server certificate SHA-256 fingerprint, hex only (drop the SHA256: prefix printed by the server)
      --udp                        use QUIC instead of TCP for --remote
      --upload-chunk-size string   split each blob upload into HTTP chunks of this size, e.g. 32MiB; 0 sends one request per blob (fastest, use a value only for a registry that refuses large bodies) (default "0")
```

### Options inherited from parent commands

```
      --config string          config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json                   structured JSON output on stdout
      --no-color               disable ANSI colors (auto-detected)
  -q, --quiet                  suppress progress output
      --registry-user string   login to use when a registry holds several accounts (default: the repository namespace); none forces an unauthenticated request
  -v, --verbose count          log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images


## backimage doctor

check privileges and runtime environment before a backup

### Synopsis

check privileges and runtime environment before a backup.

With PATH arguments it reports whether those sources can be read whole
(ownership, xattrs, ACLs, sparse files); without arguments it checks the
environment only. Each unavailable capability comes with a remedy.

  backimage doctor
  sudo backimage doctor /srv/data /var/lib/postgresql

```
backimage doctor [PATH...] [flags]
```

### Options

```
  -h, --help   help for doctor
```

### Options inherited from parent commands

```
      --config string          config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json                   structured JSON output on stdout
      --no-color               disable ANSI colors (auto-detected)
  -q, --quiet                  suppress progress output
      --registry-user string   login to use when a registry holds several accounts (default: the repository namespace); none forces an unauthenticated request
  -v, --verbose count          log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images


## backimage find

search archived paths by glob pattern

### Synopsis

search archived paths by glob pattern.

PATTERN is a shell-style glob matched against the whole archived path;
quote it so the local shell does not expand it first. Like `ls`, this
reads the index only and needs the passphrase for an encrypted backup.

  backimage find ghcr.io/me/dumps:daily '**/*.conf' --passphrase-file ./pass
  backimage find ghcr.io/me/dumps:daily 'etc/nginx/*' -l

```
backimage find IMAGE PATTERN [flags]
```

### Options

```
      --cache-size string        maximum size of the downloaded-layer cache, e.g. 512MiB, 4GiB (0 disables it) (default "2GiB")
  -h, --help                     help for find
      --identity string          age private key file, for a backup encrypted with --recipient
      --local-repo               read the image from the local Docker daemon instead of a registry
  -l, --long                     long listing: mode, owner, size and modification time
      --oci-layout string        read the image from this local OCI layout directory
      --passphrase-file string   read the backup passphrase from this file (first line)
      --passphrase-stdin         read the backup passphrase from stdin
      --password ps              backup passphrase inline (visible in shell history and in ps: prefer --passphrase-file)
      --platform string          platform variant to read from the multi-arch image, OS/ARCH (default "linux/amd64")
```

### Options inherited from parent commands

```
      --config string          config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json                   structured JSON output on stdout
      --no-color               disable ANSI colors (auto-detected)
  -q, --quiet                  suppress progress output
      --registry-user string   login to use when a registry holds several accounts (default: the repository namespace); none forces an unauthenticated request
  -v, --verbose count          log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images


## backimage inspect

show the public metadata of a backup image

### Synopsis

show the public metadata of a backup image.

IMAGE is a reference such as ghcr.io/me/dumps:nightly-20260810T031500Z,
or a local source when --local-repo/--oci-layout is used. Layout,
compression and encryption settings need no passphrase. An encrypted
backup keeps source paths, host and totals inside its encrypted
metadata, so those (and --files) appear only when a passphrase or an
age identity is supplied.

  backimage inspect ghcr.io/me/dumps:daily
  backimage inspect ghcr.io/me/dumps:daily --files --passphrase-file ./pass

```
backimage inspect IMAGE [flags]
```

### Options

```
      --cache-size string        maximum size of the downloaded-layer cache, e.g. 512MiB, 4GiB (0 disables it) (default "2GiB")
      --files                    also list archived files (decrypts the index: needs the passphrase or age identity)
  -h, --help                     help for inspect
      --identity string          age private key file, for a backup encrypted with --recipient
      --layers                   show per-layer digest, size and chunk count
      --local-repo               read the image from the local Docker daemon instead of a registry
      --oci-layout string        read the image from this local OCI layout directory
      --passphrase-file string   read the backup passphrase from this file (first line)
      --passphrase-stdin         read the backup passphrase from stdin
      --password ps              backup passphrase inline (visible in shell history and in ps: prefer --passphrase-file)
      --platform string          platform variant to read from the multi-arch image, OS/ARCH (default "linux/amd64")
```

### Options inherited from parent commands

```
      --config string          config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json                   structured JSON output on stdout
      --no-color               disable ANSI colors (auto-detected)
  -q, --quiet                  suppress progress output
      --registry-user string   login to use when a registry holds several accounts (default: the repository namespace); none forces an unauthenticated request
  -v, --verbose count          log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images


## backimage listen-remote

receive encrypted backup streams and publish them to a registry

### Synopsis

receive encrypted backup streams and publish them to a registry.

Every flag can also be set through the environment as BACKIMAGE_<FLAG>
with dashes replaced by underscores (--bind-address becomes
BACKIMAGE_BIND_ADDRESS); an explicit flag always wins. This is what makes
the container image configurable without a shell in the entrypoint.

```
backimage listen-remote [flags]
```

### Options

```
      --allow-repo strings       repository prefix a client may push to, e.g. ghcr.io/team/ (repeatable; empty = any)
      --also-tcp                 when using --udp, also listen on TCP at the same address
      --auth-token string        pre-shared client authentication token
      --auth-token-file string   read the pre-shared token from a file
      --bind-address string      address to listen on, HOST:PORT (0.0.0.0 = every interface) (default "0.0.0.0:7575")
  -h, --help                     help for listen-remote
      --insecure-no-auth         allow unauthenticated clients (strongly discouraged)
      --log-format string        diagnostics format: text|json (default "text")
      --max-bytes string         maximum bytes accepted per session, e.g. 200GiB (0 = unlimited) (default "0")
      --max-sessions int         maximum concurrent backup sessions; server disk needed is 2 x layer size x sessions (default 4)
      --metrics-address string   serve /healthz and /metrics on this HOST:PORT (empty = disabled)
      --rate-limit string        bytes per second per session, e.g. 80MiB (0 = unlimited) (default "0")
      --spool                    deprecated: the streaming protocol always spools one layer at a time
      --tls-ca string            PEM CA bundle used to authenticate mTLS clients
      --tls-cert string          PEM server certificate
      --tls-key string           PEM server private key
      --tls-self-signed          generate a self-signed certificate and print its SHA-256 pin; persisted in --tls-cert/--tls-key or under --work-dir when either is set
      --udp                      use QUIC instead of TCP
      --work-dir string          directory for the per-layer spool of streaming sessions (default $TMPDIR)
```

### Options inherited from parent commands

```
      --config string          config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json                   structured JSON output on stdout
      --no-color               disable ANSI colors (auto-detected)
  -q, --quiet                  suppress progress output
      --registry-user string   login to use when a registry holds several accounts (default: the repository namespace); none forces an unauthenticated request
  -v, --verbose count          log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images


## backimage login

store registry credentials for backimage

### Synopsis

store registry credentials for backimage.

REGISTRY is a host, e.g. ghcr.io or docker.io (default: docker.io).
Credentials are kept in backimage's own auth file, separate from
Docker's. Several accounts can be logged in on the same host: each
--username is stored separately and none overwrites another.

Which account is used is decided by the repository namespace:
docker.io/user2/img uses the login named user2. When the namespace
matches no account the command stops instead of guessing; pick one
with --registry-user NAME (or --registry-user none for an
unauthenticated request).

On Docker Hub the password must be an access token, and a repository
must include the namespace: docker.io/USER/NAME.

  backimage login docker.io --username user1 --password-stdin < t1.txt
  backimage login docker.io --username user2 --password-stdin < t2.txt
  backimage login ghcr.io --username me --password-stdin < token.txt
  backimage login --list

```
backimage login [REGISTRY] [flags]
```

### Options

```
  -h, --help              help for login
      --list              list the stored logins with provider, registry account and local owner (never the secrets)
  -p, --password ps       password or token (visible in ps, prefer --password-stdin)
      --password-stdin    read the password from stdin
      --token string      ready-made token (alternative to username/password)
  -u, --username string   registry username
```

### Options inherited from parent commands

```
      --config string          config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json                   structured JSON output on stdout
      --no-color               disable ANSI colors (auto-detected)
  -q, --quiet                  suppress progress output
      --registry-user string   login to use when a registry holds several accounts (default: the repository namespace); none forces an unauthenticated request
  -v, --verbose count          log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images


## backimage logout

remove stored registry credentials

### Synopsis

remove stored registry credentials.

REGISTRY is a host, e.g. ghcr.io (default: docker.io). Only backimage's
auth file is touched; Docker's credentials are left alone.

With one account on the host it is removed directly. With several the
command stops and lists them: pick one with --user NAME, or remove them
all with --all.

  backimage logout docker.io --user user2
  backimage logout docker.io --all

```
backimage logout [REGISTRY] [flags]
```

### Options

```
      --all           remove every account stored for the registry
  -h, --help          help for logout
      --user string   account to remove when the registry holds several logins
```

### Options inherited from parent commands

```
      --config string          config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json                   structured JSON output on stdout
      --no-color               disable ANSI colors (auto-detected)
  -q, --quiet                  suppress progress output
      --registry-user string   login to use when a registry holds several accounts (default: the repository namespace); none forces an unauthenticated request
  -v, --verbose count          log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images


## backimage ls

list the files archived inside a backup image

### Synopsis

list the files archived inside a backup image.

PATH restricts the listing to one directory inside the archive (default:
everything). Reading the file list decrypts the index, so an encrypted
backup needs the passphrase or the age identity. No data layer is
downloaded.

  backimage ls ghcr.io/me/dumps:daily --passphrase-file ./pass
  backimage ls ghcr.io/me/dumps:daily var/log -l

```
backimage ls IMAGE [PATH] [flags]
```

### Options

```
      --cache-size string        maximum size of the downloaded-layer cache, e.g. 512MiB, 4GiB (0 disables it) (default "2GiB")
      --exclude strings          skip paths matching this glob (repeatable)
  -h, --help                     help for ls
      --identity string          age private key file, for a backup encrypted with --recipient
      --include strings          list only paths matching this glob, e.g. '**/*.pdf' (repeatable)
      --local-repo               read the image from the local Docker daemon instead of a registry
  -l, --long                     long listing: mode, owner, size and modification time
      --oci-layout string        read the image from this local OCI layout directory
      --passphrase-file string   read the backup passphrase from this file (first line)
      --passphrase-stdin         read the backup passphrase from stdin
      --password ps              backup passphrase inline (visible in shell history and in ps: prefer --passphrase-file)
      --platform string          platform variant to read from the multi-arch image, OS/ARCH (default "linux/amd64")
```

### Options inherited from parent commands

```
      --config string          config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json                   structured JSON output on stdout
      --no-color               disable ANSI colors (auto-detected)
  -q, --quiet                  suppress progress output
      --registry-user string   login to use when a registry holds several accounts (default: the repository namespace); none forces an unauthenticated request
  -v, --verbose count          log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images


## backimage repo

inspect and clean up an OCI backup repository

### Synopsis

inspect and clean up an OCI backup repository.

REPO is a repository without a tag, e.g. ghcr.io/me/dumps or
docker.io/me/backups. Credentials come from `backimage login`.

### Options

```
  -h, --help   help for repo
```

### Options inherited from parent commands

```
      --config string          config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json                   structured JSON output on stdout
      --no-color               disable ANSI colors (auto-detected)
  -q, --quiet                  suppress progress output
      --registry-user string   login to use when a registry holds several accounts (default: the repository namespace); none forces an unauthenticated request
  -v, --verbose count          log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images
* [backimage repo caps]()	 - show which lifecycle operations a registry supports
* [backimage repo prune]()	 - delete old backup tags according to a retention policy
* [backimage repo rm]()	 - delete one tag or manifest from the registry
* [backimage repo stats]()	 - show shared OCI blobs and effective repository storage
* [backimage repo tags]()	 - list repository tags with digest and creation time


## backimage repo caps

show which lifecycle operations a registry supports

### Synopsis

show which lifecycle operations a registry supports.

REGISTRY is a host, e.g. ghcr.io or docker.io. Capabilities are the
protocol features the adapter implements, not a permission check on
your account.

```
backimage repo caps REGISTRY [flags]
```

### Options

```
  -h, --help   help for caps
```

### Options inherited from parent commands

```
      --config string          config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json                   structured JSON output on stdout
      --no-color               disable ANSI colors (auto-detected)
  -q, --quiet                  suppress progress output
      --registry-user string   login to use when a registry holds several accounts (default: the repository namespace); none forces an unauthenticated request
  -v, --verbose count          log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage repo]()	 - inspect and clean up an OCI backup repository


## backimage repo prune

delete old backup tags according to a retention policy

### Synopsis

delete old backup tags according to a retention policy.

A tag is kept when at least one rule selects it, and deleted only when
no rule does. With no rule at all nothing is deleted. Tags without a
creation timestamp are always kept, so a non-backimage tag is never
removed by accident.

Durations accept units: s, m, h, d (days), w (weeks); e.g. 90m, 12h, 3d, 2w.

Examples:
  # keep the 7 newest backups, delete the rest
  backimage repo prune ghcr.io/me/dumps --keep-last 7 --dry-run

  # delete everything older than 3 days
  backimage repo prune ghcr.io/me/dumps --delete-older-than 3d --yes

  # delete backups older than 12 hours, but always keep the 2 newest
  # and every tag named release-*
  backimage repo prune ghcr.io/me/dumps --delete-older-than 12h \
    --keep-last 2 --keep-tag 'release-*' --yes

Always run with --dry-run first: deletions cannot be undone.

```
backimage repo prune REPO [flags]
```

### Options

```
      --delete-older-than duration   delete backups older than this age; inverse wording of --keep-within, same rule (units: s, m, h, d (days), w (weeks); e.g. 90m, 12h, 3d, 2w)
      --dry-run                      list what would be deleted and exit without touching the registry
  -h, --help                         help for prune
      --keep-last int                keep the N newest backups regardless of age (0 = rule disabled)
      --keep-tag strings             glob pattern of tag names to keep, e.g. 'release-*' (repeatable)
      --keep-within duration         keep backups newer than this age (units: s, m, h, d (days), w (weeks); e.g. 90m, 12h, 3d, 2w)
      --yes                          required to actually delete: without it the command refuses to run
```

### Options inherited from parent commands

```
      --config string          config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json                   structured JSON output on stdout
      --no-color               disable ANSI colors (auto-detected)
  -q, --quiet                  suppress progress output
      --registry-user string   login to use when a registry holds several accounts (default: the repository namespace); none forces an unauthenticated request
  -v, --verbose count          log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage repo]()	 - inspect and clean up an OCI backup repository


## backimage repo rm

delete one tag or manifest from the registry

### Synopsis

delete one tag or manifest from the registry.

Irreversible: the manifest is deleted by digest, and a registry with
deletion disabled rejects the request. When several tags point at the
same manifest the command refuses to proceed unless --force is given,
because deleting the manifest removes all of them at once.

```
backimage repo rm REPO:TAG|REPO@DIGEST [flags]
```

### Options

```
      --force   delete the manifest even when other tags point at it (they are removed too)
  -h, --help    help for rm
      --yes     required to actually delete: without it the command refuses to run
```

### Options inherited from parent commands

```
      --config string          config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json                   structured JSON output on stdout
      --no-color               disable ANSI colors (auto-detected)
  -q, --quiet                  suppress progress output
      --registry-user string   login to use when a registry holds several accounts (default: the repository namespace); none forces an unauthenticated request
  -v, --verbose count          log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage repo]()	 - inspect and clean up an OCI backup repository


## backimage repo stats

show shared OCI blobs and effective repository storage

### Synopsis

show shared OCI blobs and effective repository storage.

Storage is what the registry stores once deduplication between tags is
taken into account; referred bytes is the sum over all tags.

```
backimage repo stats REPO [flags]
```

### Options

```
  -h, --help   help for stats
```

### Options inherited from parent commands

```
      --config string          config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json                   structured JSON output on stdout
      --no-color               disable ANSI colors (auto-detected)
  -q, --quiet                  suppress progress output
      --registry-user string   login to use when a registry holds several accounts (default: the repository namespace); none forces an unauthenticated request
  -v, --verbose count          log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage repo]()	 - inspect and clean up an OCI backup repository


## backimage repo tags

list repository tags with digest and creation time

### Synopsis

list repository tags with digest and creation time.

Columns: tag, manifest digest, creation time (RFC3339, UTC). A dash
means the image carries no creation timestamp; `prune` never removes
such a tag. Use --json for machine-readable output.

```
backimage repo tags REPO [flags]
```

### Options

```
  -h, --help   help for tags
```

### Options inherited from parent commands

```
      --config string          config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json                   structured JSON output on stdout
      --no-color               disable ANSI colors (auto-detected)
  -q, --quiet                  suppress progress output
      --registry-user string   login to use when a registry holds several accounts (default: the repository namespace); none forces an unauthenticated request
  -v, --verbose count          log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage repo]()	 - inspect and clean up an OCI backup repository


## backimage restore

restore a backup image to disk or to a tar file

### Synopsis

restore a backup image to disk or to a tar file.

IMAGE is the reference to restore (or --repo, or a local source with
--local-repo/--oci-layout). Choose one of two outcomes: -x extracts into
--destination, -o writes a tar file (- for stdout). Without either, a tar
is written to stdout.

  # extract everything into /restore
  backimage restore ghcr.io/me/dumps:daily -x -C /restore --passphrase-file ./pass

  # only the PDFs, without the leading directory level
  backimage restore ghcr.io/me/dumps:daily -x -C . \
    --include '**/*.pdf' --strip-components 1 --passphrase-file ./pass

  # keep the archive as a tar file
  backimage restore ghcr.io/me/dumps:daily -o backup.tar --passphrase-file ./pass

```
backimage restore [IMAGE] [flags]
```

### Options

```
      --cache-size string        maximum size of the downloaded-layer cache, e.g. 512MiB, 4GiB (0 disables it) (default "2GiB")
      --cpus int                 maximum CPUs used for decompression and decryption (default: half the available CPUs) (default 4)
  -C, --destination string       directory the files are extracted into (with -x) (default ".")
      --exclude strings          skip paths matching this glob (repeatable)
  -x, --extract                  extract the files into --destination instead of writing a tar
  -h, --help                     help for restore
      --identity string          age private key file, for a backup encrypted with --recipient
      --include strings          restore only paths matching this glob, e.g. '**/*.pdf' (repeatable)
      --jobs int                 number of concurrent layer downloads (default 3)
      --local-repo               read the image from the local Docker daemon instead of a registry
      --no-preserve-owner        restore files as the current user instead of the archived owner
      --no-verify                skip the plaintext chunk digest check (faster, unsafe)
      --oci-layout string        read the image from this local OCI layout directory
  -o, --output string            write the archive to this tar file; - means stdout
      --overwrite                allow writing over an existing tar file or a non-empty destination
      --passphrase-file string   read the backup passphrase from this file (first line)
      --passphrase-stdin         read the backup passphrase from stdin
      --password ps              backup passphrase inline (visible in shell history and in ps: prefer --passphrase-file)
      --platform string          platform variant to read from the multi-arch image, OS/ARCH (default "linux/amd64")
      --remove-local-image       delete the pulled Docker image once the restore succeeded
      --repo string              image reference (alias for positional IMAGE)
      --strip-components int     drop this many leading path components from each restored path (like tar)
```

### Options inherited from parent commands

```
      --config string          config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json                   structured JSON output on stdout
      --no-color               disable ANSI colors (auto-detected)
  -q, --quiet                  suppress progress output
      --registry-user string   login to use when a registry holds several accounts (default: the repository namespace); none forces an unauthenticated request
  -v, --verbose count          log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images


## backimage verify

check that a backup image is complete and intact

### Synopsis

check that a backup image is complete and intact.

By default every data layer is downloaded and each chunk digest is
recomputed, which costs the full backup size in network traffic; --quick
validates manifest, index and layer digests only. Run this before
trusting a backup for a restore. Exit code 5 means an integrity failure.

  backimage verify ghcr.io/me/dumps:daily --quick
  backimage verify ghcr.io/me/dumps:daily --passphrase-file ./pass --continue

```
backimage verify IMAGE [flags]
```

### Options

```
      --cache-size string        maximum size of the downloaded-layer cache, e.g. 512MiB, 4GiB (0 disables it) (default "2GiB")
      --continue                 do not stop at the first integrity error: report them all
  -h, --help                     help for verify
      --identity string          age private key file, for a backup encrypted with --recipient
      --local-repo               read the image from the local Docker daemon instead of a registry
      --oci-layout string        read the image from this local OCI layout directory
      --passphrase-file string   read the backup passphrase from this file (first line)
      --passphrase-stdin         read the backup passphrase from stdin
      --password ps              backup passphrase inline (visible in shell history and in ps: prefer --passphrase-file)
      --platform string          platform variant to read from the multi-arch image, OS/ARCH (default "linux/amd64")
      --quick                    check public metadata and layer digests only, without downloading the data layers
```

### Options inherited from parent commands

```
      --config string          config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json                   structured JSON output on stdout
      --no-color               disable ANSI colors (auto-detected)
  -q, --quiet                  suppress progress output
      --registry-user string   login to use when a registry holds several accounts (default: the repository namespace); none forces an unauthenticated request
  -v, --verbose count          log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images


## backimage version

print version information

```
backimage version [flags]
```

### Options

```
  -h, --help   help for version
```

### Options inherited from parent commands

```
      --config string          config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json                   structured JSON output on stdout
      --no-color               disable ANSI colors (auto-detected)
  -q, --quiet                  suppress progress output
      --registry-user string   login to use when a registry holds several accounts (default: the repository namespace); none forces an unauthenticated request
  -v, --verbose count          log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images


