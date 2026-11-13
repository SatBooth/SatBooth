# Monero v0.18.5.4 (monerod, monero-wallet-rpc, monero-wallet-cli) for the hub host (deploy/hub/compose.yml): the
# official release binaries, verified against pinned SHA-256 hashes, on debian:13-slim pinned by digest (plan 10,
# decision 5). v0.18.5.4 is the version qualified against (qualify/REPORT.md, "Monero end to end"); the hashes are those
# of dev/regtest/monero/Dockerfile, copied there from https://www.getmonero.org/downloads/hashes.txt after its PGP
# signature was verified. The download stage holds curl and bzip2; the final image holds the three binaries and the
# wallet-rpc entrypoint on the base. monero-wallet-cli is for the operator, who makes the view-only wallet with it
# (deploy/README.md). No configuration and no login is baked in.
FROM debian:13-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 AS download

ARG TARGETARCH
ARG XMR_SHA256_AMD64=ba2928962c101b609434bf8c1b43ad59ebe96c5525977284ca599d8bf0f02d10
ARG XMR_SHA256_ARM64=d20d71e6522b41eb8e549c3425f7f4622c9c9a448fc7147525393fd0a6b59703

RUN apt-get update \
 && apt-get install -y --no-install-recommends bzip2 ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) arch=x64; sha="${XMR_SHA256_AMD64}" ;; \
      arm64) arch=armv8; sha="${XMR_SHA256_ARM64}" ;; \
      *) echo "unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    f="monero-linux-${arch}-v0.18.5.4.tar.bz2"; \
    curl -fsSL -o "/tmp/${f}" "https://downloads.getmonero.org/cli/${f}"; \
    echo "${sha}  /tmp/${f}" | sha256sum -c -; \
    mkdir /out; \
    tar -xjf "/tmp/${f}" -C /out --strip-components=1 --wildcards '*/monerod' '*/monero-wallet-rpc' '*/monero-wallet-cli'

FROM debian:13-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
COPY --from=download /out/monerod /out/monero-wallet-rpc /out/monero-wallet-cli /usr/local/bin/
COPY --chmod=0755 wallet-rpc.sh /usr/local/bin/wallet-rpc.sh
# /data is monerod's data directory (a named volume: Docker copies this owner into it when it is first created).
RUN useradd --system --no-create-home --home-dir /data --uid 10002 monero \
 && install -d -o monero -g monero -m 0700 /data
USER 10002:10002
