# SatBooth: working rules for coding agents

Read first: `README.md`, `docs/design.md`, `docs/threat-model.md` and `docs/dependencies.md`.

## Status

Under construction. Plans 1 to 14 and the follow-ups of plan 6.1 are built: the Nostr core (`nostr`, `nip44`), the
ledger (`ledger`), the NWC-only relay (`relay`, `cmd/satbooth-relay`), the NWC client (`nwc`), the pairing tool
(`cmd/satbooth-pair`), the hub, bitcoind and Monero qualification suite (`qualify/`, with the regtest setup in
`dev/regtest/`), the supply-chain gates (`scripts/`), the Lightning payment service: the QR encoder (`qr`), the order
store (`store`), the invoice limits (`limits`), the payment logic (`lnpay`), signed webhooks (`webhook`), the merchant
API (`api`), the checkout page (`checkout`), the memory lock (`memlock`) and the wiring (`service`, `cmd/satbooth`), and
on-chain bitcoin: the bitcoind client (`btcrpc`), the descriptor grammar (`descriptor`), the on-chain rail (`onchain`,
with the test bitcoind `onchain/onchaintest`) and the status of all rails (`status`); fiat pricing (`rates`, with the
fake rate server `rates/ratestest`); and Monero, view-only: the rules the watched rails share (`rail`), the wallet-rpc
client (`xmrrpc`) and the Monero rail (`monero`, with the in-memory wallet-rpc `monero/monerotest`); and Lightning
refunds: the approval formats (`approval`), the claimant invoice's syntax check (`lninvoice`), the refund service
(`refund`, with the fake payout wallet `refund/refundtest`) and the signing tool (`cmd/satbooth-approve`); and
operations: the pair stamp of the store and the ledger (`store`, `ledger`, `service`), the offline backup
(`satbooth backup`, in `service` and `cmd/satbooth`), the operator alerts (`alert`, and `webhook`'s alert deliverer),
the release images and the per-host Compose bundles (`deploy/`) and the logo (`docs/logo/`); and the void that ends any
open refund (`refund`, with the reasons `cancelled` and `ledger_paid` in `store`), the extra advance of the pair stamp
after an interrupted one (`service`) and the hub's bitcoind user confined to its eleven calls (`deploy/hub`,
`dev/regtest`); and the setup command that writes every configuration file and generates every secret of the two hosts
(`setup`, `cmd/satbooth-setup`, `deploy/setup.sh` and the tools image) with the BTCPay migration guide
(`docs/migrating-from-btcpay.md`); and several webhook endpoints (`webhooks`, in `store`, `webhook` and `service`); and
the final fixes: the on-chain confirmation bound (`onchain`), the hub's network checked at start (`service`, `setup`),
the deliverers' wake-up by the store (`store`, `webhook`), the restore procedure (documentation) and the rule that an
unconfirmed payment is not marked gone while bitcoind's wallet is behind its node, for at most 60 seconds (`onchain`,
`btcrpc`, with the twelfth method of SatBooth's bitcoind user); and the items carried out of it: the webhook give-up
line kept across a crash and a 2xx that could not be recorded (`store`, `webhook`), and the on-chain rail's held orders
shown to the Monero rail of a mixed order (`rail`, `onchain`, `monero`). The design questions in `docs/design.md` §5 are
settled. New design questions are decided by the maintainer: don't pick them yourself, put the options and a
recommendation in front of the maintainer.

## Rules specific to this project

- **`main` is public and permanent.** Never rewrite, rebase or force-push it. Every change reaches `main` from a
  branch, after an independent code review and a green `scripts/check.sh`.
- **Nothing to sweep.** Never add code that stores, accepts or derives an on-chain private key, seed, xprv
  or Monero spend key. Refuse such input with a clear error. Any change that adds a way for money to leave
  goes through the threat model (§4) first.
- **This repo is public.** Nothing from a real deployment goes into committed files: no hostnames, IPs,
  node ids, account names, usage numbers, and no personal data about anyone. Deployment notes stay
  outside the repo.
- **Dependencies:** each new one is justified in the PR (what it does, why the standard library isn't
  enough, its maintenance and CVE record) and added to `docs/dependencies.md`. Prefer none.
- **Tests:** every parser/normaliser/validator gets seeded property or fuzz tests that cross every
  boundary. Money paths get tests for replay, double-pay, timeouts and restarts.
- **Secrets:** never in argv, logs, error messages or API responses, not even redacted. Load them
  in-process from a 0600 file or the environment.
- **Name:** "SatBooth" (capital B) in all text people see: docs, UI, page titles, messages. Identifiers stay
  lowercase `satbooth`: the repo, directories, the Go module, binary and domain names.
- **Acknowledgements:** any change that borrows code, test data or a design from another project adds it to
  the table of `ACKNOWLEDGEMENTS.md` in the same commit, with its license.
- **Simpler is always better.** If a feature is not in `docs/design.md` §1, propose it first.
- **Deployment bundles** for each host live in `deploy/`. The hosts themselves, their backups beyond
  `satbooth backup`, and the monitoring that receives alerts belong to the operator. The hub (and its data
  directory) never shares a host with the public-facing SatBooth service.
