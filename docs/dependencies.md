# SatBooth dependencies and supply chain

Goal: as little third-party code as possible, and every line of what remains reviewed, pinned and
vendored. The language is Go and SatBooth is self-hosted. Everything below was measured
on 2026-11-02 against Go 1.27.3, the module proxy (proxy.golang.org), the OSV database (api.osv.dev) and
the upstream sources named. Re-check before relying on it.

## 1. Let the nodes do the hard parts

The leanest dependency is the one we never need. Each rail already runs software that does the
cryptography, so SatBooth calls it over RPC and holds no Bitcoin, Lightning or Monero crypto code itself.

| Need | Done by | Source |
|---|---|---|
| Validate a descriptor and detect private keys | bitcoind `getdescriptorinfo` (`checksum`, `isrange`, `hasprivatekeys`) | `src/rpc/output_script.cpp`, bitcoin/bitcoin master |
| Derive the address for invoice *n* | bitcoind `deriveaddresses <desc> [n,n]` | same file |
| Watch payments without being able to spend | a bitcoind wallet created with `disable_private_keys=true`, then `importdescriptors` | `src/wallet/rpc/wallet.cpp` |
| Monero subaddress per invoice, incoming transfers | `monero-wallet-rpc` (view-only wallet) | design.md §2.3 |
| Create and look up Lightning invoices | Alby Hub over NWC | design.md §2.1 |
| Check a claimant's BOLT11 for a payout | Alby Hub, see §4 | `transactions/transactions_service.go` |

So SatBooth needs **no BOLT11 decoder, no BIP-32 derivation and no Monero crypto**. The one piece of bech32 it has is
the BIP-173 checksum in the claimant invoice's syntax check (`lninvoice`, §5), which decodes nothing.
For scale: the Go BOLT11 decoder the hub itself uses, `github.com/nbd-wtf/ln-decodepay`, requires all of
`lnd` and with it etcd, gRPC, OpenTelemetry, Docker client libraries and three SQL drivers (its go.mod
for v1.13.0 lists 5 direct and 99 indirect modules).

## 2. What is left, and what covers it

| Capability | Covered by |
|---|---|
| HTTP server and client, JSON, timeouts, size caps | stdlib `net/http`, `encoding/json` |
| JSON-RPC to bitcoind (cookie or basic auth) | stdlib |
| JSON-RPC to monero-wallet-rpc (`--rpc-login` is HTTP Digest, which stdlib lacks) | own Digest client (about 80 lines, RFC 7616 vectors). **Built (plan 8):** `xmrrpc/digest.go` is 224 lines with its comments, about three times the estimate, because it parses the challenge strictly (one grammar, a seeded property test and a fuzz target) and keeps the nonce state; the client around it, `xmrrpc/client.go`, is 429 lines (see §5) |
| Webhook signatures (HMAC-SHA256 with timestamp) | stdlib `crypto/hmac`, `crypto/sha256` |
| Payout approvals from the admin system | stdlib `crypto/ed25519`, independent of the Nostr keys. **Built (plan 9):** `approval` (see §5) |
| Rate limits, token buckets, global caps | own code (small) |
| Logging | stdlib `log/slog` |
| Nostr event id and BIP-340 Schnorr signature | **third party**: `btcec/v2/schnorr` (see §3) |
| NIP-44 v2 encryption | own code on stdlib `crypto/hkdf`, `crypto/hmac`, `crypto/sha256` plus **`x/crypto/chacha20`** (see §3) |
| WebSocket client to the relay | **third party**: `coder/websocket` (see §3) |
| Storage | SQLite. Driver: `ncruces/go-sqlite3` (pure Go); see §6 |
| QR code on the checkout page | own copy of `rsc.io/qr`'s encoder, rendered to SVG on the server (see §6) |

What the Go 1.27 standard library does *not* have, checked with `go list std`: secp256k1 or Schnorr
(`crypto/ecdh` has only P-256, P-384, P-521 and X25519), a public ChaCha20 (it exists only as
`vendor/golang.org/x/crypto/chacha20`, which cannot be imported), a WebSocket client, and SQLite.
`crypto/hkdf` is public.

## 3. The proposed third-party set (measured)

A probe module that imports exactly `btcec/v2/schnorr`, `coder/websocket` and `x/crypto/chacha20` builds
with `CGO_ENABLED=0 -trimpath` and pulls in **13 non-std packages from 6 modules, 16,720 vendored lines
of Go** (non-test files of the vendored packages):

| Module | Version (date) | Its go.mod requires | OSV records | Non-test Go lines (whole module) |
|---|---|---|---|---|
| `github.com/btcsuite/btcd/btcec/v2` | v2.5.1 (2026-10-21) | `chainhash/v2`, `decred secp256k1/v4`; test-only: testify, go-spew | none | 4,128 (`schnorr/`: 619) |
| `github.com/decred/dcrd/dcrec/secp256k1/v4` | v4.4.1 (2026-02-24) | `decred/dcrd/crypto/blake256` only | none | 7,509 |
| `github.com/btcsuite/btcd/chainhash/v2` | v2.0.0 | | | |
| `github.com/decred/dcrd/crypto/blake256` | v1.1.0 | | | (compiled in because btcec's schnorr imports decred's schnorr package; unused by us) |
| `golang.org/x/crypto` (only `chacha20`, `internal/alias`) | v0.58.0 (2026-10-09) | x/sys (only on some CPU architectures for chacha20) | 58 records (GO ids plus their GHSA aliases), **none in `chacha20`**: they name `ssh` (and `agent`, `knownhosts`), `openpgp/*`, `salsa20/salsa`, `cryptobyte`, `acme/autocert` | `chacha20/`: 534 |
| `golang.org/x/sys` (only `cpu`) | v0.49.0 (2026-10-06) | none | 2 advisories, 3 ids: GO-2022-0493 = GHSA-p782-xgp4-8hr8 (`x/sys/unix`, fixed 2022) and GO-2026-5024 (`x/sys/windows`, fixed v0.44.0); none in `cpu`; all fixed before v0.49.0 | `cpu/`: 3,011; only built on s390x; no shipped target (linux/amd64, linux/arm64, darwin/arm64) builds it, so it is not on the allowlist |
| `github.com/ncruces/go-sqlite3` (only `driver`, `vfs` and the helper packages they import) | v0.36.0 (2026-10-29) | `go-sqlite3-wasm/v6`, `julianday`, x/sys. The driver's own go.mod also lists `ncruces/sort` and `ncruces/wbt`, but the packages we import (`driver`, `vfs`) do not need them, so they are not in our module graph. `internal/testenv` (wasm2go harness hooks, nil variables) is compiled in via `sqlite3_wrap/wrap_sys.go`; the rest are optional-extension or test-only modules | none | 12,256 |
| `github.com/ncruces/go-sqlite3-wasm/v6` | v6.4.35401 (2026-10-29) | `ncruces/wasm2go` v0.4.19 and `golang.org/x/tools` v0.52.0 as tool requirements (not built or vendored) | none | 160,604 (SQLite 3.54.1 machine-translated by wasm2go, plus a 103 KB `sqlite3.dat` data file) |
| `github.com/ncruces/julianday` | v1.0.0 (2023-10-18) | none | none | 124 |
| `golang.org/x/sys` (now also `unix`; `memlock` imports it directly since plan 5, for `Mlockall` and `Prctl`, so it is no longer only the driver's) | v0.49.0 (2026-10-06) | none | see the row above; none in `unix` at v0.49.0 | `unix/`: 191,946 (one package, only the part for the target OS is compiled) |
| `github.com/coder/websocket` | v1.8.15 (2026-06-15) | none | none | 4,524 |

**Added in plan 2 (vendored):** `ncruces/go-sqlite3` (`driver`, `vfs` and three helper packages), `go-sqlite3-wasm/v6`,
`julianday` and `x/sys/unix`, for the ledger (§6 item 1). OSV was queried for each module at its version
on 2026-11-02 (`POST api.osv.dev/v1/query`): no records. As with any SQLite, an empty result means little,
because SQLite's own CVEs are filed in NVD; the pinned SQLite is 3.54.1. The SQL is ours and parameterised,
and the database file is never attacker-supplied.

**Added in plan 3 (vendored):** `coder/websocket` v1.8.15 (ISC-style license, `LICENSE.txt`), the WebSocket
transport of `satbooth-relay`, with its three internal packages (`bpool`, `errd`, `util`). Its go.mod requires nothing.
OSV (`POST api.osv.dev/v1/query`, 2026-11-02): no records for the module, with or without the version. The standard
library has no WebSocket server or client, and RFC 6455 framing, fragmentation, control frames and the close
handshake are a network-input parser we would rather not own. The relay uses `Accept` with default options (the
Origin check stays on; the hub sends no Origin header, and a request without one passes), `Read`, `Write`, `Ping`
and `Close`.

**Added in plan 1 (vendored):** `btcec/v2` (with `schnorr`), `decred secp256k1/v4`, `chainhash/v2` and
`blake256`, and `x/crypto/chacha20` with `internal/alias` (NIP-44). On s390x, `chacha20` also imports `x/sys/cpu` (vendored, not built on other architectures).

Why these and not the alternatives:

- **Schnorr:** `btcec/v2/schnorr` is the BIP-340 code used by btcd and lnd. Decred's own `schnorr`
  package is *not* BIP-340 (it implements Decred's variant). The alternative is our own BIP-340 on top of
  `decred secp256k1/v4` primitives, checked against the official vectors
  (`bitcoin/bips/bip-0340/test-vectors.csv`). It would drop three packages but make SatBooth own signing
  code for a key that can spend the payout budget. **SatBooth uses btcec.**
- **WebSocket:** `coder/websocket` has no dependencies and no OSV records. `gorilla/websocket` (last
  release v1.5.3, 2024-06-14) has two records, the latest from 2026-08: a weak PRNG for the mask key.
  Writing our own RFC 6455 client is possible, but it is a network-input parser with fragmentation,
  control frames and close handshakes. Recommendation: `coder/websocket`.
- **NIP-44 versus NIP-04:** the hub accepts both (`nip47/cipher/cipher.go`: `"nip44_v2 nip04"`). NIP-04
  would drop `x/crypto` entirely (AES-CBC is stdlib), but it is deprecated and has no authentication at
  the cipher layer. **NIP-44 v2 only**, own code, tested against the official vectors
  (`paulmillr/nip44/nip44.vectors.json`). SatBooth contains no NIP-04 code.
- **Not a Nostr library.** `github.com/nbd-wtf/go-nostr` v0.52.3 requires 34 direct and 54
  indirect modules (badger, lmdb, fasthttp, sonic, four SQLite drivers, markdown, etc.). SatBooth needs
  four NIP-01/NIP-47 message shapes, which we write ourselves.

## 4. Payout checks: what the hub enforces, and what that rests on

From `getAlby/hub` at commit `921003b` (the v1.24.6 release), `SendPaymentSync` in
`transactions/transactions_service.go`:

- If `pay_invoice` carries `amount` and the invoice has an amount, they **must be equal**, or the hub
  refuses ("amount cannot be specified for an invoice that already has an amount"). If the invoice is
  amountless, the hub pays our `amount` (SatBooth refuses amountless claimant invoices anyway: below). So SatBooth
  **always sends `amount` = the approved amount**, and the hub pays exactly that or nothing.
- An expired invoice is refused.
- A second payment of the same payment request is refused when one is settled ("this invoice has
  already been paid") **or pending** ("there is already a payment pending for this invoice"). That
  answers the "check the pending case" note in design.md §2.1.
- The routing fee is **not** settable over NWC: the hub caps it at max(1 %, 10 sats)
  (`CalculateFeeReserveMsat`, passed to LDK as `MaxTotalRoutingFeeMsat`). The connection's budget counts
  amount plus that reserve.
- Paying one of the hub's **own** invoices is not refused: the hub intercepts and settles it internally
  (`interceptSelfPayment`). Because the amount must match, this is an accounting anomaly (a refund
  "paid" with no money leaving, an order "paid" with no money arriving), not theft. SatBooth refuses it:
  first by comparing against every invoice string it has issued (lowercased), then with `lookup_invoice`
  on the `invoice` connection, where a result of type **incoming** means ours. "Found" alone isn't
  enough: for a connection that isn't isolated, `LookupTransaction` searches every app's rows, so a
  claimant's invoice that we already paid is found too, as *outgoing*. The local string compare is the
  primary check because the hub's own self-payment test matches the payment hash **and** the exact
  payment request (`SendPaymentSync`).
- `lookup_invoice` decodes the given invoice only to get its payment hash and searches the hub's own
  rows. For a claimant's invoice it says "not found" until we pay it. After a timeout, SatBooth looks the
  payment up with `lookup_invoice {invoice}` on the `payout` connection instead of paying again.

- A second payment is **not** refused by the hub once the first row is `failed`, and a `failed` outgoing row
  can settle later if LDK reports success (`transactions_service.go:913-927, 967`). Only LDK's own duplicate
  check stands then (ldk-node HEAD `src/payment/bolt11.rs:102-108`; its payment records are deleted after
  24 h, `lnclient/ldk/ldk.go:2161-2195`). Plan 4 could not produce this path on regtest (15 stops and kills,
  all `pending`), so it is unverified.

**The qualified hub.** `ghcr.io/getalby/hub:v1.24.6@sha256:4bb27770ff2b68ab8342f75909dfcb59db0ca8f903045967e44c908b7f6f85f7`
(the multi-arch index digest). The tag `v1.24.6` points at commit `921003b`, so the source citations in this
section and the image are the same code. The qualification suite asserts the facts marked observed below and passed in
full against this image; the timings and counts quoted are from the earlier v1.24.4 run (`qualify/REPORT.md`).

| Fact the client relies on | Source (`getAlby/hub 921003b`) | Observed (qualification) |
|---|---|---|
| Payment errors are `INTERNAL` with text; the hub never sends `PAYMENT_FAILED`; unknown method is `INTERNAL` | `nip47/controllers/map_nip47_error.go:12-32`, `constants/constants.go:60-72`, `nip47/event_handler.go:335-346` | the texts quoted above, and "received payment failed event: RecipientRejected" |
| NIP-44 notifications are kind 23197; a NIP-04 23196 copy is also sent | `nip47/models/models.go:12-13`, `nip47/notifications/nip47_notifier.go:137-145, 190-197` | not observed (the client received a notification 0.08 to 0.12 s after the payment started, but the kind was not checked here) |
| Notifications go to every non-isolated app with the `notifications` scope | `nip47_notifier.go:108-116` | in part: an invoice made through the hub's own API produced `payment_received` on the invoice connection, none on the payout connection (no `notifications` scope) |
| A failed notification publish is logged and dropped, never retried; the queue is in memory | `nip47_notifier.go:137-145, 224-231`, `nip47/nip47_service.go:69-86`, `notifications/nip47_notification_queue.go:20-33` | no `payment_received` when the test looked, right after the hub's first answered `get_info` (5.3 to 5.6 s after the relay came back) and again after one `lookup_invoice`; a longer wait was not measured, so "no retry" holds for those seconds only, and `lookup_invoice` said `settled`; the queue after a hub restart was not measured |
| `lookup_invoice` and `list_transactions` search all apps' rows for apps that are not isolated | `transactions_service.go:606-640, 682-684`, `controllers/lookup_invoice_controller.go:62`, `list_transactions_controller.go:54` | in part: an invoice made through the hub's own API was found by `lookup_invoice` on the invoice connection; `list_transactions` not tested |
| `list_transactions` filters and orders by `updated_at`; default only `SETTLED` | `transactions_service.go:686-692, 730-745` | not tested |
| `expired` is synthesized for any unsettled row past `expires_at`, outgoing too | `nip47/models/transactions.go:28-31`, `transactions_service.go:399-403` | incoming row read `expired`; an outgoing one was not produced |
| Budget 0 means no budget; usage is amount + fee + fee reserve of settled and pending rows | `transactions_service.go:1196`, `db/queries/get_budget_usage.go:11-23, 44-45`, `get_budget_controller.go:47-54` | reserve max(1%, 10 sat); over-budget is `QUOTA_EXCEEDED`; a cancelled hold invoice returns the budget |
| `pay_invoice` waits for LDK and has no hub timeout | `lnclient/ldk/ldk.go:662-712` | answers in 0.09 to 0.15 s on a direct channel; the 50 s LDK limit was not measured |
| The hub ignores `expiration`; it drops `pay_invoice` and `make_invoice` requests older than 6 h silently | `event_handler.go:348-360` | not tested |
| A request from a key with no app gets no response | `event_handler.go:73-82` | no response; the client timed out at 15 s |
| Request ids are deduplicated in the database; only the newest 1000 are kept | `db/migrations/202407151352_autoincrement.go:37`, `event_handler.go:57-66`, `service/service.go:168-178, 297-326` | a replay after a hub restart got no answer and the log line "Event already processed" |
| `get_info` without the `get_info` scope omits `network` and block height | `nip47/controllers/get_info_controller.go:69-72` (the guard `HasPermission(app, GET_INFO_SCOPE)`); the node fields, `network` and block height among them, are filled only inside it (74-105). SatBooth depends on this: the start-up check of the network (plan 14) needs the scope on both of its connections, so both pairing links ask for it. The network is the hub's own string, with only `bitcoin` mapped to `mainnet` (`get_info_controller.go:91-95`; LDK reports its `NETWORK` setting raw, so a hub set to `testnet` runs a signet node and states `testnet`). With the scope, `get_info` calls the node and can fail (`get_info_controller.go:78-89`), which refuses the start with the same reason as a timeout | confirmed |
| The wallet pubkey is per app; a supplied `pubkey` makes the hub return no secret; the hub refuses a supplied `pubkey` that is not 64 lowercase hex digits forming a valid x-only key | `service/keys/keys.go:102-103, 145-147`, `apps/apps_service.go:81-99, 148-158` | not observed |
| The admin API shares one limiter, 1 request/s, burst 2 | `http/http_service.go:100-114, 178` | not observed |

**This is hub behaviour beyond NIP-47, so it is a coupling.** SatBooth is qualified against one pinned
hub version, and `scripts/qualify.sh` runs on every hub upgrade, before that upgrade goes into service.
What it asserts (`qualify/REPORT.md`): a `pay_invoice` whose amount differs from the invoice's is refused
by the hub ("WalletError codes and messages as observed"), and a claim with such an invoice is refused by
SatBooth at claim with no hub call (`TestRefundPay`); an expired invoice and a second payment of a settled
invoice are refused ("WalletError codes"), and so is a second payment while one is pending ("Payouts in
flight"); an invoice the hub already knows ends the refund `invoice_known` (`TestRefundHubKnown`); SatBooth's
own invoice is refused at claim (`TestRefundOwnInvoice`). **Amountless invoices** cannot be qualified: the
pinned hub cannot issue one (its `MakeInvoice` refuses an amount below 1 sat,
`transactions/transactions_service.go:180-181` at `921003b`, on both `POST /api/invoices` and NWC
`make_invoice`). So SatBooth refuses an amountless claimant invoice at claim (`invalid_invoice`, asserted by
`refund.TestClaimInvoiceAmount`), deny by default, although the hub source above would pay one our `amount`.

SatBooth itself checks the claimant string's **syntax**: lowercase, `lnbc` (or the configured
network's prefix), bech32 character set, a length cap, no `lightning:` prefix and no whitespace. The one
field it reads is the amount in the human-readable part: an invoice that carries no amount or an amount other
than the approved one is refused at claim (`invalid_invoice`), so it never reaches the hub's refusal above. Both get
seeded property and fuzz tests.

**No sub-wallet.** The `payout` connection gets a lifetime budget (renewal
`never`) that a person raises in the hub when it runs low. That is the same hard cap as a refund float,
without a funding payment for each top-up. An isolated sub-wallet's balance is what has been paid into
it minus what it has paid out (`db/queries/get_isolated_balance.go`).

## 5. Our own code, and how each piece is tested

| Code | Why it is ours | Tests |
|---|---|---|
| NIP-01 event serialisation (`[0,pubkey,created_at,kind,tags,content]`) and id | Go's `encoding/json` cannot produce it: even with `SetEscapeHTML(false)` it writes U+2028/U+2029 as ` `/` ` and replaces invalid UTF-8 with U+FFFD (tested on 1.27.3). Either changes the id. | Seeded fuzz over every character class; ids and signatures checked against events the hub itself emitted |
| NIP-44 v2 | A few dozen lines over stdlib and `chacha20` | All official vectors, including the invalid ones; fuzz for padding and length bounds |
| NIP-47 request/response/notification types | Four or five message shapes | Fuzz the decoders; every response built from an allowlist of fields |
| Descriptor intake (`descriptor`) | Must refuse private keys **before** anything reaches bitcoind. Base58Check is about 50 lines over `math/big` and `crypto/sha256`, decode only, so no module is added | A hand-written **allowlist scanner** (only `wpkh(...)` and `tr(...)` with one `xpub`/`tpub` key, an optional key origin and a `/0/*` path), not a blocklist and not a regexp; a private-material scan (`xprv`-family prefixes, base58 runs that decode as extended private keys or WIF) runs first over the raw bytes; then `hasprivatekeys` must be false; then the wallet is created with `disable_private_keys`. No capability beyond the standard library: the package has none in `docs/capslock.json`. Tests: BIP32 test vector 1 (xpub, xprv), BIP32 test vector 5 (invalid keys), SLIP-132 versions, WIFs and testnet keys derived from vector 1 and pinned by derivation tests, 5,000 seeded round trips, 5,000 mutants, 5,000 key-character mutations that must all fail, `FuzzParse` and `FuzzDecodeCheck` |
| Refund approvals (`approval`) | The approval and void layouts are SatBooth's own (threat model §4), and stdlib `crypto/ed25519` signs and verifies them, so no module is added. The configured keys are also checked against libsodium's list of small-order Ed25519 public-key encodings (seven 32-byte values, re-typed as data from libsodium 1.0.18, ISC; `ACKNOWLEDGEMENTS.md`): with such a key configured, a forged signature verifies for any message | The spec vectors (`approval/testdata`), the small-order list refused as keys, a forgery with a small-order key refused, a seeded property test (`TestProperty`) and `FuzzVerify` over every field and length; floats kept out (`rates/nofloat_test.go`) |
| Claimant invoice syntax check (`lninvoice`) | The hub checks the invoice itself (§4); SatBooth only refuses, before it stores it, a malformed string, another network's, or one that does not carry exactly the approved amount: the human-readable part and a BIP-173 checksum (polymod constant 1, no 90-character limit) over the bech32 set, and the amount in the human-readable part (`AmountMsat`), about 180 lines on the standard library. Nothing in the data part is decoded | BOLT 11's example invoices as test vectors (lightning/bolts, CC-BY-4.0; `ACKNOWLEDGEMENTS.md`), with signet and regtest re-encodings; BIP-173's checksum vectors; a seeded property test (`TestProperty`) and `FuzzCheck` over every boundary (length, case, the separator, the network codes, the checksum); `TestAmountMsat`, the seeded `TestAmountMsatProperty` against a `math/big` oracle and `FuzzAmountMsat` (each multiplier, a `p` amount that is not a whole msat, a leading zero, zero, 2^53 ± 1 msat, the longest digit run) |
| bitcoind JSON-RPC client and result decoders (`btcrpc`) | Core speaks JSON-RPC 1.0 over HTTP with named params and basic auth: `net/http` and `encoding/json` are enough, and a library would add a dependency to the money path. Own code gives a method allowlist (the `rpcwhitelist` set, refused before any request), an error type that keeps the code and drops bitcoind's message (it can echo a key back), exact decimal amounts (`ParseBTC`, never `float64`) and per-entry dropping of odd `listsinceblock` entries. Capslock: NETWORK (the client) and FILES (the password file read, 0600, no symlink); the other classes (ARBITRARY_EXECUTION, REFLECT, RUNTIME, SYSTEM_CALLS, UNANALYZED, UNSAFE_POINTER) come from `net/http`, `encoding/json` and `os`, as in `nwc` and `store` | `httptest` server for the request shape, errors, timeout and an 8 MiB + 1 body; every typed call against Core-shaped answers; seeded round-trip of 5,000 amounts, `FuzzParseBTC`, `FuzzParseSignedBTC`, `FuzzDecode` (envelope and every decoder), `FuzzLoadPassword`, `FuzzValidators`, and a mutation sweep over the canned answers |
| Fiat pricing (`rates`) | An exact decimal (an integer of units at scale 10^18 on `math/big`), the one conversion per base unit (`Convert`, a ceiling over big integers, with `Sats` for BTC and, since plan 8, `Piconero` for XMR), the median and spread rules, five small JSON parsers (Kraken, Coinbase, Yadio, the custom API and, since plan 8, Bitfinex) and a background fetcher with a cache, per asset (BTC, and XMR with the `monero` block). Money must never pass through floating point, and the client must follow the outbound rules of threat model §10 (HTTPS only, no redirect, no proxy, bounded body, a fixed `User-Agent`): `math/big`, `net/http`, `crypto/tls` and `encoding/json` from the standard library are enough, so no module is added. Capslock: NETWORK (the client), plus the set every package that logs through `log/slog` carries and RUNTIME from `context.WithTimeout` | Seeded property tests and fuzz targets for every parser (`FuzzParseDecimal`, `FuzzParsePercent`, `FuzzSats`, `FuzzConvert`, `FuzzParseKraken`, `FuzzParseCoinbase`, `FuzzParseYadio`, `FuzzParseCustom`, `FuzzParseBitfinex`); recorded live answers as fixtures, checked against each other (`TestFixturesAgree`); the client against `httptest` servers and the fake rate server `rates/ratestest` (redirects, statuses, timeouts, a 256 KiB + 1 body, and a sentinel that must never reach an error); the median, spread and freshness at every boundary; the fetcher under `-race` with stalled, failing and lying providers; a source scan that keeps floats out of `rates`, `api`, `store`, `checkout`, `rail`, `xmrrpc` and `monero`; and a by-hand live test (build tag `live`) |
| monero-wallet-rpc JSON-RPC client and HTTP Digest (`xmrrpc`) | wallet-rpc speaks JSON-RPC 2.0 over HTTP with Digest authentication only (epee's `http_server_auth`: MD5, `qop=auth`), and the standard library has no Digest client; `crypto/md5`, `net/http` and `encoding/json` are enough, so no module is added. Own code gives a method allowlist (the eight methods SatBooth calls, refused before any request), one request in flight (epee keeps the Digest nonce and counter per connection, `http_protocol_handler.h:190` and `:227` at v0.18.5.4, and the client keeps one state for its pooled connections, so one call at a time keeps its `nc` in step), one retry on a 401 and never more, a refusal of a wallet-rpc that asks for no login, an error type that keeps the code and drops wallet-rpc's message, exact integer amounts (never `float64`), per-entry dropping of odd `get_transfers` entries, and the address shape rule (never decoded). It imports `btcrpc` for the URL and user-name rules only; the password is loaded by `service` with `btcrpc.LoadPassword`. Capslock: the same set as `btcrpc` and `rates`: NETWORK (the client) and the classes `net/http` and `encoding/json` bring; FILES there is capslock's over-approximation (an `io.Writer` call inside `encoding/json` that it resolves to the SQLite driver's writer), as in `rates`, not a file read | RFC 2617 §3.5 and RFC 7616 §3.9.1 (MD5) vectors; wallet-rpc v0.18.5.4's real 401 header lines, recorded into `xmrrpc/testdata/challenge.txt`, served by an `httptest` server that checks Digest as epee does, but with one nonce and counter for all connections, which is stricter than epee's per-connection state (`nc` equal to its counter, stale on an old nonce); 50 concurrent calls with no overlap; the allowlist, one retry, a stale nonce, redirects, an 8 MiB + 1 body, timeouts, the proxy environment and every rendering of a `Client` (the password and the Digest state sit two pointers away; `Config` holds no secret), and a raw listener whose malformed answers must reach no error text; every typed call against wallet-rpc-shaped answers; a seeded property test of 10,000 challenges against a reference grammar, round trips of 20,000 amounts and addresses at every boundary; `FuzzParseChallenge`, `FuzzTransfers`, `FuzzValidAddress`, `FuzzEnvelope`; and a source scan that keeps floats out (`rates/nofloat_test.go`) |
| The rules two watched rails share (`rail`) | Plan 8 moved every rule the on-chain rail had that Monero needs too (tiers, the payment predicates and sums, the reversal plan, the final event, the paid events, the ledger records of a rail and the walk over a payment's epochs, the rail's view and the peer facts) out of `onchain`, so each money rule has one definition. Standard library and SatBooth's `store`, `ledger` and `lnpay` only; no module. Capslock: none of its own; it carries `onchain`'s set through the `lnpay` import and the events callback | `onchain`'s own tests, unchanged, as the proof that the move changed nothing; table tests of every predicate, sum, the final-event rule, the reversal plan and the peer facts at their boundaries; `TestTiersProperty` and `FuzzTiers` (moved from `onchain`); the paid events and their replay (`TestPaidEvents`, `TestPaidEventsReplayLogsNothing`); the crash tests of both rails, which run through it |
| The Monero rail (`monero`, `monero/monerotest`) | wallet-rpc does the Monero cryptography (§1); what is left is SatBooth's own: the subaddress index protocol (record before showing, `store` after `create_address`, each index above every recorded one), the startup checks, the watcher over `get_transfers` and `get_transfer_by_txid`, and the settle over the `rail` rules with Monero's ledger. Standard library and SatBooth packages only; no module. `monero/monerotest` is an in-memory wallet-rpc that answers as v0.18.5.4 does (heights, the pool, `double_spend_seen`, a locked payment, a pop, a forgotten subaddress), imported by tests only. Capslock: `monero` has `onchain`'s set, all of it the store, ledger, `slog` and context artefacts described in §7; it opens no file and no socket (the `xmrrpc` client is passed in as `monero.RPC`). `monerotest` has no entry | The money paths against `monerotest`: replay (a payment found twice gives one row and one transfer), double pay (two payments, one transaction to two orders, both rails at once), timeouts and an outage of each method, restarts between discovery, mark and transfer, a crash at every hook point of the address and of the pass, reorgs before and after paid, a pool drop, 0-conf and its double spend, partial, over-, late and locked payments, the window's end, a stale peer holding the final event, and the pause on a wrong wallet; `FuzzTransferIDs` and `FuzzConfigValidate` beside seeded property tests; the whole service on regtest (`qualify/`, "Monero end to end") |
| Webhook signer, rate limiter | small | Property tests (timestamp window, constant-time compare, bucket bounds) |

## 6. Decisions

1. **Storage: start with SQLite.** If a large setup ever needs more,
   TigerBeetle is the choice, not Postgres: as good as Postgres is, it is not a financial-grade ledger.
   Driver: `ncruces/go-sqlite3` v0.36.0, pure Go (SQLite 3.54.1 translated to Go by
   wasm2go): no cgo. `mattn/go-sqlite3` needs cgo, which would break plan 1's `CGO_ENABLED=0` static, reproducible
   builds. `mattn/go-sqlite3` (cgo) stays the alternative. All driver specifics live in
   `ledger/open.go`, so swapping drivers means editing that one file.

   SatBooth shapes its money tables after TigerBeetle's model from day one, so a
   later move is a copy, not a redesign. Its reference docs (`docs/reference/transfer.md` and
   `account.md` in tigerbeetle/tigerbeetle, read 2026-11-02) define immutable transfers between two
   accounts on one ledger, each with a unique non-zero 128-bit id and an unsigned 128-bit amount;
   corrections are new transfers, never edits or deletes; pending transfers resolve at most once. In
   SQLite that means:
   - integer amounts in base units (msat, sat, piconero), one ledger per asset;
   - a unique id per transfer, so a retried write is refused instead of doubled;
   - no `UPDATE` or `DELETE` on money rows, enforced by triggers;
   - an order's paid or refunded state derived from its transfers.

   The options:
   - **SQLite via `mattn/go-sqlite3`** (v1.14.52, 2026-09-05, no Go dependencies, no OSV records).
     Caveats: it embeds about 286,000 lines of SQLite C that we won't review line by line (SQLite's own
     testing is the reason to trust it); it needs cgo, which costs the `CGO_ENABLED=0` static,
     bit-for-bit reproducible build unless we pin the C toolchain (a build container pinned by digest);
     and the empty OSV result means little, because SQLite's CVEs are filed in NVD, not the Go database.
     All SQL is our own and parameterised, and the database file is never attacker-supplied, so most
     SQLite CVE classes are out of reach.
   - **Our own append-only log** (one file, a checksum per record, fsync per write, state rebuilt in
     memory at start). It is fully reviewable, and the log *is* the audit log. But we would own fsync
     semantics, torn-tail recovery, compaction, consistent backups and format migration, for money state.
     It would need crash-injection, corrupt-tail and replay-idempotence tests.
   - **Postgres via `jackc/pgx/v5`** (v5.12.0): a network database, 6 direct dependencies, and 10 OSV
     entries (about five distinct issues) including SQL injection via protocol message size overflow
     (2024), placeholder confusion with dollar-quoted strings (2026) and memory safety (2026).
   - `modernc.org/sqlite`: 11 modules of transpiled C.
   - **Result:** SQLite via `ncruces/go-sqlite3` (see the driver note above).
2. **Schnorr:** `btcec/v2/schnorr`. See §3.
3. **Payout cap:** a lifetime budget on the `payout` connection, not a sub-wallet. See §4.
4. **monero-wallet-rpc auth:** our own Digest client, because wallet-rpc may not share a
   host with SatBooth. **Built (plan 8):** `xmrrpc`, no module added (§2 and §5).
5. **QR code: server-side SVG** from our own copy of the `coding` and `gf256`
   packages of `rsc.io/qr` (Russ Cox, BSD-3, v0.2.0 from 2018, about 1,050 lines, stdlib only, no OSV
   records; its cgo `libqrencode` package is not copied). The QR standard is frozen, so the old release date
   isn't a risk for code we own and review. The BSD-3 notice stays with the copied files. With
   no QR script, the checkout page can use a strict content security policy.
   - **What is in `qr/`** (plan 5): `coding/qr.go`, `gf256/gf256.go` and `gf256/gf256_test.go` from
     `rsc.io/qr` v0.2.0 (module hash `h1:6vBLea5/NRMVTz8V66gipeLycZMl/+UlFmk8DvqQ6WY=`, checked against the
     checksum database before copying) and its `LICENSE`. Changed: the `gf256` import path and the two import
     comments; each file carries a header naming its source. Not copied: the module's `libqrencode` package
     (cgo), its tests that use it, `png.go` and the top-level `qr.go`. `qr/svg.go` is ours: it picks
     alphanumeric or byte mode, the smallest version at level M, mask 0 (as `rsc.io/qr` does, no penalty
     scoring) and draws one `<path>`; the input is at most 1,200 bytes.
   - **Tests:** three golden symbols from ZXing's own unit test (Apache-2.0; see `qr/testdata/SOURCE.txt`),
     compared module by module (alphanumeric, numeric and byte mode). The ISO/IEC 18004 Annex I symbol could not be transcribed
     reliably. Also a structure property test (finder and timing patterns, format and version BCH codes)
     over 500 seeded inputs, a round trip of the SVG path, and `FuzzSVG`. No scanner is run in CI; the
     vectors and the property test are the evidence that the symbols decode.

## 7. Process controls (whatever the final set)

- `go.mod` pins the `toolchain`. Builds use `-trimpath`, so the same toolchain and flags give the same
  binary; with cgo, the C toolchain must be pinned as well.
- `go mod vendor` is committed, so every dependency line is in the repo and every upgrade is a diff that
  gets reviewed like our own code. CI builds with `-mod=vendor` and `GOPROXY=off`, so nothing is fetched.
  `go.sum` is checked against the public checksum database (sum.golang.org) when a version is added.
- **A dependency allowlist gate** (`scripts/check.sh`): CI fails when `go list -deps -test` shows a
  non-std package that isn't on the list, for each shipped target (linux/amd64, linux/arm64,
  darwin/arm64). The seed list is the 13 packages in §3.
- `govulncheck` (golang.org/x/vuln, v1.9.0; `scripts/tools.sh` installs it, `scripts/vulncheck.sh` runs
  it) in CI, on each Go point release and on a schedule. It reports only
  vulnerable symbols that are reachable, which is why the `x/crypto/ssh` records above don't concern us.
- `capslock` (github.com/google/capslock, v0.3.4, 2026-10-14) reports what each dependency can do
  (network, files, exec, unsafe, reflection). Its report is committed as `docs/capslock.json`;
  `scripts/capslock.sh` (also run by `scripts/check.sh`) fails when a capability is added, `scripts/capslock.sh update` rewrites it, and an
  upgrade that adds a capability needs a reason in the PR. The baseline is generated for linux/amd64.
  The `CAPABILITY_ARBITRARY_EXECUTION` entries for `nip44` and `chacha20` come from chacha20's assembly
  stubs (capslock treats assembly as arbitrary execution), not from exec; on linux/amd64 the same applies to
  the blake256 AVX assembly (`cpuid`, `blocksAVX`), and its `init` reads an environment variable
  (`CAPABILITY_READ_SYSTEM_STATE`).
  Adding the SQLite driver (plan 2) added these capabilities to `ledger`: `FILES` (the database file, via
  `os`), `SYSTEM_CALLS` (`x/sys/unix`), `UNSAFE_POINTER` (`go-sqlite3-wasm/v6`, `sqlite3_wrap`),
  `ARBITRARY_EXECUTION` (`sqlite3_wrap`: body-less `//go:linkname` declarations in `wrap_vfs.go`, which capslock cannot analyse; not assembly),
  `RUNTIME`, `REFLECT` and `UNANALYZED` (`database/sql`, `util`). Measured on the driver's own packages:
  `vfs` additionally shows `READ_SYSTEM_STATE`, not reachable from `ledger`. **No `NETWORK` and no
  `EXEC` anywhere** in the driver or its dependencies, which is the check that matters here.
  Adding the relay (plan 3, `relay` with `coder/websocket`) added `NETWORK` to `relay` and `coder/websocket`: the HTTP
  listeners and the WebSocket upgrade. That is the expected new capability. Where the other new `relay` capabilities
  come from, counted in `docs/capslock.json` by the last function of each path:
  - `RUNTIME`: `runtime.SetFinalizer` in `coder/websocket` (24 of 33 paths: 23 end in `(*Conn).close`, which clears the finalizer, `conn.go:158`, and 1 in `newConn`, which sets it), and `runtime.Goexit` via the
    standard library (the other 9).
  - `FILES`: `os.File` `Write` and `Close` (the `slog` text writer, `net/http`) and one `os.Open`.
  - `READ_SYSTEM_STATE`: one `os.Getenv`, the existing blake256 initialiser.
  - `UNANALYZED`: `errors.Is`, `sync.Pool` and `io.ReadAll` (19 of 43 paths go through `coder/websocket`).
  - `SYSTEM_CALLS`, `UNSAFE_POINTER`, `REFLECT` and 25 of 27 `ARBITRARY_EXECUTION` paths: call-graph artefacts. capslock
    resolves an interface call (an `io.Writer` or `io.Closer`) to every implementation in the program, including the SQLite
    driver's, so `relay` appears to reach `x/sys/unix.Getpagesize` and `go-sqlite3-wasm`. `relay` does not import the
    driver or `ledger`. The other two `ARBITRARY_EXECUTION` paths are the blake256 assembly, as above.
  The binary `cmd/satbooth-relay` (package `main` in the baseline) has the capabilities of `relay` plus
  `MODIFY_SYSTEM_STATE` (`os/signal.NotifyContext`, for SIGINT and SIGTERM); its `FILES` paths are the `os.Open` of the
  configuration file, and its `ARBITRARY_EXECUTION` paths are the same artefacts and the same blake256 assembly.
  The new baseline also lists `NETWORK` for `ledger`, for the same reason: capslock resolves interface calls in
  `database/sql` (context cancel) and the SQLite driver's `Close` to `net` and `net/http` types, which are now present in
  the program. The imports of `ledger` did not change, and it imports no network package.
  Adding `nwc` (plan 4, wire format only: `encoding/json`, `nostr`, `nip44`) adds these capabilities to `nwc`, counted in
  `docs/capslock.json` by the first and last function of each path. `nwc` itself opens no file and no socket. The
  paths start at its entry points: `BuildRequest`, `buildRequest`, `OpenResponse`, `openResponse`, `openEvent`,
  `openNotification`, `decodeResponse`, the parsing helpers (`checkJSON`, `parseObject`, `parseArray`,
  `parseTransaction`, `txFromFields`, `parseWalletError`, `rawString`, `reqString`, `optString`, `optStringStrict`,
  `optStrings`) and `nwc.init`.
  - `NETWORK` (2 paths, `net/http.body.Close`), `FILES` (2, `os.File.Close`), `RUNTIME` (2, `runtime.Goexit`),
    `SYSTEM_CALLS` (2, `x/sys/unix.Getpagesize`) and `UNSAFE_POINTER` (2, `go-sqlite3-wasm` `load32`): artefacts, all
    from `BuildRequest` and `buildRequest` through `json.Marshal` of the request. It flushes through an `io.Writer`,
    and capslock resolves that interface call to every implementation in the program, among them `net/http` and
    the SQLite driver. `nwc` imports neither. (The client, added next, is covered by its own entry below.)
  - `UNANALYZED` (18): `errors.Is` (15) and `io.ReadAll` (3), reached from every entry point above except `nwc.init`, which has none, through
    `Decoder.Token`, `json.Unmarshal`, `json.Marshal`, `nostr.Verify` and `PublicKeyHex`.
  - `REFLECT` (15): none of it reflects over `Transaction`. 2 paths are `json.Marshal` of the request
    (`transformMarshalError`), 7 are `Decoder.Token` (`reflect.TypeFor[float64]`), 6 are `json.Unmarshal` into a map
    or string (`reflect.MakeMap`).
  - `ARBITRARY_EXECUTION` (7): 6 go through `nip44.ConversationKey`, `Encrypt` and `Decrypt`, where `crypto/hkdf` and
    `hmac` call a `hash.Hash` that capslock resolves to blake256's `Hasher224.Write`, hence `blocksAVX` (an
    interface-resolution artefact, as above; `nip44` uses SHA-256). The 7th is `nwc.init` to `nostr.init` to the
    schnorr initialiser, ending at the `cpuid` assembly stub.
  - `READ_SYSTEM_STATE` (1): `nwc.init` to `nostr.init` to `os.Getenv`, the blake256 initialiser reading an
    environment variable. A real read, run at start-up.
  Adding the client (plan 4, `nwc/client.go`: one WebSocket per relay through `coder/websocket`'s `Dial`, `Read`,
  `Write`, `Ping` and `CloseNow`) takes `nwc` from 51 paths to 242 in `docs/capslock.json`; no other package changed. The
  entry points are now also `Dial`, `dial`, every `Client` method, `supervise`, `runSession`, `call`, `write`, `pinger`,
  `sendREQ`, `onEvent`, `onResponse`, `onNotification`, `drop`, `parseOK` and `frameEvent`, `frameAuth`, `frameWith`.
  The client opens no file. Its only network operations are those WebSocket calls to the one configured relay URL.
  - `NETWORK` (2 to 31): 15 end at `net.netFD.connect$1` through context cancellation (`context.WithCancel` to
    `afterFuncCtx.cancel`; capslock resolves the cancel callback to every `AfterFunc` user in the program, among them
    `net`'s dial), 7 are the real ones, `(*websocket.Conn).CloseNow` and `close` to `net/http.loggingConn.Close` (capslock's resolution of the `net.Conn` interface: the close is real, the type is not), 5 are the
    `net/http.body.Close` artefact above and 4 are `net.IP.AppendText` through `slog`. capslock does not follow `Dial`
    itself, so the real network I/O is not named by any path.
  - `FILES` (2 to 23): 18 `os.File.Write` through the `slog` handler, which `Config.Log` can point at a file (it is the
    caller's writer, not ours), and 5 `os.File.Close` (the SQLite artefact).
  - `RUNTIME` (2 to 31): 22 `runtime.SetFinalizer`, all ending in `(*websocket.Conn).close`, which clears the finalizer (`conn.go:158`), and 9 `runtime.Goexit`.
  - `UNANALYZED` (18 to 45), `REFLECT` (15 to 37): `errors.Is`, `io.ReadAll`, `sync.Pool`, `bufio.Writer.Flush`,
    `encoding/json` marshalling and decoding of the relay frames and of the requests.
  - `ARBITRARY_EXECUTION` (7 to 28), `SYSTEM_CALLS` (2 to 23), `UNSAFE_POINTER` (2 to 23): the same artefacts as before, now
    reached from the new entry points: 24 `ARBITRARY_EXECUTION` paths end in blake256's `blocksAVX` or `cpuid`, 4 in the SQLite
    driver's `vfsCheckReservedLock`; all `SYSTEM_CALLS` end at `x/sys/unix.Getpagesize` and all `UNSAFE_POINTER` at
    `go-sqlite3-wasm` `load32`, via interface calls capslock resolves to the driver. `nwc` does not import the driver.
  - `READ_SYSTEM_STATE` (1): unchanged.
  The baseline also records two new `nostr` `FILES` paths (`os.OpenFile` and `os.Remove` in `WriteNewSecretKeyFile`,
  added in the key generation commit before this one, whose baseline had not been updated).
  `nostr.Sign` counts the signed JSON length by hand rather than calling `json.Marshal`, to keep `nostr`'s baseline free of the
  `NETWORK`, `RUNTIME`, `SYSTEM_CALLS` and `UNSAFE_POINTER` artefacts that `encoding/json`'s encoder would add.
  Adding `cmd/satbooth-pair` (plan 4; package `main` in the baseline, as `cmd/satbooth-relay`; imports `nostr`, `flag`,
  `net/url`) adds 10 paths in `docs/capslock.json`, all from `main`, `run`, `loadOrCreate` or `init`. No other package changed.
  It has no `NETWORK`, `EXEC` or `MODIFY_SYSTEM_STATE` path: the link is printed, never fetched.
  - `FILES` (3): `os.Lstat` in `loadOrCreate`, which decides between loading and creating the key file. The reads and the
    create are real too (`LoadSecretKeyFile`, `WriteNewSecretKeyFile`) but capslock names them under `nostr`.
  - `UNANALYZED` (3): `errors.Is` in `loadOrCreate`, which tests for a missing file.
  - `REFLECT` (2): `flag.FlagSet.Parse` to `PrintDefaults` to `reflect.New`, the standard library's usage printing.
  - `READ_SYSTEM_STATE` (1) and `ARBITRARY_EXECUTION` (1): `main.init` to `nostr.init` to the blake256 initialiser
    (`os.Getenv`, the `cpuid` assembly stub), the artefacts listed above for `nwc.init`.
  - Adding `setup` (plan 12: the setup command's logic, `cmd/satbooth-setup`; standard library and SatBooth packages
    only, no module) adds one package entry to `docs/capslock.json`. It lists `NETWORK`, like `alert`, `ledger` and
    `store`, and for the same reason: its `encoding/json` encodes reach an `io.Writer` call that capslock resolves to
    every writer type in the program, the SQLite driver's and `net/http`'s among them. `setup` imports no network
    package and opens no socket. The command analysed alone (`./cmd/satbooth-setup`, where that resolution cannot
    arise) has `ARBITRARY_EXECUTION`, `FILES`, `READ_SYSTEM_STATE`, `REFLECT` and `UNANALYZED` and no `NETWORK`;
    `scripts/capslock.sh` runs that analysis for it and for `cmd/satbooth-pair` (both ship in the tools image) and
    fails if `NETWORK` appears, if capslock fails or if it prints no capability at all.
  - `service` and `cmd/satbooth` gain `MODIFY_SYSTEM_STATE` under the label `LOGGING` (plan 13): `service.Run` sets the
    standard logger's flags and output (`log.SetFlags`, `log.SetOutput`) once, to a writer that logs a fixed line. The
    standard logger is where `net/http` writes on its own (a client logs the first bytes a server sent on a connection
    with no request pending), and a webhook receiver could put its URL's token into SatBooth's standard error that
    way. No module is added and no other capability changes.
  - Adding `qr`, `qr/coding` and `qr/gf256` (plan 5) adds three package entries to `docs/capslock.json` and no
    capability: all three have none (`bytes`, `errors`, `strconv`, `fmt` and arithmetic only).
  - Adding `store` (plan 5: orders, invoices and the webhook outbox on the SQLite driver, opened as `ledger` is)
    adds one package entry with 262 paths in `docs/capslock.json` and no capability `ledger` does not already have: the
    same eight (`FILES`, `NETWORK`, `RUNTIME`, `SYSTEM_CALLS`, `ARBITRARY_EXECUTION`, `UNANALYZED`, `UNSAFE_POINTER`,
    `REFLECT`), each reached from `Open`, `openDB`, `migrate`, `write` and every `Store` method through
    `database/sql` and the driver. They are the driver artefacts described for `ledger` above, not new behaviour: `store`
    imports no network package, runs no program and opens only its one database file (via the driver). By the last
    function of each path:
    - `FILES` (33): `os.File.Close`. `SYSTEM_CALLS` (33): `x/sys/unix.Munmap`. `ARBITRARY_EXECUTION` (33): the driver's
      `vfsAccess` (body-less `//go:linkname`, not assembly). `UNSAFE_POINTER` (33): `go-sqlite3-wasm` `load16`/`load32`.
    - `NETWORK` (33): 31 end at `net.netFD.connect$1` (context cancellation, `context.WithCancel` to `afterFuncCtx.cancel`,
      which capslock resolves to every `AfterFunc` user in the program) and 2 at `net/http.body.Close` (an interface call).
    - `RUNTIME` (33): 23 `runtime.SetFinalizer` and 10 `runtime.Goexit`. `UNANALYZED` (33): 32 `errors.Is`, 1 `bufio.Writer.Flush`.
      `REFLECT` (31): 29 `reflect.Value.SetZero` (`database/sql` scanning) and 2 `json.transformMarshalError`
      (an interface call; `store` does not use `encoding/json`).
    - No `EXEC`, no `MODIFY_SYSTEM_STATE` and no `READ_SYSTEM_STATE`.
  - Adding `limits` (plan 5; imports `errors`, `fmt`, `math`, `sync`, `time`) adds one package entry and no
    capability to `docs/capslock.json`: no file, network, exec or reflection path.
  - Adding `lnpay` (plan 5; imports `store`, `limits`, `ledger`, `nwc`, `log/slog` and the standard library) adds one
    package entry with 246 paths in `docs/capslock.json` (36 direct, 210 transitive) and no capability `store` and `nwc` do
    not already have. It opens no file, socket or program itself: it reaches the wallet only through its `Wallet`
    interface and the databases only through `store` and `ledger`. By the last function of each path:
    - The driver artefacts of `store` and `ledger` (`New`, `Pay`, `Run` and the reconciler call their methods): `FILES`
      `os.File.Close` (17), `SYSTEM_CALLS` `x/sys/unix.Munmap` (29) and `Getpagesize`, `ARBITRARY_EXECUTION` `vfsAccess`
      (29), `vfsCheckReservedLock` and the blake256 initialiser's `cpuid`, `UNSAFE_POINTER` `load16`/`load32` (30),
      `RUNTIME` `SetFinalizer` (29) and `Goexit` (2), `REFLECT` `reflect.Value.SetZero` (29), `UNANALYZED` `errors.Is` (25)
      and `errors.As` (6).
    - `slog`: the thirteen `FILES` paths ending in `os.File.Write` and three `NETWORK` paths ending in `net.IP.AppendText` are
      the logger's handler writing a record and formatting an attribute; `lnpay` logs only fixed strings and opaque order
      ids to the `*slog.Logger` it is given. One `REFLECT` path ends in `encoding/json.transformMarshalError` and
      `UNANALYZED` has `sync.Pool.Get`.
    - `NETWORK` (28): `net.netFD.connect$1`, reached through `context.WithCancel` and `WithTimeout` (capslock resolves
      the cancel to every `AfterFunc` user in the program), as for `store`. `lnpay` dials nothing: the `nwc` client is
      passed in.
    - `READ_SYSTEM_STATE` (1) and the `cpuid` path above: `lnpay.init` to `nwc.init` to `nostr.init` to the blake256
      initialiser, the same chain `nwc` already has.
    - No `EXEC` and no `MODIFY_SYSTEM_STATE`.
  - Adding `webhook` (plan 5; imports `store`, `log/slog`, `net/http`, `crypto/hmac`, `crypto/sha256` and the standard
    library) adds one package entry with 32 paths in `docs/capslock.json` (16 direct, 16 transitive) and no capability that
    `store` and `lnpay` do not already have, but it is the first package of ours that makes an HTTP request. By the last
    function of each path:
    - `NETWORK` (4 direct): `net/http.NewRequestWithContext`, in `(*Deliverer).post`, reached from `Run`, `RunOnce` and
      `attempt`. This is real: the deliverer POSTs to the one configured webhook URL (http or https, no user info),
      through an `http.Client` with a 10 s timeout and redirects off, reading at most 4 KiB of the answer. It dials nothing
      else.
    - `FILES` (4 direct): `os.OpenFile` in `LoadSecret`, which reads the webhook secret from a 0600 file (regular file, no
      symbolic link, at most 130 bytes); and three `(*os.File).Write` paths through the `slog` handler, as for `lnpay`.
      `webhook` logs only fixed messages, opaque event ids and an HTTP status, never the URL, the secret or an error text
      that could contain them.
    - `UNANALYZED` (5 direct): `io.Copy` (4; draining the limited response body into `io.Discard`) and `io.ReadAll` (1; the
      secret file, limited to 130 bytes). capslock does not analyse either.
    - `REFLECT` (3 direct): `encoding/json.Marshal` of the four-field event payload, through `transformMarshalError`.
    - `ARBITRARY_EXECUTION` (7 transitive): `blake256.blocksAVX`, the artefact `nwc` already has: `hmac.HMAC.Sum` calls a
      `hash.Hash` that capslock resolves to blake256's `Hasher224.Write`. `webhook` uses SHA-256.
    - `RUNTIME` `SetFinalizer` (3), `SYSTEM_CALLS` `x/sys/unix.Munmap` (3) and `UNSAFE_POINTER` `load16` (3), all transitive:
      the `store` driver artefacts, as for `lnpay`.
    - No `EXEC`, no `MODIFY_SYSTEM_STATE` and no `READ_SYSTEM_STATE`.
  - Adding `api` (plan 5; imports `store`, `lnpay`, `net/http`, `crypto/sha256`, `crypto/subtle`, `encoding/json` and
    the standard library) adds one package entry with 72 paths in `docs/capslock.json` (39 direct, 33 transitive) and no
    capability that `store`, `lnpay` and `webhook` do not already have. By the last function of each path:
    - `NETWORK` (6 direct): `net/http.MaxBytesReader` and `(*net/http.response).Header`, the request body cap and the response
      headers. `api` dials nothing and makes no outbound request: it is a handler, served on the private listener by `cmd/satbooth`.
    - `FILES` (3 direct): `os.OpenFile` in `readKeyFile`, reached from `LoadKeys` and `Reload`. It reads the API key file: a regular
      file (symbolic links refused) with mode exactly 0600, at most 64 KiB, holding only SHA-256 hashes of the keys. Errors are
      fixed strings and never contain the file's content. Six transitive `(*os.File).Close` paths are the same file's close.
    - `UNANALYZED` (15 direct, 1 transitive): `io.ReadAll` (3; the key file, limited to 64 KiB, and the request body, limited by
      `MaxBytesReader` to 16 KiB), `errors.Is` and `errors.As` (10), and two `(*bufio.Writer).Write` paths from the response
      writer. capslock does not analyse them.
    - `REFLECT` (15 direct): `encoding/json` decoding and encoding of the request body, the key file and the nine-field response
      (`reflect.TypeFor`, `reflect.MakeMap`, `transformMarshalError`).
    - `ARBITRARY_EXECUTION` (7 transitive): `cpuid` (blake256) via `init`, and `sqlite3_wrap.vfsAccess` and
      `vfsCheckReservedLock` via `store`, as for `lnpay`.
    - `READ_SYSTEM_STATE` (1 transitive, `os.Getenv` from `init`), `RUNTIME` (6), `SYSTEM_CALLS` (6) and `UNSAFE_POINTER` (6): the
      `store` driver and `nwc` initialiser artefacts, as for `lnpay`.
    - No `EXEC` and no `MODIFY_SYSTEM_STATE`.
    - The same update also recorded two `NETWORK` paths of `webhook` (`noProxyTransport`, `(*net/http.Transport).Clone`) that the
      previous commit's baseline lacked; they are the deliverer's own HTTP client, already described above.
  - Adding `checkout` (plan 5; imports `store`, `lnpay`, `limits`, `qr`, `net/http`, `html/template` and the standard
    library) adds one package entry with 71 paths in `docs/capslock.json` and no capability that `store`, `lnpay` and `api` do not
    already have. It is a handler, served on the public listener by `cmd/satbooth`; it dials nothing, opens no file and runs no
    program. By the last function of each path:
    - `NETWORK` (14): 10 direct, from `net/http.NewServeMux` (2), `(*net/http.response).Header` (7, the response headers) and
      `net/http.MaxBytesReader` (the 1 KiB cap on the pay request body); and 4 transitive: `(*ServeMux).ServeHTTP` and three
      `netFD.connect$1` paths, the interface-call artefacts described for `store` (`checkout` dials nothing).
    - `REFLECT` (7): `(*text/template.Template).execute` (the page template, with embedded fixed text) and 6 `reflect.Value.SetZero`
      (`database/sql` scanning, via `store`).
    - `UNANALYZED` (13): `errors.Is` (8), `io.WriteString` (4) and `(*bufio.Writer).Write`: error tests and the response writer.
    - `FILES` (7), `RUNTIME` (7), `SYSTEM_CALLS` (7), `UNSAFE_POINTER` (7), `ARBITRARY_EXECUTION` (8), `READ_SYSTEM_STATE` (1):
      the `store` driver and `nwc` initialiser artefacts, as for `api` (`FILES` is `(*os.File).Close` 5 and `Write` 2, reached through
      `store` and `lnpay.Pay`); `checkout` itself touches no file (the page and the stylesheet are embedded).
    - No `EXEC` and no `MODIFY_SYSTEM_STATE`.
  - Adding `service` and `cmd/satbooth` (plan 5; `service` imports every package above, `log/slog`, `net`, `net/http`,
    `os/signal`, `reflect` and `encoding/json`; `cmd/satbooth` is package `main` in the baseline, as the other commands) adds one
    package entry with 96 paths and 19 `main` paths in `docs/capslock.json`; each update also drops older paths that the new
    call graph no longer reaches. The new capability is `MODIFY_SYSTEM_STATE`/`SIGNALS` (1 each): `os/signal.Notify`, from
    `service.Notify`, which subscribes the binary to SIGHUP, SIGINT and SIGTERM. No `EXEC`. By the last function of each path:
    - `NETWORK` (13 in `service`, 2 in `main`): the two listeners and their servers: `(*net.ListenConfig).Listen` (the configured
      public and private addresses), `(*net/http.Server).Serve` (3) and `Close`, `net/http.NewRequestWithContext` (the webhook
      deliverer and the NWC dial, reached through `Run`), and `netFD.connect$1` and `(net.IP).AppendText` (the interface-call
      artefacts described for `store` and `nwc`). Outbound calls go only to `nwc.relay_url` and `webhook.url`; the service has no
      other dialler. (Since plan 13 the webhook receivers are the `url` of each `webhooks` entry, one deliverer per
      entry; the dialler is the same.)
    - `FILES` (12 in `service`, 2 in `main`): `os.MkdirAll` (`prepareDataDir`: `data_dir`, 0700), `os.Open` (the configuration, also in `main`) and `os.OpenFile` (the configuration and
      the key, API-key and webhook-secret files, through `LoadConfig`, `CheckFiles` and `Run`), `os.Lstat` and
      `(*os.File).Write`/`Close` (the `nostr` key loader and the store driver). `service` writes no file itself.
    - `REFLECT` (14 in `service`, 2 in `main`): `reflect.TypeFor[float64]` and `json.transformMarshalError` (the logger of `net/http`'s error log, `fixedWriter`, among them), from the
      configuration's `encoding/json` decoding and the key walk in `checkKeys`, and one `SetZero` from `database/sql`.
    - `SYSTEM_CALLS` (9), `UNSAFE_POINTER` (9), `ARBITRARY_EXECUTION` (10), `RUNTIME` (10), `UNANALYZED` (17) and `READ_SYSTEM_STATE`
      (1): the `store`, `ledger` and `nwc` initialiser artefacts described above (`Munmap`, `load16`/`load32`, `vfsAccess`,
      `blocksAVX`/`cpuid`, `SetFinalizer`/`Goexit`, `errors.Is`/`io.ReadAll`/`io.WriteString`, `os.Getenv`), reached through `Run`.
    - Package `main` shows the same set through `run` and `init`, plus `os/signal.Notify` in `main`.
  - Adding `memlock` (plan 5: `mlockall`, `PR_SET_DUMPABLE` and `RLIMIT_CORE` 0 at start-up) adds one package entry to
    `docs/capslock.json` with one path, `SYSTEM_CALLS` (`memlock.Protect` to `x/sys/unix.Mlockall`). That one is real: it is
    the point of the package. capslock does not list `Prctl` or `Setrlimit`. `memlock` opens no file and no socket.
    `service` gains `service.Lock` (and `ReadConfig`, the first half of `LoadConfig`): `SYSTEM_CALLS` through `memlock.Protect`
    (real), and `FILES`, `NETWORK`, `RUNTIME`, `ARBITRARY_EXECUTION`, `UNANALYZED`, `UNSAFE_POINTER`, `REFLECT` through
    `Lock`'s `slog` calls and `ReadConfig`'s `os.Open`/`io.ReadAll`/`json.Decoder.Token`: the same artefacts as the existing `Run`
    paths (`slog` handler writing to an `io.Writer`, resolved to every implementation in the program).
  - Adding `btcrpc` (plan 6: the bitcoind client; imports `net/http`, `encoding/json`, `os` and the standard library)
    adds one package entry with 141 paths to `docs/capslock.json`. The real ones: `NETWORK` (15 direct:
    `net/http.NewRequestWithContext`, the POST to the one configured node, and `(*net/http.Client).CloseIdleConnections`
    in `Zero`), `FILES` (`os.OpenFile` in `LoadPassword`: the password file, `O_NOFOLLOW`, mode 0600 or stricter),
    `REFLECT` (27: `encoding/json` encoding the request and decoding the answers) and `UNANALYZED` (28:
    `errors.Is`/`errors.As`). The rest are the artefacts described above: `ARBITRARY_EXECUTION` (14, the driver's
    `vfsCheckReservedLock`), `FILES` (14, `(*os.File).Close`), `RUNTIME` (14, `Goexit`), `SYSTEM_CALLS` (14,
    `Getpagesize`) and `UNSAFE_POINTER` (14, `load32`), reached through interface calls; `btcrpc` imports neither the
    driver nor `nwc`. No `EXEC` and no `MODIFY_SYSTEM_STATE`. `descriptor` (plan 6) has no entry: capslock found no
    capability in it.
  - Adding `onchain` (plan 6: addresses and the startup checks; imports `btcrpc` for its types only, `descriptor`,
    `store`, `ledger`, `lnpay`, `log/slog` and the standard library) adds one package entry with 87 paths to
    `docs/capslock.json`, all of them artefacts of calls through interfaces and of the packages it imports; none is a
    path of its own. The watcher takes it to 260 paths, in the same classes, with no new one: the `store` and `ledger`
    driver artefacts (`SetFinalizer`, `errors.Is`, `Munmap`, `vfsAccess`, `SetZero`, `load16`, about 27 paths each),
    `(*os.File).Write` (26) and `(net.IP).AppendText` (8) through `slog`, and `netFD.connect$1` (26) through context
    cancellation. It dials nothing: the `btcrpc` client is passed in as `onchain.RPC`, and `lnpay` as
    `onchain.Lightning`. It opens no file and runs no program. By the last function of each path:
    - `NETWORK` (12): `(*net.netFD).connect$1` (10, reached through `context.WithCancel`/`WithTimeout` `AfterFunc` interface calls and
      through the `store` and `ledger` calls) and `(net.IP).AppendText` (2, from `slog`'s attribute formatting): the artefacts
      described for `store` and `service`. `onchain` has no socket.
    - `FILES` (10), `RUNTIME` (12), `SYSTEM_CALLS` (10), `UNSAFE_POINTER` (10), `ARBITRARY_EXECUTION` (11), `UNANALYZED` (11),
      `REFLECT` (10) and `READ_SYSTEM_STATE` (1): the `store`, `ledger` and `nwc` (via `lnpay`) driver and initialiser artefacts
      described above.
    - No `EXEC` and no `MODIFY_SYSTEM_STATE`.
    - `onchain/onchaintest` (the in-memory bitcoind for tests; standard library and `btcrpc` types only) is imported by tests only, and capslock
      found no flagged path in it, so it has no entry in the baseline.
  - Adding `status` (plan 6: one order status from both rails; imports `store`, `lnpay`, `onchain` and the standard
    library) adds one package entry with 18 paths to `docs/capslock.json`, all of them artefacts of the packages it
    imports and of calls through the `Lightning` and `Onchain` interfaces; none is a path of its own. It dials nothing,
    opens no file and runs no program. By the last function of each path: `NETWORK` (2), `FILES` (2), `RUNTIME` (2),
    `SYSTEM_CALLS` (2), `UNSAFE_POINTER` (2), `ARBITRARY_EXECUTION` (3), `UNANALYZED` (2), `REFLECT` (2) and
    `READ_SYSTEM_STATE` (1): the `store` driver and the `lnpay` (via `nwc`) initialiser artefacts described above. No
    `EXEC` and no `MODIFY_SYSTEM_STATE`.
  - The same plan grows existing entries without a new class: `store` from 262 to 486 paths (the on-chain tables and queries,
    all through `database/sql` and the driver, 59 to 61 per class), `lnpay` from 246 to 288 (the startup completion and the
    orders without a Lightning amount) and `webhook` from 34 to 35 (`errors.As` in `osReason`, from plan 5's last webhook change, which the baseline then lacked). Plan 6's last baseline update was
    that of `status`; its later changes (`api`, `checkout`, `service`, `qualify`) added no capability, and their
    entries were not regenerated then. Plan 7 regenerated the baseline (below).
  - Adding `rates` (plan 7: fiat pricing; imports `math/big`, `net/http`, `crypto/tls`, `encoding/json`, `log/slog` and
    the standard library, no SatBooth package) adds one package entry with 67 paths to `docs/capslock.json`. The real
    ones: `NETWORK` (5 direct: `net/http.NewRequestWithContext`, the `GET` to a provider's fixed URL or the custom URL,
    HTTPS only), `UNANALYZED` `io.Copy` (5: draining at most 256 KiB of a non-200 answer into `io.Discard`) and
    `REFLECT` (`encoding/json` decoding a provider's answer with `UseNumber`; its `reflect.TypeFor[float64]` paths are
    inside `encoding/json`, and the no-float scan covers our code). The rest are artefacts: `FILES` `(*os.File).Write`,
    `ARBITRARY_EXECUTION`, `SYSTEM_CALLS` and `UNSAFE_POINTER` through the `slog` handler writing a record (an
    `io.Writer` resolved to the SQLite driver's), `NETWORK` `netFD.connect$1` and `RUNTIME` through
    `context.WithTimeout`, and `errors.Is`/`As`/`Unwrap`. `NETWORK` came with the client; the background fetcher, which
    logs and bounds each call with a context, added the slog and context classes every logging package carries
    (accepted). `rates` opens no file and runs no program. `rates/ratestest` (the fake rate server, imported by tests
    only) adds 12 paths: `net.Listen` and the listener of its loopback `httptest` server (its round tripper re-routes
    the providers' hosts there), and the same artefacts. No `EXEC` and no `MODIFY_SYSTEM_STATE`. The other plan 7
    changes (`store`, `api`, `webhook`, `checkout`, `status`, `service`) added no capability class. The regenerated
    baseline grows existing entries' path counts with no new class: `store` 486 to 542, `onchain` 260 to 362, `service`
    115 to 131, `btcrpc` 141 to 151, `checkout` 71 to 81 and `status` 18 to 26; `lnpay` shrinks from 288 to 280 (the
    example paths capslock renders change with the program).
  - Plan 8 (Monero) adds three package entries and no capability class beyond the packages they mirror. The counts
    below are those of the committed baseline, last regenerated with `monero`'s subaddresses and
    checks; the later changes (the watcher, the API, the wiring, the qualification) added no class, which
    `scripts/capslock.sh` checks on every `scripts/check.sh`, and were not regenerated. By the last function of each
    path:
    - `rail` (the rules the two watched rails share; imports `store`, `ledger`, `lnpay` and the standard library): 10
      paths, all transitive, one per class of `onchain`'s set (`FILES` `(*os.File).Close`, `NETWORK`
      `netFD.connect$1`, `RUNTIME` `SetFinalizer`, `SYSTEM_CALLS` `Munmap`, `UNSAFE_POINTER` `load16`, `REFLECT`
      `SetZero`, `UNANALYZED` `errors.Is`, `ARBITRARY_EXECUTION` `vfsAccess` and the blake256 `cpuid`,
      `READ_SYSTEM_STATE` `os.Getenv`): the `store` driver and `nwc` initialiser artefacts reached through `lnpay` and
      the events callback. `rail` opens no file and no socket.
    - `xmrrpc` (the wallet-rpc client; imports `net/http`, `crypto/md5`, `crypto/tls`, `encoding/json`, `btcrpc` and
      the standard library): 107 paths, the same classes as `btcrpc`. The real ones: `NETWORK`
      `net/http.NewRequestWithContext` (the POST to the one configured wallet-rpc) and
      `(*net/http.Client).CloseIdleConnections` (in `Zero`), `REFLECT` (`encoding/json` encoding the request and
      decoding the answers: `reflect.MakeMap` 9, `TypeFor[float64]` 1, inside `encoding/json`), and `UNANALYZED`
      (`errors.As`/`Is`, `io.Copy` draining a 401's body in `drain`, and `io.WriteString` in `Client.Format`, which prints a
      fixed text for every verb). The rest are artefacts: 11
      `NETWORK` paths end at `(*net/http.body).Close` (an interface call), and `FILES` `(*os.File).Close`, `RUNTIME`
      `SetFinalizer`, `SYSTEM_CALLS` `Getpagesize`, `UNSAFE_POINTER` `load32` and `ARBITRARY_EXECUTION`
      `vfsCheckReservedLock` (10 each) are the SQLite driver reached through `io.Writer` and `io.Closer` interface
      calls; `xmrrpc` imports neither the driver nor any file API. No `EXEC`, no `MODIFY_SYSTEM_STATE`.
    - `monero` (the rail; imports `xmrrpc` for its types, `rail`, `store`, `ledger`, `lnpay`, `log/slog` and the standard
      library): 148 paths, `onchain`'s classes: the `store` and `ledger` driver artefacts (`SetFinalizer`, `errors.Is`,
      `Munmap`, `vfsAccess`, `SetZero`, `load16`, 13 to 16 each), `(*os.File).Write` (9) and `(net.IP).AppendText` (3)
      through `slog`, `netFD.connect$1` (17) through context cancellation, and the blake256 initialiser (`cpuid`,
      `os.Getenv`) through `lnpay`. It dials nothing (the client is passed in as `monero.RPC`), opens no file and runs
      no program. `monero/monerotest` (the in-memory wallet-rpc, imported by tests only) has no entry: capslock found
      no flagged path in it.
- GitHub Actions pinned by commit SHA, with least-privilege `permissions:`. No third-party actions
  beyond checkout and setup-go.
- Each dependency has a row here: version, date, what it requires, OSV records, size, and why stdlib is
  not enough (CLAUDE.md).

## 8. Third-party servers in the hub bundle (plan 10)

`deploy/hub/` runs third-party servers beside SatBooth's relay. None is linked into SatBooth, and SatBooth's own
images never contain them. Alby Hub is its release image, pulled by digest. bitcoind, Fulcrum and the Monero binaries
are official release binaries, checked against pinned SHA-256 hashes in the Dockerfiles (`deploy/hub/*/Dockerfile`)
and copied onto `debian:13-slim` pinned by digest (plan 10, decision 5). Each runs as its own non-root user with a
read-only root and no capabilities (`deploy/README.md`). Alby Hub v1.24.6, Bitcoin Core 32.1 and Monero v0.18.5.4 are
the qualified versions (`qualify/REPORT.md`); the bundle uses the same pins as `dev/regtest/`.

| Server | Version (date) | Why | License | Source of the binary and its check | Needs | Record |
|---|---|---|---|---|---|---|
| Alby Hub | v1.24.6 | The Lightning node SatBooth's NWC connections reach (`design.md` §2.1); §4 above has what it enforces | Apache-2.0 | `ghcr.io/getalby/hub:v1.24.6@sha256:4bb27770ff2b68ab8342f75909dfcb59db0ca8f903045967e44c908b7f6f85f7`, the multi-arch index digest; the tag points at commit `921003b`, the source the docs cite (§4) | bitcoind as its chain source (`LDK_BITCOIND_RPC_*`), the bundle's Fulcrum for its address lookups, SatBooth's relay | The version qualified against; a new version is re-qualified before its digest replaces this one |
| Bitcoin Core | 32.1 | The hub's chain source and SatBooth's on-chain rail (watch-only wallet, whitelisted RPC user); Fulcrum's index | MIT | `bitcoincore.org/bin/bitcoin-core-32.1/bitcoin-32.1-{x86_64,aarch64}-linux-gnu.tar.gz`; SHA-256 pinned in `deploy/hub/bitcoind/Dockerfile`, the same hashes as `dev/regtest/bitcoind/Dockerfile`, the entries of the release's `SHA256SUMS` (8 good signatures from the builder keys of `bitcoin-core/guix.sigs` at `fdc0f963`). Only `bitcoind` and `bitcoin-cli` are copied | `txindex=1` and an unpruned node, for Fulcrum | The version qualified against (`qualify/REPORT.md`, "On-chain end to end") |
| Monero (`monerod`, `monero-wallet-rpc`, `monero-wallet-cli`) | v0.18.5.4 | The `monero` profile: the operator's node and SatBooth's view-only wallet-rpc; `monero-wallet-cli` for the operator to make the view-only wallet | BSD-3-Clause | `downloads.getmonero.org/cli/monero-linux-{x64,armv8}-v0.18.5.4.tar.bz2`; SHA-256 pinned in `deploy/hub/monero/Dockerfile`, the same hashes as `dev/regtest/monero/Dockerfile`, copied from `getmonero.org/downloads/hashes.txt` after its PGP signature verified with the key in `utils/gpg_keys/binaryfate.asc` at the tag | a pruned node is enough | The version qualified against (`qualify/REPORT.md`, "Monero end to end"); not qualified against v0.19 (`docs/monero.md`, "Setting up monerod and monero-wallet-rpc") |
| Fulcrum | 2.1.4 (2026-10-18) | The hub's on-chain address lookups (whether an address was used, for its address API and swaps) need an Electrum or Esplora server: bitcoind has no address index (getAlby/hub v1.24.6 `chain/address_lookup.go:17-26`). Without one they go to Alby's server, which learns the hub's addresses and the host's IP. The hub's Lightning chain monitoring does not use it: with bitcoind configured, LDK's only chain source is bitcoind (`lnclient/ldk/ldk.go:217-232`). | GPL-3.0-or-later (`LICENSE.txt` in the tarball); run unmodified as a separate program, no code taken | `github.com/cculianu/Fulcrum` releases, `Fulcrum-2.1.4-{x86_64,arm64}-linux.tar.gz`; hashes from `Fulcrum-2.1.4-shasums.txt`, whose signature verified as "Good signature" from key `D465 135F 97D0 047E 18E9 9DC3 2181 0A54 2031 C02C` (Calin Culianu), the key published in `Electron-Cash/Electron-Cash` `pubkeys/calinkey.txt` (2026-11-11). Dynamically linked against glibc, zlib and libbz2 only, all in the base image | bitcoind with `txindex=1`, unpruned; disk for its index (133 GB for Bitcoin as of August 2023, per its README, more since); RAM 1 GB minimum (`db_mem` default 2048 MiB) | No CVE in the NVD (keyword searches "Fulcrum SPV" and "cculianu", 2026-11-11) and no GitHub security advisory. Maintained since 2020 (53 releases; 2.1.0 to 2.1.4 between 2025-11 and 2026-10); 1,704 commits by its author, 49 by the next four contributors. Chosen over electrs (MIT), which publishes no release binaries. |

**The hub's bitcoind RPC calls (plan 11).** The hub's RPC user is confined by `rpcwhitelist` to the calls its chain
source makes, in the bundle (`deploy/hub/config-example/bitcoin.conf`) and on regtest
(`dev/regtest/bitcoind/bitcoin.conf`):

    getblockchaininfo,getblockheader,getblock,getblockhash,gettxout,getrawmempool,getmempoolentry,getrawtransaction,getmempoolinfo,estimatesmartfee,sendrawtransaction

The source read, and how sure each link is:
- Alby Hub v1.24.6 is commit `921003b`. Its `go.mod:16` records `github.com/getAlby/ldk-node-go
  v0.0.0-20261021094837-02f88de84f63`. Recorded in a file.
- `ldk-node-go` `02f88de` holds only built libraries ("Update bindings."), with no record of the source commit. That it
  was built from getAlby/ldk-node `87ea53f` (ldk-node 0.7.1) is **inferred**: `02f88de` landed 9 minutes after
  `87ea53f` on the fork's `main`, as each bindings commit follows a `main` commit; `87ea53f`'s only change that reaches
  the build is `rustls-webpki` 0.103.15 to 0.103.16 in `Cargo.lock`, and the shipped `libldk_node.so` (x86_64 and
  aarch64) holds the string `rustls-webpki-0.103.16`, where the bindings commit before it holds 0.103.15. The bitcoind
  code is the same in the `main` commit before `87ea53f`.
- ldk-node `87ea53f` takes `lightning-block-sync` 0.2.0 from crates.io (`Cargo.toml:50`, checksum in `Cargo.lock`).
  Recorded in files.
- Each of the eleven method names is a string in the shipped `libldk_node.so`; `getblockcount`, `getnetworkinfo`,
  `submitpackage` and `testmempoolaccept` are not.

Where each call is sent (ldk-node `87ea53f` `src/chain/bitcoind.rs`; `lightning-block-sync` 0.2.0 `src/rpc.rs`):

| Method | Sent at | When |
|---|---|---|
| `getblockchaininfo` | `rpc.rs:156` (`get_best_block`) | the chain tip: at start, every 2 s poll (`poll.rs:222`), and in gossip checks (`gossip.rs:204`) |
| `getblockheader` | `rpc.rs:141` (`get_header`) | the tip's header and the walk back over a reorg (`init.rs:29,160`, `poll.rs:228,249`) |
| `getblock` | `rpc.rs:151` (`get_block`, verbosity 0) | each new block for the listeners (`poll.rs:262`); gossip checks (`gossip.rs:218`) |
| `getblockhash` | `rpc.rs:166` (`get_block_hash_by_height`) | gossip checks only (`gossip.rs:204`) |
| `gettxout` | `rpc.rs:176` (`is_output_unspent`, mempool excluded) | gossip checks only (`gossip.rs:242`) |
| `getrawmempool` | `bitcoind.rs:928` | each mempool poll of the on-chain wallet |
| `getmempoolentry` | `bitcoind.rs:964` | each mempool transaction not seen before |
| `getrawtransaction` | `bitcoind.rs:829` | each mempool transaction not yet handed to the wallet |
| `getmempoolinfo` | `bitcoind.rs:793` | fee estimates (`mempoolminfee`), as for `estimatesmartfee` |
| `estimatesmartfee` | `bitcoind.rs:769` | fee estimates: once at start, blocking (`src/lib.rs:248`; on mainnet a failure stops the start, `bitcoind.rs:533-536`), then after each new block |
| `sendrawtransaction` | `bitcoind.rs:731` | every broadcast, one transaction at a time (the comment at `bitcoind.rs:599-602`: no `submitpackage`) |

- The hub picks the RPC chain source (`lnclient/ldk/ldk.go:217-226`, `SetChainSourceBitcoindRpc`), so ldk-node builds
  one `RpcClient` (`bitcoind.rs:660-664`). `call_method` (`rpc.rs:66-132`) posts one JSON object per call, never a
  batch, so the whitelist sees each method. Every `call_method` in ldk-node's `src` is in `src/chain/bitcoind.rs`
  (the six lines above); the rest go through `lightning-block-sync`'s `BlockSource` and `UtxoSource` for `RpcClient`
  (`rpc.rs:134-180`), reached from `bitcoind.rs:1189-1231` and `bitcoind.rs:703-705`. The other `call_method` lines of
  `rpc.rs` are its tests. No REST, no ZMQ: ldk-node has no ZMQ code, and the hub never calls the REST variant.
- The gossip checks run because the hub leaves `LDK_GOSSIP_SOURCE` empty (`ldk.go:253-256`): P2P gossip, whose
  verifier (ldk-node `src/gossip.rs:65-81`) uses the `RpcClient` as its `UtxoSource`. With a rapid gossip sync server
  set, `getblockhash` and `gettxout` would not be called.
- The hub's Go code calls bitcoind nowhere else: the four `LDK_BITCOIND_RPC_*` settings are only passed to ldk-node.
- What qualification ran on the confined user is in `qualify/REPORT.md`, "The hubs' bitcoind user". Force closes,
  fee bumps and sweeps run in no phase.

**Base images.** Every image is pinned by digest; a tag alone is never used. The reason for each file in a release
image, and each pin, is in `deploy/README.md` ("Release images").

| Image | Pin | Used for |
|---|---|---|
| Go builder | `golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9` | the build stage of `deploy/image/Dockerfile` (only the two binaries and its CA bundle leave it); the `volume-init` of the public bundle and of `dev/regtest/` |
| BuildKit | `moby/buildkit:v0.34.1@sha256:99368f3e46177c6405b9740e4461d91cee004128daeecca78d4ceac50ec1da5c` (v0.34.1, published 2026-10-20; the newest release at least 14 days old when pinned) | the `docker-container` builder of `scripts/release-lib.sh`: Docker's own BuildKit, pinned because layer compression and the exporter decide the release image digests |
| Debian | `debian:13-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2` | the bitcoind, Fulcrum and Monero images of the hub bundle and of `dev/regtest/`, the hub bundle's `netns` and `volume-init` |
| Alby Hub | `ghcr.io/getalby/hub:v1.24.6@sha256:4bb27770…` (the table above) | the hub bundle and the regtest hubs |
| `satbooth`, `satbooth-relay` | built locally from `deploy/image/Dockerfile`, `FROM scratch`; never pushed to a registry | the public bundle, the hub bundle's relay, and qualification |

**The font.** The logo's wordmark (`docs/logo/satbooth-logo*.svg`) is Rubik 2.300 (github.com/googlefonts/rubik, the
variable font at weight 800), SIL Open Font License 1.1, with its letters converted to paths. No font file is in the
repository or an image, and nothing renders text at run time: the checkout page writes "SatBooth" in HTML beside the
mark. `ACKNOWLEDGEMENTS.md` credits it. resvg 0.49.0 (Apache-2.0 OR MIT) renders the logo's PNGs from the SVGs
(`docs/logo/render.sh`); it is a local tool, not a dependency.
