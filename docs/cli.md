# Riferimento CLI

_File generato da `go run ./cmd/gendocs`; non modificare a mano._

## backimage

store backups inside runnable, encrypted OCI images

### Synopsis

backimage archives, compresses, encrypts and stores your files inside a multi-arch OCI image that can be restored with plain docker run.

### Options

```
      --config string   config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
  -h, --help            help for backimage
      --json            structured JSON output on stdout
      --no-color        disable ANSI colors (auto-detected)
  -q, --quiet           suppress progress output
  -v, --verbose count   log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage backup]()	 - archive, encrypt and push a backup to an OCI registry
* [backimage doctor]()	 - check privileges and runtime environment
* [backimage find]()	 - find archived paths by glob
* [backimage inspect]()	 - show public backup metadata
* [backimage listen-remote]()	 - receive encrypted backup streams and publish them to a registry
* [backimage login]()	 - store registry credentials for backimage
* [backimage logout]()	 - remove registry credentials
* [backimage ls]()	 - list archived files
* [backimage repo]()	 - inspect an OCI backup repository
* [backimage restore]()	 - restore a backup image to tar or disk
* [backimage verify]()	 - verify a backup image
* [backimage version]()	 - print version information


## backimage backup

archive, encrypt and push a backup to an OCI registry

```
backimage backup <PATH...> --repo IMAGE [flags]
```

### Options

```
      --age-identity string       age identity file used to reuse a deduplication key
      --allow-degraded            continue despite unreadable files
      --auth-token string         pre-shared remote authentication token
      --auth-token-file string    read the remote authentication token from a file
      --compression string        zstd|gzip|xz|lz4|none (default "zstd")
      --compression-level int     codec level (0 = codec default)
      --created string            fixed RFC3339 image creation time (reproducible builds)
      --dedup                     enable content-defined incremental deduplication (reveals chunk equality)
      --dedup-chunk-avg string    advanced CDC average chunk size
      --dedup-chunk-max string    advanced CDC maximum chunk size
      --dedup-chunk-min string    advanced CDC minimum chunk size
      --dedup-polynomial string   advanced Rabin polynomial (0x...) for CDC
      --dry-run                   print the plan and exit without writing
      --encrypt                   encrypt chunks (default) (default true)
      --exclude strings           glob pattern to exclude (repeatable)
  -h, --help                      help for backup
      --jobs int                  parallel uploads (default 3)
      --local-repo                output to the Docker daemon instead of a registry
      --max-layer-size string     target layer size, e.g. 512MiB, 2GiB (default "1GiB")
      --no-encrypt                disable encryption (exclusive with --encrypt)
      --no-metadata               omit source paths from labels
      --numeric-owner             do not resolve user/group names
      --one-file-system           do not cross mount points
      --output string             registry|daemon|oci-layout|tar (default "registry")
      --output-path string        destination for oci-layout/tar
      --passphrase-file string    read the passphrase from a file
      --passphrase-stdin          read the passphrase from stdin
      --platform strings          self-extract platforms (repeatable) (default [linux/amd64,linux/arm64])
      --recipient strings         age public key (repeatable)
      --remote string             send layers to a remote backimage server
      --repo string               target repository, e.g. ghcr.io/me/dumps
      --resume                    resume from the checkpoint if present (default true)
      --runnable                  build runnable images (false allows non-standard codecs) (default true)
      --server-side-compress      ask the remote server to compress (server sees plaintext)
      --tag string                backup tag (default "latest")
      --temp-dir string           spool directory (default $TMPDIR)
      --timestamp                 append a timestamp to the tag
      --timestamp-format string   Go layout for --timestamp (default "20060102T150405Z")
      --tls-ca string             PEM CA bundle for the remote server
      --tls-cert string           PEM client certificate for mTLS
      --tls-key string            PEM client private key for mTLS
      --tls-pin string            remote server certificate SHA-256 fingerprint
      --udp                       use QUIC instead of TCP for --remote
```

### Options inherited from parent commands

```
      --config string   config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json            structured JSON output on stdout
      --no-color        disable ANSI colors (auto-detected)
  -q, --quiet           suppress progress output
  -v, --verbose count   log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images


## backimage doctor

check privileges and runtime environment

```
backimage doctor [PATH...] [flags]
```

### Options

```
  -h, --help   help for doctor
```

### Options inherited from parent commands

```
      --config string   config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json            structured JSON output on stdout
      --no-color        disable ANSI colors (auto-detected)
  -q, --quiet           suppress progress output
  -v, --verbose count   log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images


## backimage find

find archived paths by glob

```
backimage find IMAGE PATTERN [flags]
```

### Options

```
      --cache-size string        maximum downloaded-layer cache size (default "2GiB")
  -h, --help                     help for find
      --identity string          age private key file
      --local-repo               read the image from the local Docker daemon
  -l, --long                     long listing
      --oci-layout string        read from a local OCI layout directory
      --passphrase-file string   read passphrase from a file
      --passphrase-stdin         read passphrase from stdin
      --platform string          source platform (default "linux/amd64")
```

### Options inherited from parent commands

```
      --config string   config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json            structured JSON output on stdout
      --no-color        disable ANSI colors (auto-detected)
  -q, --quiet           suppress progress output
  -v, --verbose count   log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images


## backimage inspect

show public backup metadata

```
backimage inspect IMAGE [flags]
```

### Options

```
      --cache-size string        maximum downloaded-layer cache size (default "2GiB")
      --files                    also list archived files (requires credentials)
  -h, --help                     help for inspect
      --identity string          age private key file
      --layers                   show data layer details
      --local-repo               read the image from the local Docker daemon
      --oci-layout string        read from a local OCI layout directory
      --passphrase-file string   read passphrase from a file
      --passphrase-stdin         read passphrase from stdin
      --platform string          source platform (default "linux/amd64")
```

### Options inherited from parent commands

```
      --config string   config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json            structured JSON output on stdout
      --no-color        disable ANSI colors (auto-detected)
  -q, --quiet           suppress progress output
  -v, --verbose count   log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images


## backimage listen-remote

receive encrypted backup streams and publish them to a registry

```
backimage listen-remote [flags]
```

### Options

```
      --allow-repo strings       allowed repository prefix (repeatable)
      --also-tcp                 when using --udp, also listen on TCP at the same address
      --auth-token string        pre-shared client authentication token
      --auth-token-file string   read the pre-shared token from a file
      --bind-address string      listen address (default "0.0.0.0:7575")
  -h, --help                     help for listen-remote
      --insecure-no-auth         allow unauthenticated clients (strongly discouraged)
      --log-format string        text|json (default "text")
      --max-bytes string         maximum bytes per session (0 = unlimited) (default "0")
      --max-sessions int         maximum concurrent sessions (default 4)
      --metrics-address string   listen address for /healthz and /metrics
      --rate-limit string        bytes per second per session (0 = unlimited) (default "0")
      --spool                    enable disk spool fallback
      --tls-ca string            PEM CA bundle used to authenticate mTLS clients
      --tls-cert string          PEM server certificate
      --tls-key string           PEM server private key
      --tls-self-signed          generate an ephemeral certificate and print its SHA-256 pin
      --udp                      use QUIC instead of TCP
      --work-dir string          fallback spool directory (unused unless --spool)
```

### Options inherited from parent commands

```
      --config string   config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json            structured JSON output on stdout
      --no-color        disable ANSI colors (auto-detected)
  -q, --quiet           suppress progress output
  -v, --verbose count   log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images


## backimage login

store registry credentials for backimage

```
backimage login [REGISTRY] [flags]
```

### Options

```
  -h, --help              help for login
      --list              list configured registries (never the secrets)
  -p, --password ps       password or token (visible in ps, prefer --password-stdin)
      --password-stdin    read the password from stdin
      --token string      ready-made token (alternative to username/password)
  -u, --username string   registry username
```

### Options inherited from parent commands

```
      --config string   config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json            structured JSON output on stdout
      --no-color        disable ANSI colors (auto-detected)
  -q, --quiet           suppress progress output
  -v, --verbose count   log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images


## backimage logout

remove registry credentials

```
backimage logout [REGISTRY] [flags]
```

### Options

```
  -h, --help   help for logout
```

### Options inherited from parent commands

```
      --config string   config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json            structured JSON output on stdout
      --no-color        disable ANSI colors (auto-detected)
  -q, --quiet           suppress progress output
  -v, --verbose count   log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images


## backimage ls

list archived files

```
backimage ls IMAGE [PATH] [flags]
```

### Options

```
      --cache-size string        maximum downloaded-layer cache size (default "2GiB")
      --exclude strings          exclude glob (repeatable)
  -h, --help                     help for ls
      --identity string          age private key file
      --include strings          include glob (repeatable)
      --local-repo               read the image from the local Docker daemon
  -l, --long                     long listing
      --oci-layout string        read from a local OCI layout directory
      --passphrase-file string   read passphrase from a file
      --passphrase-stdin         read passphrase from stdin
      --platform string          source platform (default "linux/amd64")
```

### Options inherited from parent commands

```
      --config string   config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json            structured JSON output on stdout
      --no-color        disable ANSI colors (auto-detected)
  -q, --quiet           suppress progress output
  -v, --verbose count   log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images


## backimage repo

inspect an OCI backup repository

### Options

```
  -h, --help   help for repo
```

### Options inherited from parent commands

```
      --config string   config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json            structured JSON output on stdout
      --no-color        disable ANSI colors (auto-detected)
  -q, --quiet           suppress progress output
  -v, --verbose count   log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images
* [backimage repo caps]()	 - show repository lifecycle capabilities
* [backimage repo prune]()	 - apply a retention policy
* [backimage repo rm]()	 - delete an OCI manifest
* [backimage repo stats]()	 - show shared OCI blobs and effective repository storage
* [backimage repo tags]()	 - list repository tags


## backimage repo caps

show repository lifecycle capabilities

```
backimage repo caps REGISTRY [flags]
```

### Options

```
  -h, --help   help for caps
```

### Options inherited from parent commands

```
      --config string   config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json            structured JSON output on stdout
      --no-color        disable ANSI colors (auto-detected)
  -q, --quiet           suppress progress output
  -v, --verbose count   log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage repo]()	 - inspect an OCI backup repository


## backimage repo prune

apply a retention policy

```
backimage repo prune REPO [flags]
```

### Options

```
      --dry-run                show deletions without changing the registry
  -h, --help                   help for prune
      --keep-last int          keep the newest N backups
      --keep-tag strings       glob pattern for tags to keep
      --keep-within duration   keep backups newer than this duration
      --yes                    confirm destructive deletions
```

### Options inherited from parent commands

```
      --config string   config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json            structured JSON output on stdout
      --no-color        disable ANSI colors (auto-detected)
  -q, --quiet           suppress progress output
  -v, --verbose count   log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage repo]()	 - inspect an OCI backup repository


## backimage repo rm

delete an OCI manifest

```
backimage repo rm REPO:TAG|REPO@DIGEST [flags]
```

### Options

```
      --force   delete a manifest even when other tags reference it
  -h, --help    help for rm
      --yes     confirm this destructive operation
```

### Options inherited from parent commands

```
      --config string   config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json            structured JSON output on stdout
      --no-color        disable ANSI colors (auto-detected)
  -q, --quiet           suppress progress output
  -v, --verbose count   log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage repo]()	 - inspect an OCI backup repository


## backimage repo stats

show shared OCI blobs and effective repository storage

```
backimage repo stats REPO [flags]
```

### Options

```
  -h, --help   help for stats
```

### Options inherited from parent commands

```
      --config string   config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json            structured JSON output on stdout
      --no-color        disable ANSI colors (auto-detected)
  -q, --quiet           suppress progress output
  -v, --verbose count   log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage repo]()	 - inspect an OCI backup repository


## backimage repo tags

list repository tags

```
backimage repo tags REPO [flags]
```

### Options

```
  -h, --help   help for tags
```

### Options inherited from parent commands

```
      --config string   config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json            structured JSON output on stdout
      --no-color        disable ANSI colors (auto-detected)
  -q, --quiet           suppress progress output
  -v, --verbose count   log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage repo]()	 - inspect an OCI backup repository


## backimage restore

restore a backup image to tar or disk

```
backimage restore [IMAGE] [flags]
```

### Options

```
      --cache-size string        maximum downloaded-layer cache size (default "2GiB")
      --cpus int                 maximum CPUs used during restore (default: half available CPUs) (default 8)
  -C, --destination string       destination directory (default ".")
      --exclude strings          exclude glob (repeatable)
  -x, --extract                  extract instead of writing a tar
  -h, --help                     help for restore
      --identity string          age private key file
      --include strings          include glob (repeatable)
      --jobs int                 parallel layer downloads (default 3)
      --local-repo               read the image from the local Docker daemon
      --no-preserve-owner        do not preserve ownership
      --no-verify                skip plaintext chunk digest verification
      --oci-layout string        read from a local OCI layout directory
  -o, --output string            tar filename (- for stdout)
      --overwrite                replace an existing output
      --passphrase-file string   read passphrase from a file
      --passphrase-stdin         read passphrase from stdin
      --platform string          source platform (default "linux/amd64")
      --remove-local-image       remove the local Docker image after a successful restore
      --repo string              image reference (alias for positional IMAGE)
      --strip-components int     remove leading path components
```

### Options inherited from parent commands

```
      --config string   config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json            structured JSON output on stdout
      --no-color        disable ANSI colors (auto-detected)
  -q, --quiet           suppress progress output
  -v, --verbose count   log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images


## backimage verify

verify a backup image

```
backimage verify IMAGE [flags]
```

### Options

```
      --cache-size string        maximum downloaded-layer cache size (default "2GiB")
      --continue                 report every integrity error
  -h, --help                     help for verify
      --identity string          age private key file
      --local-repo               read the image from the local Docker daemon
      --oci-layout string        read from a local OCI layout directory
      --passphrase-file string   read passphrase from a file
      --passphrase-stdin         read passphrase from stdin
      --platform string          source platform (default "linux/amd64")
      --quick                    validate public metadata without downloading data layers
```

### Options inherited from parent commands

```
      --config string   config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json            structured JSON output on stdout
      --no-color        disable ANSI colors (auto-detected)
  -q, --quiet           suppress progress output
  -v, --verbose count   log verbosity (repeat: -v debug, -vv trace)
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
      --config string   config file (default $XDG_CONFIG_HOME/backimage/config.yaml)
      --json            structured JSON output on stdout
      --no-color        disable ANSI colors (auto-detected)
  -q, --quiet           suppress progress output
  -v, --verbose count   log verbosity (repeat: -v debug, -vv trace)
```

### SEE ALSO

* [backimage]()	 - store backups inside runnable, encrypted OCI images


