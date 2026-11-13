# Release images: a static binary and nothing else. Build from the repository root:
#   docker build -f deploy/image/Dockerfile --target satbooth -t satbooth:local .
#   docker build -f deploy/image/Dockerfile --target satbooth-relay -t satbooth-relay:local .
#   docker build -f deploy/image/Dockerfile --target satbooth-tools -t satbooth-tools:local .
# What each image may hold is checked by scripts/image-check.sh; the reason for each file is in deploy/README.md.
FROM golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS build
WORKDIR /src
COPY . .
ENV GOFLAGS=-mod=vendor GOPROXY=off GOTOOLCHAIN=local CGO_ENABLED=0
RUN go build -trimpath -ldflags=-buildid= -o /out/satbooth ./cmd/satbooth \
 && go build -trimpath -ldflags=-buildid= -o /out/satbooth-relay ./cmd/satbooth-relay \
 && go build -trimpath -ldflags=-buildid= -o /out/satbooth-setup ./cmd/satbooth-setup \
 && go build -trimpath -ldflags=-buildid= -o /out/satbooth-pair ./cmd/satbooth-pair

FROM scratch AS satbooth
COPY --from=build /out/satbooth /satbooth
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
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
