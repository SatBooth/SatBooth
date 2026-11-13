# Development

Run `scripts/check.sh` before every commit. It is the offline gate: formatting, vet, module and
vendor consistency, the dependency allowlist, build, tests and the capability baseline (`scripts/capslock.sh`),
all without network access. Run `scripts/tools.sh` once first: it installs the pinned capslock.
(The vendor-consistency step needs the module cache; set `GOPROXY_VENDOR=https://proxy.golang.org` on a
cold machine.)

Supported platforms are Linux and macOS (the key-file loader uses `O_NONBLOCK`; its tests use `Mkfifo`).
The other scripts:

| Script | What it does | Network |
|---|---|---|
| `scripts/tools.sh` | installs the pinned `govulncheck` and `capslock` into `.tools/bin` | yes |
| `scripts/vulncheck.sh` | runs `govulncheck ./...`; fails on any finding | yes (vulnerability database) |
| `scripts/capslock.sh [update]` | fails if a capability was added versus `docs/capslock.json`; `update` rewrites the baseline (offline once `tools.sh` has installed capslock); `check.sh` runs it | no |
| `scripts/fuzz.sh [seconds]` | runs every `Fuzz*` target in turn (default 30 s each) | no |
| `scripts/image-check.sh <image> <allowed file>...` | lists every entry in every layer of a built release image and fails on any file not named | no |
| `scripts/image-run-test.sh <satbooth image> <relay image> <tools image>` | runs each release image as it ships (read-only root, no capabilities, user 65532) with a missing configuration (the tools image: `/satbooth-setup` with no arguments), and checks the refusal, the user and the environment | no |
| `scripts/volume-init-test.sh` | runs each bundle's `volume-init` twice on directories laid out like an operator's and checks the owners and modes (Docker; on Linux, where a bind mount keeps the host's owner) | to pull the pinned images |

CI (`.github/workflows/check.yml`) runs the first four; `.github/workflows/image.yml` builds all three release images
(`deploy/README.md`, "Build and check") and runs the last three. `scripts/check.sh` also checks both Compose bundles
with `docker compose config` when Compose is installed (CI has it).

**The rate providers.** The offline tests parse recorded answers of Kraken (BTC and XMR), Coinbase, Yadio and
Bitfinex in `rates/testdata/` (Kraken's trimmed to its seven BTC pairs and its two XMR pairs, Coinbase's to those seven
codes plus its longest fraction when that is longer, Yadio's and Bitfinex's whole, with their response headers in
`*.meta.json`; `custom.json` is written by hand in SatBooth's format),
and `TestFixturesAgree` checks that the three recordings agree with each other, within 2%, in each of Kraken's seven
currencies.
The service's tests reach a fake rate server (`rates/ratestest`) through `service.WithRateTransport`, never the
network. Two tests, with the build tag `live`, call the real providers and are run by hand only:

- `go test -tags live -run '^TestLiveSmoke$' -v ./rates/` runs the live smoke test, `TestLiveSmoke`: one request to each
  built-in provider, then the median, the spread and whether the default rules would price an order, all logged. It
  fails on a changed answer format or a 4xx other than 429 (a request the provider no longer accepts), not on a
  provider that is down or limiting. It does not rewrite anything.
- `SATBOOTH_RECORD_FIXTURES=1 go test -tags live -run '^TestLiveRecord$' -v ./rates/` re-records the fixtures: one
  request per provider, the bodies (trimmed as above; `TestFixturesAreTrimmed` checks the committed ones are) and
  headers written to `rates/testdata/`, then the checks the vendors' docs do not
  cover (Kraken's key for each of its seven currencies, Coinbase's longest fraction, Yadio's timestamp lag, the USD
  spread). Without the variable it skips. Review the new files before you commit them (public market data, a few
  hundred bytes to a few KB each); the parser tests and `TestFixturesAgree` then run against them.

Qualification against real hubs is separate and dev-only: `scripts/qualify.sh [--keep]` builds a regtest
environment from `dev/regtest/compose.yml` (Bitcoin Core, two Alby Hub containers, a SatBooth relay beside each
hub, monerod, two monero-wallet-rpc (`wallet-customer`, which holds the test's customer and merchant wallets, and
`wallet-satbooth`, SatBooth's view-only wallet behind a Digest login) and a Go test runner, all on an internal network
with no internet) and runs the tests in `qualify/`, which carry the build tag `qualify`, so `scripts/check.sh` never
compiles them. It needs Docker with Compose and, on first use, the network to pull the pinned images, build bitcoind
from the Bitcoin Core release tarball and the Monero image from the Monero v0.18.5.4 release tarball (`dev/regtest/monero/`,
its SHA-256 pinned from the PGP-signed `hashes.txt`). The Lightning phases wait for an invoice to expire in real time, so they take about 20 minutes (1,230 s in plan 5's last run, on Docker Desktop with the images already
built and cached); the on-chain phases follow, against the same bitcoind through SatBooth's own whitelisted RPC user (`dev/regtest/bitcoind/bitcoin.conf`), then the Monero phases, against `wallet-satbooth` through SatBooth's own client, then the refund phases (about 12 minutes, with the payout connection and the merchant's hub as the claimant's wallet), then the release images' phases (a `satbooth` container of the release image driven over HTTP only; the relays always run from the release image) and the public host's bundle (`deploy/public/compose.yml` with its `compose.build.yml`); the whole suite took 84 minutes in plan 10's run; a first run also pulls and builds the images and takes longer (not measured). `qualify/REPORT.md` lists what the phases measure and which ran on the image. `--keep` leaves the
environment up. `--only deploy` runs the setup and the public bundle's phases alone, `--only image` the setup,
`TestOnchainSetup`, `TestMoneroSetup` and the release image's phases, `--only refund` the setup and the refund phases
(about 15 minutes, most of it the 11-minute void wait), `--only hub` the setup and `TestQualify` with the check on the
hub's unknown-app lines (about 1.5 minutes), and `--only onchain` the setup and every on-chain phase with the bitcoind
kills between them (about 15 minutes), for a targeted check; none is the qualification. `QUALIFY_CRASH_REPEATS=<n>`
repeats the hub stop and kill phases; each round spends part of the payout connection's
50,000 sat test budget, which the refund phases share (they pay about 5,100 sat), so about 12 rounds in all is the limit (an estimate from the budget arithmetic). `.github/workflows/qualify.yml` runs it by hand and weekly.
