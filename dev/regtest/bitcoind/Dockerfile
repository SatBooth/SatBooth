# Bitcoin Core 32.1, verified against pinned SHA-256 hashes (dev/regtest only; never a production image).
FROM debian:13-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2

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
    tar -xzf "/tmp/${f}" -C /usr/local/bin --strip-components=2 \
      "bitcoin-32.1/bin/bitcoind" "bitcoin-32.1/bin/bitcoin-cli"; \
    rm "/tmp/${f}"

RUN useradd --system --create-home --home-dir /var/lib/bitcoin --uid 10001 bitcoin
USER bitcoin
COPY --chown=bitcoin:bitcoin bitcoin.conf /var/lib/bitcoin/.bitcoin/bitcoin.conf
EXPOSE 18443
ENTRYPOINT ["bitcoind", "-printtoconsole"]
