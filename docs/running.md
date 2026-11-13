# Running SatBooth

## Running the relay

`satbooth-relay -config <path>` serves two listeners until SIGINT or SIGTERM. The configuration is one JSON file
with no secrets (an example is `relay/testdata/config-example.json`):

| Key | Meaning |
|---|---|
| `hub_listen` | `host:port` for the hub; the host must be loopback (`127.0.0.1` or `::1`); no login |
| `client_listen` | `host:port` for SatBooth; requires NIP-42 AUTH; the host must be a specific IP address (for example the WireGuard IP); hostnames and wildcards (empty host, `0`, `0.0.0.0`, `::`) are refused; it is plain `ws://` |
| `client_relay_url` | the `ws://` or `wss://` URL SatBooth uses; AUTH is checked against it byte for byte, so it must equal `nwc.relay_url` in SatBooth's configuration exactly |
| `pairs` | 1 to 64 entries of `wallet` (the hub's connection key) and `client` (SatBooth's key), 64 hex characters each |
| `window_seconds` | optional, 60 to 21600, default 3600: how long requests, responses and notifications are kept |
| `window_max_events` | optional, 100 to 1000000, default 10000 |
| `window_max_bytes` | optional, 1048576 to 1073741824, default 67108864 |

Run NTP on every host. The relay keeps everything in memory and may lose messages by design; see
`docs/design.md` §2.4 ("Delivery guarantees").

## Pairing with the hub

SatBooth makes its own NWC client key, so the hub never sees a secret. A first set-up goes through `satbooth-setup`,
which makes the keys and prints the links itself (`deploy/README.md`, "Set up with `satbooth-setup`"); `satbooth-pair`
remains for a later pairing, a second connection or a rotated key. It writes the key to a new
0600 file (or reuses it) and prints a link to the hub's own new-connection page:

    satbooth-pair -key <path> -hub https://hub.example -connection invoice
    satbooth-pair -key <path> -hub https://hub.example -connection payout -max-amount-sat 100000

`invoice` asks for `make_invoice`, `lookup_invoice`, `list_transactions`, `get_info` and `payment_received`
notifications. `payout` asks for `pay_invoice`, `lookup_invoice` and `get_info` only, with a budget in sats that is
never renewed; a budget of 0 is refused, because the hub reads it as no budget at all. The secret key is never printed.
`get_info` is how SatBooth learns, at every start, which network the hub is on: the hub states its network only to a
connection that holds it, and it shows in the hub's form as "Read your node info". Leave it ticked: SatBooth refuses to
start with a connection whose `get_info` names no network. The start also refuses a hub that states another network
than `nwc.network` (`the invoice connection's hub is on the network "mainnet", not nwc.network "signet"`, with the
connection's name, `payout` or `invoice`, and the two names; a name outside the four known ones is never printed:
`the invoice connection's hub reports a network SatBooth does not know`), and a connection without the permission:
`the invoice connection's get_info does not state the network; pair it with satbooth-pair -connection invoice`. The
permission discloses the node's alias, colour, public key, network, block height and block hash to the connection, and
adds no way to move money. A connection paired without it, whenever that was, is refused at start: on the
connection's page in the hub, click Edit Connection, open Custom, tick "Read your node info" and click Save (do not
pick another permission group), or pair it again. Then:

1. Open the link in the hub.
2. Check the methods and the budget (leave "Read your node info" ticked), and click Connect.
3. In the connection's "Connection Details", copy "Wallet Public Key".
4. Put it into SatBooth's configuration and into the relay's `pairs` (as `wallet`, with SatBooth's public key
   as `client`). The relay refuses one wallet key in two pairs.

## Running satbooth

`satbooth -config <path>` is the service. It serves the checkout page on a public listener and the merchant API
on a private one, and runs until SIGINT or SIGTERM. It takes Lightning payments and, with an `onchain` block in the
configuration, on-chain bitcoin; with a `monero` block, Monero; with a `rates` block it also prices orders given in a
fiat currency. It logs JSON lines to standard error with fixed messages: no key, secret, invoice, payment hash,
address, rate, text from bitcoind or wallet-rpc, or text from a rate provider. Exit code 0 is a clean stop, 1 a
failure, 2 a usage or configuration error.

What the operator provides:

- **A supervisor.** Startup is fail-fast: if the first connection to the wallet relay fails (it is not retried, and
  waits at most 30 seconds), `satbooth` logs a reason and exits 1. After that first connection the NWC client reconnects by itself.
  The same holds for bitcoind when the `onchain` block is set: if bitcoind does not answer, or a startup check fails
  (see [Setting up bitcoind](bitcoind.md)), `satbooth` logs `startup failed` with the reason `onchain` and exits 1 (30 seconds at
  most). Once running, it rides out a bitcoind outage. Likewise with the `monero` block: if wallet-rpc does not answer
  or its startup check fails (see [Setting up monerod and monero-wallet-rpc](monero.md)), `satbooth` logs `startup failed` with
  the reason `monero` and a fixed `cause` (the check's own text, for example `monero: the wallet is not view-only`,
  never wallet-rpc's), and exits 1 (30 seconds at most). Once running, it rides out a wallet-rpc outage.
  **SatBooth needs the hub to answer at every start:** it checks with `get_info` that the invoice connection holds
  only the methods `satbooth-pair -connection invoice` gets from the hub, `make_invoice` and `lookup_invoice` among
  them, and so cannot pay (and, with the `payout` block, that the payout connection holds only what refunds need). This runs with or
  without refunds configured, so a hub outage during a restart keeps SatBooth from starting, on every rail, until the
  hub answers (reason `invoice_connection` or `payout_connection`). The same `get_info` must name the network of
  `nwc.network`, or the start is refused with the same reason.
  Run `satbooth` under a supervisor that restarts it, for example systemd with `Restart=on-failure` and
  `RestartSec=5`. The public host's Compose bundle uses Docker's `restart: unless-stopped`, which also brings it back
  after a reboot (`deploy/README.md`, "Restarts").
- **Locked memory.** On Linux, before it opens any secret file, `satbooth` calls `mlockall(MCL_CURRENT|MCL_FUTURE)`,
  marks the process not dumpable (`PR_SET_DUMPABLE` 0) and sets `RLIMIT_CORE` to 0, so keys do not reach swap or a
  core dump. If locking fails it refuses to start, unless the configuration sets `allow_swap: true`, which is for
  hosts whose swap is encrypted or absent (it then logs a warning on every start). A failure to disable core dumps
  is always fatal. Grant the lock with systemd `LimitMEMLOCK=infinity` (or `CAP_IPC_LOCK`). **A fixed limit such as
  64 MB fails:** the locked amount counts the address space the Go runtime reserves, which is far more than the
  process's resident memory. In Docker use `ulimits: memlock: -1`. On macOS nothing can be locked: `satbooth`
  logs one line and continues, which is for development only.
- **A data directory.** `data_dir` holds `ledger.db` and `store.db`. `satbooth` creates it with mode 0700 and
  refuses an existing one that grants anything to group or others. Back it up with `satbooth backup` (see "Backup and
  restore"); the ledger is the record of what was paid. One `satbooth` process per data directory: at start it takes
  an exclusive lock on `satbooth.lock` there (0600), and a second one refuses to start ("another SatBooth process
  holds the data directory"). The lock file must be a regular file of the user `satbooth` runs as, with no group or
  other bits (a symbolic link is refused). Keep the data directory on a local filesystem: the lock must exclude every
  other process, which a network filesystem may not do.
- **Secret files**, each a regular file (a symbolic link is refused), loaded in-process and never put in
  argv:
  - the NWC client key (`nwc.client_key_file`): 64 lowercase hex digits and an optional final newline, mode 0600 or
    stricter. `satbooth-pair` writes it (see "Pairing with the hub").
  - the API keys (`api_keys_file`): mode exactly 0600, `{"keys":[{"id","capability","sha256"}]}` with 1 to 256 keys.
    `id` is 1 to 64 characters of `[A-Za-z0-9._-]` and unique; `capability` is `orders:write`, `orders:read`,
    `refunds:write` or `refunds:read` (see [Refunds](refunds.md)); `sha256`
    is the SHA-256 of the key, 64 lowercase hex digits. The file holds only hashes. A key is any string of up to 512
    bytes sent as `Authorization: Bearer <key>`; make one with `openssl rand -hex 32`. Each key holds one capability.
    `SIGHUP` reloads the file; if the new file is refused, the old keys stay in force.
  - with the `payout` block, the payout connection's NWC client key (`payout.client_key_file`): the same rules as
    `nwc.client_key_file`, a key of its own (`satbooth-pair -connection payout` writes it).
  - each webhook endpoint's secret (`webhooks[].secret_file`): 64 to 128 lowercase hex digits (32 to 64 bytes) and an
    optional final newline, with no group or other permission bits. Make one with `openssl rand -hex 32`. Every
    endpoint has a file and a secret of its own.
  - with the `onchain` block, the bitcoind RPC password (`onchain.rpc_password_file`): 1 to 128 printable ASCII bytes
    (`0x21` to `0x7e`) and an optional final newline, mode 0600 or stricter.
  - with the `monero` block, the wallet-rpc Digest password (`monero.rpc_password_file`): the same rules as the
    bitcoind password (the same loader).
  - with the `alerts` block, the alert secret (`alerts.secret_file`): the same rules as a webhook secret, and a
    different secret from every endpoint's.
  - with `rates.custom_url_file`, the URL of your custom rate API: one `https` URL with a host, no user info or
    fragment, no spaces or control characters, at most 512 bytes, **a query allowed** (a token, for example) if it
    parses (no `;`, every `%` followed by two hex digits) and carries no `asset` parameter (SatBooth adds its own), and an
    optional final newline, mode 0600 or stricter. The URL is never logged, nor put in an error.

**Secrets in Docker Compose** are bind-mounted files. Compose ignores a `secrets:` entry's `uid`, `gid` and `mode` for a
`file:` source outside Swarm, so the file keeps the owner and mode it has on the host. The operator therefore `chown`s
each secret file to 65532 (the user the release image runs as) with mode 0600 before the first start, and SatBooth's
loaders keep refusing anything looser. The data volume is owned by 65532 with mode 0700 in the same way. The bundles in
`deploy/` do the `chown`s with a one-shot `volume-init` container and leave the modes to the operator, or to
`satbooth-setup`, which creates every file with mode 0600 (`deploy/README.md`, "Set up with `satbooth-setup`", "The
directories").

A second SIGINT or SIGTERM exits at once; the first lets in-flight requests finish for up to 10 seconds. An
application that embeds the service, as `qualify/` does, must call `service.ReadConfig`, `service.Lock`,
`Config.CheckFiles` and `service.Run` in that order.
