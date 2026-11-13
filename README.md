# SatBooth

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/logo/satbooth-logo-dark.svg">
    <img src="docs/logo/satbooth-logo.svg" alt="SatBooth" height="96">
  </picture>
</p>

**Open a booth. Hold no keys.**

SatBooth is a small, self-hosted checkout for Bitcoin Lightning, on-chain bitcoin and Monero. It is built
for merchants, creators and services that want to take payments without running a hot wallet that can be
swept.

> Status: **under construction**. The foundation (the Nostr core, the NWC client and the
> supply-chain checks), the Lightning payment path, on-chain bitcoin payments, fiat pricing, Monero payments
> (view-only), Lightning refunds and the operations side (release images, a Compose bundle per host, matched backups
> and operator alerts) are built and tested on regtest (`qualify/REPORT.md`). Nothing has run on mainnet.
> Do not use it for real money yet. See [`docs/design.md`](docs/design.md) and
> [`docs/threat-model.md`](docs/threat-model.md).

## Why

Self-hosted payment servers usually keep spendable keys on the same machine that faces the internet. When
that machine or its application is compromised, the wallet is gone. That is a property of the design, not
of any one bug: a hot wallet on an internet-facing server is one exploit away from empty.

SatBooth starts from the opposite assumption: **the server will be attacked, so it should have almost
nothing worth stealing.**

## The idea in one table

| Rail | What the SatBooth server holds | Worst case if the server is fully compromised |
|---|---|---|
| On-chain bitcoin | A **watch-only** descriptor (xpub). No private keys. | The attacker sees addresses and payments. They can't spend anything. |
| Monero | The wallet's primary address only, and a login to a **view-only** wallet (address + private view key) in monero-wallet-rpc on a private host. No spend key anywhere. | The attacker sees incoming payments, and through the login can read the view key and fake payments to SatBooth. They can't spend anything. |
| Lightning | Scoped **Nostr Wallet Connect** (NWC) connections to a small, dedicated Alby Hub node, with a hub-enforced spending budget. | Losses are capped by the hub's channel size and the payout budget, which are both small by design. |

Received Lightning funds are swept regularly from the small hub to the merchant's main node or cold storage.
The Lightning bound holds only if **the hub runs on a different host (a separate trust domain) from the
public-facing SatBooth service**: that separation is a requirement, not an option.
Refunds on-chain and in Monero are paid by a human from the merchant's own wallet, never by the server.

## Principles

1. **Nothing to sweep.** No on-chain or Monero spend keys on the server, ever. Hot Lightning money is bounded and
   budgeted.
2. **Small and boring.** As few dependencies as possible. Code that a reviewer can read in an afternoon.
3. **Global limits.** Accounts and identities are cheap to create, so every limit that protects the hub or the money
   is global and keeps paying customers working during a flood. The one per-account limit, a cap on one account's
   open invoices, only comes on top of them: it bounds a single account, not a flood of new ones.
4. **No third-party calls by default.** Your own bitcoind, your own monerod, your own Nostr relay. No vendor
   accounts, no telemetry. Fiat pricing is the one exception, and only when the operator turns it on: the merchant's
   app may then price an order in a fiat currency, and SatBooth converts it with the median of the public rate
   providers the operator lists (see [The `rates` block](docs/configuration.md)).
5. **Secrets never in argv, logs or responses.** Not even "redacted".
6. **Open source** under the [Apache License 2.0](LICENSE), so that more eyes are on the code.

## Documentation

Run it:

- [Running SatBooth](docs/running.md): the relay, pairing with the hub, and the service's start, stop and files.
- [Configuration](docs/configuration.md): every key of the configuration file, with an example.
- [Setting up bitcoind](docs/bitcoind.md): the node, the watch-only wallet and the descriptor for on-chain payments.
- [Setting up monerod and monero-wallet-rpc](docs/monero.md): the node and the view-only wallet for Monero payments.
- [Deploying with Docker Compose](deploy/README.md): the release images, how to verify them, and the bundle of each
  host.
- [Releasing](docs/releasing.md): how a release is made, tagged and published.
- [Migrating from BTCPay Server](docs/migrating-from-btcpay.md): what moves, what does not, and the API mapping.

Take payments:

- [The merchant API](docs/merchant-api.md): creating and reading orders on the private listener.
- [The checkout page](docs/checkout.md): what the customer sees on the public listener.
- [Webhooks](docs/webhooks.md): the signed events SatBooth sends to the merchant's endpoints.
- [Refunds](docs/refunds.md): Lightning refunds, their approvals, and `satbooth-approve`.

Rules per payment method:

- [Lightning payments](docs/rules-lightning.md): how an order is paid and settled over Lightning.
- [On-chain payments](docs/rules-onchain.md): tiers, confirmations, replacement, reorgs and the watch window.
- [Monero payments](docs/rules-monero.md): the same rules for Monero, and where they differ.
- [Fiat pricing](docs/rules-fiat.md): how a fiat price becomes an amount, and what the rate providers may do.

Operate it:

- [Backup and restore](docs/backup-and-restore.md): the offline backup, the restore and the pair stamp.
- [Alerts](docs/alerts.md): the `ALERT:` lines and the operator's alert webhook.

Work on it:

- [Development](docs/development.md): building, testing and the checks.
- [Design](docs/design.md), [threat model](docs/threat-model.md) and [dependencies](docs/dependencies.md): the
  decisions, the attacks considered, and every third-party dependency.
- [The qualification report](qualify/REPORT.md): what the regtest suite measured.
- [Security policy](SECURITY.md): how to report a vulnerability.

## Layout

| Path | What |
|---|---|
| `docs/design.md` | Architecture, rails and design decisions |
| `docs/threat-model.md` | What we defend against, and how |
| `docs/migrating-from-btcpay.md` | A guide for a merchant who runs BTCPay Server: what moves, what does not, and the API mapping |
| `docs/dependencies.md` | Every third-party dependency, why it is needed, and the supply-chain controls |
| `relay/`, `cmd/satbooth-relay` | SatBooth's NWC-only Nostr relay and its binary |
| `nwc/` | the NWC client: requests to the hub, with timeouts, lookups and notifications |
| `cmd/satbooth-pair` | the operator's pairing tool (see [Pairing with the hub](docs/running.md)) |
| `setup/`, `cmd/satbooth-setup` | the setup command: it writes every configuration file and generates every secret of a host, in four stages across the two hosts (see `deploy/README.md`, "Set up with `satbooth-setup`") |
| `approval/`, `lninvoice/`, `refund/` | refund approvals (the formats and their checks), the claimant invoice's syntax and amount check, and the Lightning refund service (admission, sending, resolving, voids, recovery; `refund/refundtest` is a fake payout wallet for tests) |
| `cmd/satbooth-approve` | the signing tool for refund and void approvals (see [`satbooth-approve`](docs/refunds.md#satbooth-approve)) |
| `cmd/satbooth`, `service/`, `memlock/` | the service binary, its wiring and configuration, and the memory protection (see [Running satbooth](docs/running.md)) |
| `api/`, `checkout/`, `webhook/` | the merchant API (private listener), the checkout page (public listener) and signed webhooks |
| `alert/` | the log handler that queues every `ALERT:` line for the operator's webhook (see [Alerts](docs/alerts.md)) |
| `lnpay/`, `limits/`, `store/`, `qr/` | the Lightning payment logic, the invoice limits, the order store (SQLite) and the QR encoder |
| `rates/` | fiat pricing: the exact decimal, the conversion to sats, the rate providers and the background fetcher (`rates/ratestest` is a fake rate server for tests; `rates/testdata` holds recorded provider answers) |
| `onchain/`, `btcrpc/`, `descriptor/` | the on-chain rail (addresses and the watcher; `onchain/onchaintest` is an in-memory bitcoind for tests), the bitcoind JSON-RPC client and the watch-only descriptor grammar |
| `monero/`, `xmrrpc/` | the Monero rail (subaddresses, the startup checks and the watcher; `monero/monerotest` is an in-memory wallet-rpc for tests) and the monero-wallet-rpc JSON-RPC client with HTTP Digest |
| `rail/` | the rules the on-chain and Monero rails share: tiers, the payment predicates and sums, the final event, the ledger records and the rail's view |
| `status/` | one order status from the three rails, for the API and the checkout page |
| `ledger/` | the money ledger |
| `deploy/` | the release images and the Docker Compose bundle of each host, `hub/` and `public/` (see `deploy/README.md`) |
| `deploy/setup.sh` | runs one stage of `satbooth-setup` in the pinned tools image, with no network (see `deploy/README.md`) |
| `qualify/`, `dev/regtest/` | the hub, bitcoind and Monero qualification suite and its regtest setup (dev only); `qualify/REPORT.md` has the measured values |
| `CLAUDE.md` | Working rules for coding agents in this repo |

## Acknowledgements

SatBooth stands on other people's work. What it borrows is listed in [`ACKNOWLEDGEMENTS.md`](ACKNOWLEDGEMENTS.md), and
the Go libraries it links, with their licenses and records, in [`docs/dependencies.md`](docs/dependencies.md).
