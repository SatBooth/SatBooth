# Monero v0.18.5.4 (monerod, monero-wallet-rpc, monero-wallet-cli), verified against pinned SHA-256 hashes (dev/regtest
# only; never a production image). The hashes are copied from https://www.getmonero.org/downloads/hashes.txt after its
# PGP signature (made 2026-10-16) was verified with binaryFate's key from utils/gpg_keys/binaryfate.asc at tag
# v0.18.5.4, primary key fingerprint 81AC 591F E9C4 B65C 5806 AFC3 F0AF 4D46 2A0B DF92.
FROM debian:13-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2

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
    tar -xjf "/tmp/${f}" -C /usr/local/bin --strip-components=1 --wildcards \
      '*/monerod' '*/monero-wallet-rpc' '*/monero-wallet-cli'; \
    rm "/tmp/${f}"

# /data is the wallet directory of the two wallet-rpc services (a named volume each: Docker copies this owner into it).
RUN useradd --system --create-home --home-dir /var/lib/monero --uid 10002 monero \
 && install -d -o monero -g monero -m 0700 /data
COPY monerod.conf /etc/monero/monerod.conf
COPY --chmod=0755 wallet-satbooth.sh /usr/local/bin/wallet-satbooth.sh
USER monero
EXPOSE 18081 18083 18084
