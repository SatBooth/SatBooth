# Bitcoin Core 32.1 for the hub host (deploy/hub/compose.yml): the official release binaries, verified against pinned
# SHA-256 hashes, on debian:13-slim pinned by digest (plan 10, decision 5). Bitcoin Core 32.1 is the version qualified
# against (qualify/REPORT.md, "On-chain end to end"); the hashes are those of dev/regtest/bitcoind/Dockerfile. The
# download stage holds curl; the final image holds only the two binaries on the base. No configuration is baked in: the
# operator's bitcoin.conf is bind-mounted read-only (deploy/README.md).
FROM debian:13-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 AS download

ARG TARGETARCH
ARG BTC_SHA256_AMD64=61545c17c3120fb7412f896d248b11993af04ec8817aad3649c55b2afb044963
ARG BTC_SHA256_ARM64=7ed37c7373eb6aa3e3f72842596a1afeba1b223717033cade9842b25773e1922

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) arch=x86_64; sha="${BTC_SHA256_AMD64}" ;; \
      arm64) arch=aarch64; sha="${BTC_SHA256_ARM64}" ;; \
      *) echo "unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    f="bitcoin-32.1-${arch}-linux-gnu.tar.gz"; \
    curl -fsSL -o "/tmp/${f}" "https://bitcoincore.org/bin/bitcoin-core-32.1/${f}"; \
    echo "${sha}  /tmp/${f}" | sha256sum -c -; \
    mkdir /out; \
    tar -xzf "/tmp/${f}" -C /out --strip-components=2 "bitcoin-32.1/bin/bitcoind" "bitcoin-32.1/bin/bitcoin-cli"

FROM debian:13-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
COPY --from=download /out/bitcoind /out/bitcoin-cli /usr/local/bin/
# /data is the data directory (a named volume: Docker copies this owner into it when it is first created).
RUN useradd --system --no-create-home --home-dir /data --uid 10001 bitcoin \
 && install -d -o bitcoin -g bitcoin -m 0700 /data
USER 10001:10001
ENTRYPOINT ["bitcoind", "-conf=/etc/bitcoin/bitcoin.conf", "-datadir=/data", "-printtoconsole"]
