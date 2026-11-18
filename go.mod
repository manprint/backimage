module github.com/manprint/backimage

go 1.27

// Minimum toolchain: go1.27.4 is the latest patch of the language version
// above. It closes the 12 advisories fixed on 2026-11-10 (11 in the
// toolchain/cgo, one in net/http); govulncheck reports no advisory reachable
// from this code with it. Earlier patches did not: go1.26.6 left 11, and
// go1.27.2 still had crypto/x509 GO-2026-6809. This line is what stops a
// release from being built with an older patch.
toolchain go1.27.4

require (
	filippo.io/age v1.3.2
	github.com/google/go-containerregistry v0.22.1
	github.com/klauspost/compress v1.20.4
	github.com/moby/moby/client v0.6.3
	github.com/pierrec/lz4/v4 v4.1.33
	github.com/quic-go/quic-go v0.65.0
	github.com/restic/chunker v0.5.0
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.1.0
	github.com/ulikunitz/xz v0.5.17
	golang.org/x/sys v0.50.0
	golang.org/x/term v0.48.0
	google.golang.org/protobuf v1.36.12
)

require (
	filippo.io/hpke v0.4.0 // indirect
	github.com/Microsoft/go-winio v0.6.4 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/containerd/errdefs v1.0.0 // indirect
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/cpuguy83/go-md2man/v2 v2.0.7 // indirect
	github.com/distribution/reference v0.6.0 // indirect
	github.com/docker/cli v29.9.3+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.9.12 // indirect
	github.com/docker/go-connections v0.8.2 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/moby/docker-image-spec v1.3.1 // indirect
	github.com/moby/moby/api v1.57.0 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/russross/blackfriday/v2 v2.1.0 // indirect
	github.com/sirupsen/logrus v1.10.2 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.72.0 // indirect
	go.opentelemetry.io/otel v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	go.uber.org/mock v0.6.0 // indirect
	go.yaml.in/yaml/v3 v3.0.6 // indirect
	golang.org/x/crypto v0.59.0 // indirect
	golang.org/x/mod v0.42.0 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/tools v0.51.0 // indirect
)
