# Acknowledgements

SatBooth stands on other people's work. Everything it borrows, whether code, test data or a design, is
listed here, and new entries are added in the same change that borrows them.

| Project | What SatBooth takes from it | License |
|---|---|---|
| [btcec](https://github.com/btcsuite/btcd/tree/master/btcec), [btcd chainhash](https://github.com/btcsuite/btcd/tree/master/chainhash), [dcrd secp256k1](https://github.com/decred/dcrd/tree/master/dcrec/secp256k1) and [dcrd blake256](https://github.com/decred/dcrd/tree/master/crypto/blake256) | the secp256k1 curve and BIP-340 signing, vendored unmodified under `vendor/` | ISC |
| [golang.org/x/crypto](https://go.googlesource.com/crypto) (`chacha20`, `internal/alias`) and [golang.org/x/sys](https://go.googlesource.com/sys) (`unix`, for the SQLite driver and for `memlock`; `cpu`, built only on s390x) | ChaCha20 for NIP-44 and the SQLite driver's system calls, vendored unmodified under `vendor/` | BSD-3-Clause |
| [ncruces/go-sqlite3](https://github.com/ncruces/go-sqlite3) (`driver` and its internal packages) | the pure-Go `database/sql` SQLite driver for the ledger, vendored unmodified under `vendor/` | MIT |
| [ncruces/go-sqlite3-wasm](https://github.com/ncruces/go-sqlite3-wasm) | SQLite 3.54.1 translated to Go (by wasm2go), vendored unmodified; the original authors keep their copyright, SQLite itself is public domain | MIT-0 (the translation glue), public domain (SQLite) |
| [coder/websocket](https://github.com/coder/websocket) | the WebSocket transport of the relay, vendored unmodified | ISC |
| [ncruces/julianday](https://github.com/ncruces/julianday) | date conversion used by the SQLite driver, vendored unmodified | MIT |
| [BIP-173](https://github.com/bitcoin/bips/blob/master/bip-0173.mediawiki) (bech32, bitcoin/bips) | the bech32 checksum design (the polymod generator and the human-readable-part expansion) used by the syntax check of a refund claimant's invoice, and BIP-173's valid test strings, copied into `lninvoice/lninvoice_test.go` (`TestBech32Vectors`) | BSD-2-Clause |
| [libsodium](https://github.com/jedisct1/libsodium) (tag 1.0.18-RELEASE, commit 940ef42) | the list of small-order Ed25519 public-key encodings (`ge25519_has_small_order` in `crypto_core/ed25519/ref10/ed25519_ref10.c`), which `approval` refuses as approval keys; the seven entries are re-typed as data, no other code | ISC |
| [khatru](https://pkg.go.dev/fiatjaf.com/nostr/khatru) (in `fiatjaf.com/nostr`) | structure of the relay: connection and subscription handling, NIP-42 flow, OK/CLOSED prefixes (no code) | Unlicense |
| [rsc.io/qr](https://pkg.go.dev/rsc.io/qr) (v0.2.0) | our copy of the `coding` and `gf256` packages, in `qr/` (with their license), the QR encoder behind the checkout page | BSD-3-Clause |
| [ZXing](https://github.com/zxing/zxing) | three golden QR matrices from its `EncoderTestCase`, copied into `qr/testdata/` as independent test vectors | Apache-2.0 |
| [TigerBeetle](https://github.com/tigerbeetle/tigerbeetle) | the money model (immutable transfers between accounts, corrections as new transfers), field names, result codes and order of checks | Apache-2.0 |
| [strfry](https://github.com/hoytech/strfry) | the idea of keeping ephemeral events for a bounded window (no code) | GPL-3.0 |
| [Kubernetes](https://github.com/kubernetes/kubernetes) (the pod's `pause` container) | the idea of a container that only holds a network namespace for others to join, used for the hub and the relay in `deploy/hub/compose.yml` (no code) | Apache-2.0 |
| [BTCPay Server](https://github.com/btcpayserver/btcpayserver) | the rate model: a background fetcher with a cache (`BackgroundFetcherRateProvider`), a configurable margin (its per-store spread), the provider list, and the rule that a Kraken ticker counts only when its bid is at most its ask (`KrakenExchangeRateProvider`; no code); the median and the spread bound are SatBooth's own | MIT |
| [Yadio](https://yadio.io) | a rate source SatBooth can use; yadio.io asks to be cited with a link wherever its data is used or displayed: the checkout page links it on an order Yadio helped price, and an operator who shows its data anywhere else credits it there too | free with attribution |
| [BTCPay Server Monero plugin](https://github.com/btcpay-monero/btcpayserver-monero-plugin) | the attention to a sender's `unlock_time` (the plugin waits for the lock to pass; SatBooth treats such a payment as late), and the `monero:<address>?tx_amount=<xmr>` link (no code) | MIT |
| [Monero](https://github.com/monero-project/monero) (v0.18.5.4) | the RPC facts cited by file and line in `docs/` and the code; its release binaries for the regtest images (plan 8) and the hub bundle (plan 10); the header lines of monero-wallet-rpc's 401 answer, recorded from the official release into `xmrrpc/testdata/challenge.txt`, and the shapes of its answers and error codes, used as test fixtures for the wallet-rpc client | BSD-3-Clause |
| [Bitcoin Core](https://github.com/bitcoin/bitcoin) (v32.1) `share/rpcauth/rpcauth.py` | the format of the `rpcauth` line (`rpcauth=<user>:<salt>$<HMAC-SHA256 of the password, keyed by the salt's hex text>`) that `satbooth-setup` writes, and the test vectors in `setup/secret_test.go`, produced by running the script at v32.1 (no code) | MIT |
| [Rubik](https://github.com/googlefonts/rubik) 2.300 (the variable font at weight 800, ExtraBold; "Copyright 2015 The Rubik Project Authors") | the outlines of the letters in the wordmark of the logo (`docs/logo/satbooth-logo*.svg`), converted to paths; the font itself is not shipped | SIL Open Font License 1.1 |

Published examples of some standards are used only as test vectors. They are the official Schnorr vectors of
[BIP-340](https://github.com/bitcoin/bips/blob/master/bip-0340.mediawiki) (bitcoin/bips), copied unmodified into
`nostr/testdata/` (BSD-2-Clause); the official NIP-44 v2 vectors of
[paulmillr/nip44](https://github.com/paulmillr/nip44), copied unmodified into `nip44/testdata/` (no licence stated, used
as test data); vectors 1 and 5 of
[BIP-32](https://github.com/bitcoin/bips/blob/master/bip-0032.mediawiki) (bitcoin/bips), in
`descriptor/testdata/vectors.json` (BSD-2-Clause); the invoices of the examples of
[BOLT 11](https://github.com/lightning/bolts/blob/191462825ae341f0a27098765c075f01ca9bc6c2/11-payment-encoding.md)
(lightning/bolts, commit 1914628), copied unmodified into `lninvoice/testdata/bolt11.json` (CC-BY-4.0, Creative Commons
Attribution 4.0 International, per the repository README); and the HTTP Digest examples of
[RFC 2617](https://www.rfc-editor.org/rfc/rfc2617) §3.5 and [RFC 7616](https://www.rfc-editor.org/rfc/rfc7616) §3.9.1,
in `xmrrpc/digest_test.go` (Internet Society notice for RFC 2617; IETF Trust Legal Provisions for RFC 7616).

Recorded public API responses are used as parser fixtures: [Kraken](https://docs.kraken.com/api/docs/rest-api/get-ticker-information) (`rates/testdata/kraken.json` and
`rates/testdata/kraken-xmr.json`),
[Coinbase](https://docs.cdp.coinbase.com/coinbase-app/track-apis/exchange-rates) (`rates/testdata/coinbase.json`) and
[Bitfinex](https://docs.bitfinex.com/reference/rest-public-tickers) (`rates/testdata/bitfinex.json`).

SatBooth runs alongside [Alby Hub](https://github.com/getAlby/hub) (Apache-2.0),
[Bitcoin Core](https://github.com/bitcoin/bitcoin) and [Monero](https://github.com/monero-project/monero), and
relies on their documented and tested behaviour. The hub host's bundle also runs
[Fulcrum](https://github.com/cculianu/Fulcrum) (GPL-3.0-or-later) unmodified, as a separate program. The Go libraries
it links are listed, with their licenses
and records, in [`docs/dependencies.md`](docs/dependencies.md).
