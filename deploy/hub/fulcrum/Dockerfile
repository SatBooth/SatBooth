# Fulcrum 2.1.4, the Electrum server of the hub bundle (deploy/hub/compose.yml): the hub's on-chain address lookups go
# to it instead of Alby's server. The official release binaries are checked against pinned SHA-256 hashes, copied from
# Fulcrum-2.1.4-shasums.txt after its PGP signature was verified with Calin Culianu's key D465 135F 97D0 047E 18E9 9DC3
# 2181 0A54 2031 C02C (published in Electron-Cash/Electron-Cash pubkeys/calinkey.txt): gpg reported "Good signature" for
# the shasums file from that key (docs/dependencies.md §8). They are copied
# onto debian:13-slim, pinned by digest, as the node images are (plan 10, decision 5). The download stage holds curl;
# the final image holds the binary on the base, whose libraries it links against (libbz2, zlib, glibc). No configuration
# and no password is baked in.
FROM debian:13-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 AS download

ARG TARGETARCH
ARG FULCRUM_SHA256_AMD64=faf7cb36266c1694c834ad551d9592c0aef357bca66fdb5efdfe0d131c4b38ae
ARG FULCRUM_SHA256_ARM64=bc9cda576bf889222e89c7616012e628597e4921f9116eebc759dfc854785b84

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) arch=x86_64; sha="${FULCRUM_SHA256_AMD64}" ;; \
      arm64) arch=arm64; sha="${FULCRUM_SHA256_ARM64}" ;; \
      *) echo "unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    f="Fulcrum-2.1.4-${arch}-linux.tar.gz"; \
    curl -fsSL -o "/tmp/${f}" "https://github.com/cculianu/Fulcrum/releases/download/v2.1.4/${f}"; \
    echo "${sha}  /tmp/${f}" | sha256sum -c -; \
    mkdir /out; \
    tar -xzf "/tmp/${f}" -C /out --strip-components=1 "Fulcrum-2.1.4-${arch}-linux/Fulcrum" \
      "Fulcrum-2.1.4-${arch}-linux/LICENSE.txt"

FROM debian:13-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
COPY --from=download /out/Fulcrum /usr/local/bin/Fulcrum
COPY --from=download /out/LICENSE.txt /usr/share/doc/fulcrum/LICENSE.txt
# /data is Fulcrum's database (a named volume: Docker copies this owner into it when it is first created).
RUN useradd --system --no-create-home --home-dir /data --uid 10004 fulcrum \
 && install -d -o fulcrum -g fulcrum -m 0700 /data
# Qt warns and falls back without a UTF-8 locale; C.UTF-8 is built into glibc.
ENV LANG=C.UTF-8
USER 10004:10004
ENTRYPOINT ["Fulcrum"]
