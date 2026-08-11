# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# The main executable embeds the two platform self-extract helpers.
RUN make embed
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    GOARM="${TARGETVARIANT#v}" \
    go build -trimpath \
      -ldflags "-s -w -X github.com/manprint/backimage/internal/buildinfo.Version=${VERSION} -X github.com/manprint/backimage/internal/buildinfo.Commit=${COMMIT} -X github.com/manprint/backimage/internal/buildinfo.Date=${DATE}" \
      -o /out/backimage ./cmd/backimage
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/backimage /usr/local/bin/backimage
# Seed the mount point so a new named volume inherits the nonroot ownership.
COPY --from=build --chown=65532:65532 --chmod=0700 /out/data /data
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/backimage"]
