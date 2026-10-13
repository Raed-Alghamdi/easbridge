# syntax=docker/dockerfile:1.27.1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
#
# easbridge: IMAP/SMTP in front of Exchange ActiveSync.
#
#   docker build -t local/easbridge:0.3.0 --build-arg VERSION=0.3.0 .
#
# Static binary (CGO off, no libc), distroless nonroot runtime, no shell.
#
# Everything the build pulls is pinned by digest (the multi-architecture index), so
# that a rebuild cannot silently pick up other bytes than the ones that were
# reviewed: the BuildKit frontend named on the first line (docker/dockerfile 1.27.1),
# the builder (golang 1.27.2, the Go release the project-local toolchain uses) and the
# runtime (distroless static-debian12:nonroot). The tag in front of each digest only
# says what it was on 2026-10-09. To update, look up the digest of the new tag
# (`docker buildx imagetools inspect <image>:<tag>`, or a GET of its manifest at the
# registry) and change it here in a commit of its own, together with go.mod if the
# Go release changes.

# The build stage runs on the machine that builds (BUILDPLATFORM) and cross-compiles for the image's platform
# (TARGETARCH): an arm64 image is built by a native amd64 Go, not by Go under emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c AS build
WORKDIR /src
# Dependencies first, for layer caching. The vendored go-activesync is a
# replace target, so it must exist before `go mod download`.
COPY go.mod go.sum ./
COPY third_party ./third_party
RUN go mod download
COPY . .
ARG VERSION=0.3.0
# IMAGE overrides where `easbridge init` says the image lives (internal/version.Image); empty keeps
# the default in the source. GOTAGS is empty for the real image: the production binary cannot contain
# the fake upstream. `make e2e` builds a test image with GOTAGS=devfake; never publish that one.
ARG IMAGE=
ARG GOTAGS=
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath ${GOTAGS:+-tags "$GOTAGS"} \
      -ldflags "-s -w -X easbridge/internal/version.Version=${VERSION} ${IMAGE:+-X easbridge/internal/version.Image=$IMAGE}" \
      -o /out/easbridge ./cmd/easbridge \
 && mkdir -p /out/data /out/ca && chmod 0700 /out/data && chmod 0755 /out/ca

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/easbridge /easbridge
# The state directory exists in the image, owned by the nonroot user (uid 65532), mode 0700,
# and is declared a VOLUME after that: a named Docker volume is initialised from it and gets
# the same owner without a chown. A host directory mounted over it must be writable by uid
# 65532. /ca is the same for the exported CA certificate (EASBRIDGE_CA_EXPORT_DIR=/ca in the
# compose file `easbridge init` writes). No EASBRIDGE_TLS_* variable is set here: with none, and
# no files at /tls, the bridge makes its own certificate; files mounted at /tls are used as they are.
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build --chown=65532:65532 /out/ca /ca
USER 65532:65532
ENV EASBRIDGE_DATA_DIR=/data
VOLUME ["/data"]
# The listeners (IMAPS 993, SMTPS 465, CardDAV 8443) are deliberately not EXPOSEd: nothing should publish them.
# Attach the container to the private network MailFlow's backend uses.
# distroless has no shell or curl, so the binary checks itself.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD ["/easbridge", "healthcheck"]
ENTRYPOINT ["/easbridge"]
CMD ["serve"]
