# Release images: a static binary and nothing else. Build from the repository root:
#   docker build -f deploy/image/Dockerfile --target satbooth -t satbooth:local .
#   docker build -f deploy/image/Dockerfile --target satbooth-relay -t satbooth-relay:local .
#   docker build -f deploy/image/Dockerfile --target satbooth-tools -t satbooth-tools:local .
# The release build, reproducible for linux/amd64 and linux/arm64 (same inputs, same digests), is:
#   scripts/release-build.sh <output-directory>
# What each image may hold is checked by scripts/image-check.sh; the reason for each file is in deploy/README.md.
# The build stage runs on the build host's platform and cross-compiles, so no emulation runs the compiler.
FROM --platform=$BUILDPLATFORM golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY . .
ENV GOFLAGS=-mod=vendor GOPROXY=off GOTOOLCHAIN=local CGO_ENABLED=0
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags=-buildid= -o /out/satbooth ./cmd/satbooth \
 && GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags=-buildid= -o /out/satbooth-relay ./cmd/satbooth-relay \
 && GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags=-buildid= -o /out/satbooth-setup ./cmd/satbooth-setup \
 && GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags=-buildid= -o /out/satbooth-pair ./cmd/satbooth-pair

# The CA bundle comes from a fixed platform, so its bytes do not depend on the build host.
FROM --platform=linux/amd64 golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS certs

FROM scratch AS satbooth
COPY --from=build /out/satbooth /satbooth
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 65532:65532
ENTRYPOINT ["/satbooth"]

FROM scratch AS satbooth-relay
COPY --from=build /out/satbooth-relay /satbooth-relay
USER 65532:65532
ENTRYPOINT ["/satbooth-relay"]

# The setup tools: no entrypoint (deploy/setup.sh names the command), no CA bundle (they never use the network).
FROM scratch AS satbooth-tools
COPY --from=build /out/satbooth-setup /satbooth-setup
COPY --from=build /out/satbooth-pair /satbooth-pair
USER 65532:65532
