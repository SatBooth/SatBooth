# Deployment

This directory holds the release images (`image/`) and one Docker Compose bundle per host: `hub/` for the private
hub host and `public/` for the public host. The two hosts are never one machine (CLAUDE.md).

**Deploy from a release tag.** The bundles and `deploy/setup.sh` pin the three SatBooth images by tag and digest, and
the pins in a tag's tree are the ones that release published. On both hosts, check out the tag of the release you
deploy and verify it with `git tag -v <tag>`, after importing the release signing key and comparing its fingerprint
(`SECURITY.md`, "Release signing key"). Between releases, the pins on `main` name the last release while the code around
them may be newer. "Release images" below says how to verify the images themselves, and `docs/releasing.md` is the
maintainer's procedure.

## Set up with `satbooth-setup`

`satbooth-setup` writes every configuration file of a host and generates every secret, once per host: you answer a few
questions and it writes what the tables under "Per-host bundles" describe, with the modes and the values the bundles
expect. It opens no network connection and runs no other program: it prints the commands you run next. It never
overwrites a file, and it never prints a secret, a descriptor or a Monero address. The same files can still be written
by hand, as the tables below describe. `qualify/REPORT.md` ("Deployment bundles") records the bundles started from the
setup command's output.

`deploy/setup.sh` pulls the pinned release tools image and runs one stage of the command in it. With `SATBOOTH_BUILD=1`
in the environment it builds that image from this repository on every run instead (the Go build is offline, since the
modules are vendored, but the first build needs Docker to pull the pinned base image).
The container the stage runs in has no network, a read-only root, no capabilities and only the setup directory (and the
files you name) visible. Stages run in a fixed order, two on each host, and each refuses to run out of turn.

What the setup command supports: the two bundles as they are, a new hub with its own bitcoind. The bundle's dedicated
bitcoind is the only supported node: an existing bitcoind, an existing Alby Hub, a hub on LND or Core Lightning and a
hub hosted by Alby are not supported. If you come from BTCPay Server, `docs/migrating-from-btcpay.md` says what moves
and what does not.

### Before you start

- **Two hosts**, the hub host (private) and the public host. They are never one machine.
- **The private link between them**, with an IPv4 address on each (WireGuard, or a private network and a firewall that
  admits only the public host to the hub host's ports; see "Per-host bundles"). Setting it up is yours. The two
  addresses are answers, and each must be outside the bundles' own Docker networks.
- **Docker** with Compose on both hosts, and this repository checked out on both, at the release tag you deploy. The
  setup wrapper needs Docker Engine in its default mode: `deploy/setup.sh` refuses a rootless engine or Podman ("a rootless Docker engine or Podman
  is not supported by this wrapper (the bundles are qualified on Docker Engine in its default mode)"), because the
  bundles are qualified on the default mode only.
- **From the admin system, the refund approval public key or keys**, if you want refunds: `satbooth-approve keygen
  -out <path>` there prints the public key (`docs/refunds.md`, "Refunds" and "`satbooth-approve`"). The admin's signing
  key lives on neither host, and `satbooth-approve` is not in the tools image: the setup command takes public keys only.
- For the on-chain rail, a watch-only descriptor of a new account (`docs/bitcoind.md`, "Setting up bitcoind
  (on-chain)"); for Monero, the primary address, and later the private view key and a restore height (`docs/monero.md`,
  "Setting up monerod and monero-wallet-rpc (Monero)").

### The answers

Each host is given its answers once, at its first stage: from a file (`-answers <file>`, one JSON object of at most 16
KiB, keys as in the tables, an unknown or repeated key refused) or, without `-answers`, by questions on the terminal. A
key left out of a file takes the default below; a key without a default is required. `host_dir` is the base directory as
the host sees it: the absolute path of `<dir>` with symbolic links resolved (letters, digits and `/._-` only, at most
200 bytes). A `<dir>` that is itself a symbolic link (also when written with a trailing `/` or `/.`), or that is `.`,
`..` or ends in `/..`, is refused by the wrapper: give the real directory. A real directory written with a trailing
`/` or `/.` is the directory itself. The wrapper gives it to the command, so prompts do not ask for it; an answers
file must contain it, with exactly that value, and a file with another path is refused with "satbooth-setup: setup:
the answers' host_dir is not the directory this command was given (-host-dir)". The file is yours to delete
afterwards: it may hold the descriptor and the Monero address, and the setup directory's `state.json` keeps a copy of
the answers (it is mode 0600, and no service mounts it).

**The public host, stage 1:**

| Answer | Meaning | Default |
|---|---|---|
| `network` | `mainnet`, `signet` or `regtest` | none |
| `host_dir` | this host's base directory, as above; required in a file, given by the wrapper with prompts | none |
| `hub_private_addr` | the hub host's address on the private link: an IPv4 address, not in `172.30.78.0/24`, not link-local or reserved; loopback only on `regtest` | none |
| `public_private_addr` | this host's address on the private link: the same rules, and different from the hub's | none |
| `public_addr` | the address the checkout page is published on (`PUBLIC_ADDR`): `127.0.0.1` behind a reverse proxy, never `0.0.0.0` | `127.0.0.1` |
| `public_port` | the port it is published on (`PUBLIC_PORT`) | `8080` |
| `checkout_base_url` | the checkout page's public URL: `http` or `https`, no query | none |
| `webhook_url` | where SatBooth sends signed payment webhooks | none |
| `alerts_url` | where operator alerts go; must differ from `webhook_url`; empty: no alerts | empty |
| `descriptor` | the watch-only descriptor of the on-chain rail (`wpkh` or `tr`, one extended public key, `/0/*`; a private key is refused); an `xpub` on `mainnet`, a `tpub` otherwise; empty: no on-chain rail | empty |
| `monero_address` | the Monero primary address (checked as a mainnet address); empty: no Monero rail | empty |
| `monero_network` | `mainnet`, the only value: the hub bundle runs Monero on mainnet only; required with `monero_address` (the prompt offers it as the default) | none |
| `approval_keys` | the refund approval public keys (1 to 8 distinct keys of 64 lowercase hex characters each); empty: no refunds | empty |
| `payout_budget_sat` | the payout connection's budget in sats; it goes into the pairing link | none |
| `payout_max_sat` | the most one refund may pay, in sats; at most the daily cap and the budget | none |
| `payout_daily_sat` | the most refunds may pay in a UTC day, in sats | none |
| `payout_alert_sat` | alert when the budget's headroom falls below this many sats; `0`: no alert; below the budget | none |
| `currencies` | the fiat currencies to price in, such as `["EUR","USD"]` (at most 32 distinct codes of 3 uppercase letters, not `BTC` or `XMR`); empty: no fiat pricing | empty |

The four refund caps have no default: money leaves through refunds, so every cap is yours to give, and each is asked
whenever `approval_keys` is not empty (and refused in a file without it). `network` must name the chain the hub is on:
`testnet` is refused (see "What the setup command leaves to you").

**The hub host, stage 2:** the first three are typed here as well and compared with the public host's: the handoff
carries the public host's copies so that the stage can refuse when the two hosts were given different values.

| Answer | Meaning | Default |
|---|---|---|
| `network` | as above; the same value as on the public host | none |
| `host_dir` | this host's base directory, as above | none |
| `hub_private_addr`, `public_private_addr` | the same two addresses as on the public host, under the same rules (outside `hub_subnet` and `electrum_subnet` too) | none |
| `hub_admin_addr` | where the hub's admin page is published (`HUB_ADMIN_ADDR`): an IPv4 loopback or private address, never one of the two link addresses and not inside `hub_subnet` or `electrum_subnet` | `127.0.0.1` |
| `hub_subnet` | the hub bundle's Docker network (`HUB_SUBNET`): an IPv4 network of `/16` to `/29` | `172.30.78.0/25` |
| `hub_ip` | the hub's fixed address in it (`HUB_IP`): not its network, gateway or broadcast address | `172.30.78.10` |
| `electrum_subnet` | the bundle's internal Docker network (`ELECTRUM_SUBNET`); must not overlap `hub_subnet` | `172.30.78.128/25` |

A minimal answers file for the public host (Lightning only):

```json
{
  "network": "mainnet",
  "host_dir": "/srv/satbooth",
  "hub_private_addr": "192.0.2.20",
  "public_private_addr": "192.0.2.10",
  "checkout_base_url": "https://pay.example.com",
  "webhook_url": "https://hooks.example.com/satbooth"
}
```

and for the hub host (every other answer is a default):

```json
{
  "network": "mainnet",
  "host_dir": "/srv/satbooth-hub",
  "hub_private_addr": "192.0.2.20",
  "public_private_addr": "192.0.2.10"
}
```

With questions instead of a file, a prompt shows its default in brackets and an empty line takes it; `-` clears an
optional answer. Related answers are checked as a group after the last of them. When one is refused, the reason is
printed (it never repeats what you typed) and the group is asked again with your other answers kept; a second refusal
of the same answer asks every question again, with `[keep]` in place of the old answer (an empty line keeps it, and it
is never displayed).

### The stages

In the commands, `<dir>` is the base directory of the host: a path of your own choosing, such as `/srv/satbooth` on the
public host and `/srv/satbooth-hub` on the hub host. `deploy/setup.sh` creates it with mode 0700 if it is missing. Run
every command from the root of this repository. The command prints its report on standard output and its prompts on
standard error; its exit code is 0 when done, 1 when it refused or failed and 2 for a usage error (an unknown stage or
flag, an extra argument, or `-host-dir`, `-hub-url`, `-answers`, `-wallet-rpc-pass` or `-handoff` given empty).

1. **`public init`, on the public host.** Writes SatBooth's secrets and the handoff for the hub host.

   ```sh
   deploy/setup.sh <dir> public init -answers <answers file>
   ```

   (Leave out `-answers` to be asked. `-hub-url` is the address of the hub's admin page as you will reach it through
   the tunnel; it defaults to `http://127.0.0.1:8080`, and only the pairing links carry it.) It writes, under `<dir>`:
   in `etc/`, `nwc-client.key`, `webhook.secret`, `api-keys.json` (the hashes of the API keys, never the keys) and, as
   the answers call for them, `payout.key` (refunds), `alerts.secret` (alerts) and `bitcoind.pass` (on-chain); in
   `setup/api-keys/`, one plain API key per file (`orders-write.key`, `orders-read.key` and, with refunds,
   `refunds-write.key` and `refunds-read.key`); and `setup/public-to-hub.json`, the handoff. It prints:
   - the pairing links: the invoice link and, with refunds, the payout link. **Do not open them yet.** Keep them for
     step 2.
   - the digest of the handoff file (eight groups of eight hex characters).
   - the steps to take: copy `<dir>/setup/public-to-hub.json` to the hub host, and give the hub host the digest to
     compare. Copy the file by any channel you trust (`scp`, for one); take the digest by another, if you can (see
     "The handoff files and their digests").

2. **`hub init`, on the hub host.** Writes the hub host's configuration and secrets, then you start the hub alone and
   pair it.

   ```sh
   deploy/setup.sh <dir> hub init -handoff <public-to-hub.json> -digest "<the digest of step 1>" -answers <answers file>
   ```

   (Without `-digest` it asks for it. Without `-answers` it asks the questions.) The digest is checked before anything
   is written. The stage writes, under `<dir>`: in `config/`, `hub.env`, `fulcrum.conf` and `bitcoin.conf` (with the
   `rpcauth` lines of the hub's, Fulcrum's and, with the on-chain rail, SatBooth's bitcoind users and their
   `rpcwhitelist` lines) and, with Monero, `monerod.conf`, `wallet-rpc/rpc-login` and `wallet-rpc/wallet.pass`;
   `compose.env`; a copy of the handoff it read, `setup/read-public-to-hub.json`; and, with Monero,
   `setup/carry/wallet-rpc.pass`, the copy of the wallet-rpc login password that step 3 carries to the public host.
   It prints the digest it computed (it matched the one you gave) and these steps, which you do in this order. Below,
   `dc` stands for `docker compose -f deploy/hub/compose.yml --env-file <dir>/compose.env`.

   1. **Start the hub alone:** `dc up -d --build hub`. This starts `volume-init`, `netns`, bitcoind and the hub.
   2. **Open an SSH tunnel to its admin page** from your workstation, with the address of `hub_admin_addr` (loopback by
      default): `ssh -L 8080:<hub_admin_addr>:8080 <hub host>`, then open `http://127.0.0.1:8080`.
   3. **Set the unlock password now, before anything else.** A fresh hub belongs to whoever opens its page first.
   4. **Open the two links from step 1** in the same browser (the second only with refunds). Check the methods and the
      budget, leave "Read your node info" ticked (the hub states its network only to a connection that holds it, and
      SatBooth refuses to start with a connection whose `get_info` names none; leave it ticked on both connections: an
      unticked one makes the start fail with reason `invoice_connection` or `payout_connection` and the cause "the
      invoice connection's get_info does not state the network; pair it with satbooth-pair -connection invoice"
      (`payout`
      in both places for the payout connection)),
      click Connect, and in each connection's "Connection Details" copy the "Wallet Public Key": the invoice
      connection's, then the payout connection's. You need them in step 3. (`docs/running.md`, "Pairing with the hub".)
   5. **With the on-chain rail, make the watch-only wallet and import the descriptor** once bitcoind runs. Let `bc`
      stand for `dc exec bitcoind bitcoin-cli -conf=/etc/bitcoin/bitcoin.conf -datadir=/data`:

      ```sh
      bc -named createwallet wallet_name=satbooth disable_private_keys=true blank=true descriptors=true \
        load_on_startup=true
      bc getdescriptorinfo "<your descriptor>"
      bc -named -rpcwallet=satbooth importdescriptors \
        requests='[{"desc":"<your descriptor with its #checksum>","active":true,"internal":false,"timestamp":"now"}]'
      ```

      `getdescriptorinfo` prints the descriptor's checksum, which the import needs after a `#`. `docs/bitcoind.md`,
      "Setting up bitcoind (on-chain)", explains each command and what to check after it.
   6. **With Monero, make the view-only wallet** once monerod runs (it is in the `monero` profile; the bundle's own
      Compose command `dc --profile monero up -d monerod` starts it alone, and the setup command does not print that
      line):

      ```sh
      dc --profile monero run --rm --no-deps --entrypoint monero-wallet-cli wallet-rpc \
        --generate-from-view-key /wallet/satbooth-view --password-file /etc/wallet-rpc/wallet.pass \
        --restore-height <height> --daemon-address monerod:18081 --log-file /tmp/wallet-cli.log
      ```

      It asks for the primary address and the private view key (the setup command never asks for it). The new wallet's
      password is read from `/etc/wallet-rpc/wallet.pass` in the container, which is the file
      `<dir>/config/wallet-rpc/wallet.pass` (stage 2 wrote it, and wallet-rpc opens the wallet with it), so you neither
      type nor read it; do not paste it anywhere. `docs/monero.md`, "Setting up monerod and monero-wallet-rpc (Monero)",
      has the restore height and the checks.

3. **`hub pair`, on the hub host.** Writes the relay's configuration from the Wallet Public Keys and the handoff back
   for the public host.

   ```sh
   deploy/setup.sh <dir> hub pair -invoice-wallet <invoice Wallet Public Key> -payout-wallet <payout Wallet Public Key>
   ```

   With refunds off, give the payout flag empty: `-payout-wallet ""`. A flag left out makes the stage ask for that key
   (at its prompt, an empty line means no payout key), so a script must give both flags. It writes `config/relay.json`
   (the pairs: the hub's keys and the client keys of step 1) and `setup/hub-to-public.json`. It prints the digest of
   that file and these steps:
   - **Start the rest:** `dc up -d --build`, or with Monero `dc --profile monero up -d --build`. The relay starts now
     that `relay.json` exists. With Monero, make the view-only wallet first (step 2.6): wallet-rpc refuses to start
     until the wallet exists.
   - Give the public host's operator the digest to compare.
   - Copy `<dir>/setup/hub-to-public.json` to the public host. **With Monero,** also copy
     `<dir>/setup/carry/wallet-rpc.pass` there as a file of mode 0600, then delete it on this host.

4. **`public finish`, on the public host.** Writes `satbooth.json` and the rest of what SatBooth reads.

   ```sh
   deploy/setup.sh <dir> public finish -handoff <hub-to-public.json> -digest "<the digest of step 3>" \
     -wallet-rpc-pass <the carried file>
   ```

   (`-wallet-rpc-pass` only with Monero; it must be exactly what step 2 wrote: 64 lowercase hex characters and a
   newline, mode 0600.) It writes `etc/satbooth.json`, with Monero `etc/wallet-rpc.pass`, `compose.env` and
   `setup/read-hub-to-public.json`. It prints the start command:

   ```sh
   docker compose -f deploy/public/compose.yml --env-file <dir>/compose.env up -d --build
   ```

   Hand over the secrets (step 5) before you run it. (The printed commands use the pinned release images. To build
   them from source, see "Build from source" under "Release images".)

   **Run `public finish` before the first start of this bundle**: at the first start `volume-init` gives `etc/` to the
   user 65532, and the setup command then refuses the directory: "setup: etc belongs to another user: give it back to
   the user running this command (chown), or set up from a fresh directory". If the bundle was started before stage 4,
   give the directory back to the calling user with `sudo chown "$(id -u):$(id -g)" <dir>/etc`, run stage 4, and start
   again (`volume-init` gives `etc/` to 65532 again).

5. **After stage 4, before the public bundle's first start, and afterwards.**
   - Hand over the secrets now, while you can still read them. The merchant's application gets the plain API keys in
     `<dir>/setup/api-keys/` on the public host (one file per key: the orders keys for creating and reading orders, the
     refunds keys for refunds) and the webhook secret `<dir>/etc/webhook.secret` (the endpoint `main`'s), to verify
     webhooks (`docs/merchant-api.md`, "The merchant API", and `docs/webhooks.md`, "Webhooks"). The receiver of alerts
     gets `<dir>/etc/alerts.secret` (`docs/alerts.md`, "Alerts"). After the first start `etc/` and its files belong to
     the user 65532, and reading them needs root (for example `sudo cat`); the plain API keys in `setup/api-keys/` stay
     readable, because no service touches `setup/`.
   - Delete the carried password everywhere it is a copy: the channel you copied it through, `setup/carry/` on the hub
     host and the file you gave to `-wallet-rpc-pass` on the public host (`etc/wallet-rpc.pass` is the one that stays).
     Delete the handoff copies you made on your workstation, and the answers files.
   - Unlock the hub in its admin page after each hub restart; SatBooth cannot start until it answers (see "The hub
     host" and "Restarts").

### Adding a webhook endpoint

After setup `satbooth.json` has one webhook endpoint, named `main`, whose secret is `etc/webhook.secret`. Further
endpoints receive every event too, each with a secret of its own (`docs/webhooks.md`, "Webhooks", and
`docs/configuration.md`, "Configuration"). To add
one:

1. Make its secret file in `etc/`: 64 to 128 lowercase hex digits (32 to 64 bytes), mode 0600, different from every
   other secret in the list and from the alert secret (the `alerts` block's `secret_file`). After the first start
   `etc/` belongs to the user 65532 with mode 0700, so write the file as root, for example
   `sudo sh -c 'umask 077; openssl rand -hex 32 > <dir>/etc/second.secret'`.
2. Add an entry to `webhooks` in `<dir>/etc/satbooth.json`, with a name of your own that is not yet in the list, the
   receiver's URL and the file's path as the container sees it:

   ```json
   { "name": "second", "url": "https://hooks.example.com/second", "secret_file": "/etc/satbooth/second.secret" }
   ```

3. Give the new file to the user that reads it: `volume-init` does that (the bundle's one-shot container, which
   gives `etc/`'s files to 65532 and never changes a mode; see "Per-host bundles"). Then restart SatBooth, which reads
   the configuration at start only:

   ```sh
   dc() { docker compose -f deploy/public/compose.yml --env-file <dir>/compose.env "$@"; }
   dc run --rm --no-deps volume-init
   dc restart satbooth
   ```

A refused configuration (a name, URL or secret file used twice, two equal secrets, a ninth entry) stops the start with
a message that names the entry by its position counted from 0, `webhooks[1]` being the second entry, and shows no URL
or secret. The new endpoint is sent the events of the last 24 hours at its first delivery pass. Give the receiver the
secret file's content, by a channel you trust. To remove an endpoint, delete its entry and restart; its recorded
attempts stay.

### The directories

Each host has one base directory `<dir>` with three kinds of content:

- `<dir>/setup/`: the command's own files: `state.json`, the handoffs written and read, the plain API keys
  (`api-keys/`, public host) and `carry/wallet-rpc.pass` (hub host, with Monero). **No service mounts it.** `state.json`
  holds the stage reached, the answers and the public values later stages need, never a secret.
- On the public host, `<dir>/etc/` is `SATBOOTH_ETC`, `<dir>/backups/` is `SATBOOTH_BACKUPS`, and `<dir>/compose.env`
  is the bundle's env file.
- On the hub host, `<dir>/config/` is `HUB_CONFIG_DIR`, `<dir>/wallet/` is `MONERO_WALLET_DIR`, and
  `<dir>/compose.env` is the bundle's env file.

The base directory and every directory a stage writes into must be a real directory (never a symbolic link) that
belongs to the user running the command and has mode 0700; the command refuses otherwise. The refusals are "setup: the
base directory (-dir) must have mode 0700: run chmod 700 on it" and "setup: the base directory (-dir) belongs to another
user: give it back to the user running this command (chown), or use a fresh directory"; for `setup/`, `etc/` and
`config/`, "setup: <name> must have mode 0700: run chmod 700 on it, or set up from a fresh directory", "setup: <name>
belongs to another user: give it back to the user running this command (chown), or set up from a fresh directory" and
"setup: <name> must be a real directory (not a symbolic link): set up from a fresh directory". A first
stage also refuses a base directory that already holds any of its
directories. Every file is created with mode 0600 and every directory with 0700, whatever the umask. The base
directory's path may hold only letters, digits and `/._-` (it goes into a Compose env file), and the wrapper refuses a
colon in any path it mounts (Docker's mount syntax).

**What `volume-init` changes at the first start.** On the public host it gives `etc/` and `backups/` to the user
65532 and sets them to 0700, and `etc/`'s files go with it: after the first start you read them as root. On the hub
host it gives each configuration file to the user that reads it (the hub, bitcoind, Fulcrum, and with Monero
`monerod.conf` and `wallet-rpc/` to 10002; `relay.json`, if it exists, to 65532) and `wallet/`, Monero or not, to 10002,
and keeps the file modes.
It never touches `setup/` or `compose.env`. That is why a stage that writes into `etc/` (stage 4) must come before the
public host's first start.

### The handoff files and their digests

Two files cross between the hosts, and they carry public values only:

- `public-to-hub.json` (stage 1 to 2): the network and the two private addresses (compared, never used), the two NWC
  client public keys (the second is empty without refunds), SatBooth's `rpcauth` line (empty without the on-chain
  rail), and whether Monero is on and the login's user name.
- `hub-to-public.json` (stage 3 to 4): the hub's two Wallet Public Keys.

The one secret that crosses is the Monero wallet-rpc login's password, as the carried file, because its HTTP Digest
login needs it in plain text on both hosts. It is in neither handoff.

The stage that writes a handoff prints the SHA-256 of its bytes, in eight groups of eight hex characters. The stage
that reads it takes that digest from you (`-digest`, or a prompt: quote it in a shell, or give it without spaces) and
refuses, **before it writes anything**, a file whose digest differs. The file holds no digest of itself. The digest
protects against a corrupted copy and against a channel that carries only one of the two; if one channel carries both
the file and the digest, whoever controls it can replace both, so take the digest by another channel when you can.

A stage refuses, with these messages (each is printed after `satbooth-setup: `, and nothing is written):

| Cause | Message |
|---|---|
| the digest differs, or is not 64 hex characters | `setup: the handoff file's digest is not the one given; nothing was written` |
| the file is empty (one over 4096 bytes is read cut short, so its digest does not match: the first row) | `setup: the handoff file is empty or larger than 4096 bytes` |
| not JSON of the right shape, a key missing, repeated or unknown, or another version | `setup: the handoff file is not a handoff of this version` |
| the other direction's file | `setup: this is the other host's handoff file` |
| a value that is not allowed (an address that is not an allowed private-link address, an unknown network, equal keys, a malformed `rpcauth` line) | `setup: the handoff file holds a value that is not allowed` |
| stage 2: the network or an address differs from your answers | `setup: the two hosts were given different values: network` (or `hub_private_addr`, `public_private_addr`) |
| stage 3 or 4: refunds are on at one host and off at the other | `setup: the two hosts were given different values: payout Wallet Public Key (refunds are on on the public host)` at stage 3 (`off` in the other case) and `setup: the two hosts were given different values: payout_wallet (refunds are on on this host)` at stage 4 |
| stage 4: the carried file given without Monero, or missing with it | `setup: the two hosts were given different values: wallet-rpc password file (Monero is on on this host)` (or `off`) |
| stage 4: the carried file is not what stage 2 wrote | `setup: the carried password file must be 64 lowercase hex characters and a final newline` |
| stage 4: the carried file is readable by group or other | `the carried password file must be mode 0600 (no group or other access)` |

### If a stage is interrupted

A stage journals what it is about to create (`setup/journal.json`) before it writes anything, writes every file under a
temporary name, links each into place and replaces `setup/state.json` last, so a stage run twice, or after a crash,
refuses and changes nothing. After a crash the next run of the stage says:

```
satbooth-setup: setup: an earlier run of this stage did not finish; run it with -discard-incomplete
```

Run the same command with `-discard-incomplete` (and the same `<dir>`; it takes no answers and asks nothing):

```sh
deploy/setup.sh <dir> hub init -discard-incomplete
```

The journal holds, for every file the run may create, the SHA-256 of the exact bytes it writes (a hash of a secret
reveals nothing of it). `-discard-incomplete` removes a journaled final file only if it exists and its content is that,
and the command's own scratch files (`setup/state.json.tmp` and the files in `setup/tmp-<stage>/`) whatever their
content: it removes the files the unfinished run wrote and the directories it made that are empty (a directory that holds anything
else stays), puts back the exact `state.json` the run replaced, and removes the journal last. A file that exists under a
journaled name with other content (made by hand after the run died, or changed since) is left in place, and the command
prints one `kept <name>` line for it:

```
removed config/relay.json
kept compose.env
Each kept file was not written by the unfinished run (it has other content): it is still there, and the stage will not
run until you move or delete it.
The unfinished run is discarded.
```

The stage then refuses to run while a kept file is in its way ("a file this stage writes already exists: ..."), as for
any file that exists. `-discard-incomplete` works only on the journal of its own stage and refuses, removing nothing, a
journal that names a path the stage never writes, is of another stage, has a missing or malformed hash, or is empty or
unreadable (inspect it and delete it by hand). `state.json` and `journal.json` are read as strictly as the handoffs: one
JSON object, every key exactly once, no unknown key, nothing after the object. If a crash or a discard leaves an empty
`setup/` behind, the stage refuses with "a file this stage writes already exists: setup (an empty setup/ was left
behind by an interrupted run or discard: remove it with rmdir and run the stage again)", and `-discard-incomplete` says
"there is nothing to discard: setup/ is empty (an interrupted run or discard left it): remove it with rmdir". With no
unfinished run at all (a fresh directory, or a finished stage) it says "setup: there is nothing to discard: no earlier
run of a stage was left unfinished".

Only one `satbooth-setup` run can use a directory at a time, a stage or `-discard-incomplete`: a second one is refused
with "setup: another satbooth-setup run is using this directory", before it asks a question, reads an answers or
handoff file, or writes anything in the directory. The lock is held from there until the command ends. Run it again
when the first has ended.

Two other refusals name their fix. A stage run out of turn says "this stage does not follow the one recorded in
setup/state.json"; a directory set up for the other host says "this directory was set up for the other host"; a stage
whose files exist lists them ("a file this stage writes already exists: ..."): the setup command never replaces them.

### What the setup command leaves to you

- **Testnet, for development.** SatBooth runs on `testnet` and the other test networks for development work; a shop
  runs on mainnet. The setup command writes no testnet setup: it refuses `testnet` with "set up testnet by hand
  (deploy/README.md)". Write the files by hand from the tables under "Per-host bundles". The hub's `HUB_NETWORK` is
  `bitcoin` or `signet`: the hub maps `testnet` to signet (its `lnclient/ldk/ldk.go:206-213`, quoted in the Values
  table), so a testnet hub is a signet hub, and bitcoind and SatBooth's `network` must name the chain the hub is on.
  Give such a hub `HUB_NETWORK=signet`, never `testnet`: a hub set to `testnet` states `testnet` while it runs on
  signet, so SatBooth refuses it at start under `signet`, and refuses its invoices under `testnet`.
- **A Monero network other than mainnet.** The hub bundle runs Monero on mainnet only (its `monerod.conf`, the
  `monerod` command and `wallet-rpc.sh` carry no network flag), so the setup command refuses `stagenet`, `testnet` and
  `regtest` as `monero_network`, with a message that says to set up monerod and wallet-rpc by hand (`docs/monero.md`,
  "Setting up monerod and monero-wallet-rpc (Monero)") and to add the `monero` block to `satbooth.json`. Leave
  `monero_address` empty in the answers, set up the Monero side by hand and add the block to `satbooth.json` after
  stage 4.
- **IPv6.** The bundles were qualified on IPv4 only, and the setup command refuses an IPv6 address in three places:
  `hub_private_addr` or `public_private_addr` with "setup: answers: hub_private_addr is an IPv6 address: set up an IPv6
  private link by hand (deploy/README.md)" (the field's name is the answer's); `public_addr` with "setup: answers:
  public_addr is an IPv6 address: set up an IPv6 listener by hand (deploy/README.md)"; and `hub_admin_addr` with "setup:
  answers: hub_admin_addr must be an IPv4 address (not a hostname, a zone, or a reserved, wildcard, multicast or
  link-local address)". By hand: `PUBLIC_ADDR` and `HUB_ADMIN_ADDR` are the host part of a published port, written
  `"${PUBLIC_ADDR}:<port>:8080"` and `"${HUB_ADMIN_ADDR}:8080:8080"` in the compose files, so an IPv6 value needs
  brackets there, in the SSH tunnel's `-L` option and in the reverse proxy; none of this was qualified. For the private
  link, the address is in these values: on the hub host, `HUB_PRIVATE_ADDR` (the relay's, bitcoind's and wallet-rpc's
  published ports, written the same way) and `client_relay_url` in `relay.json`, and the `rpcallowip` line of the public
  host in `bitcoin.conf`; on the public host, `nwc.relay_url`, `onchain.rpc_url`, `monero.wallet_rpc_url` and
  `payout.relay_url` in `satbooth.json`. Check that each accepts the address form, and bracket it where a port follows.
- **The private link itself:** the tunnel or network between the hosts and its firewall.
- **A TLS reverse proxy in front of the checkout page,** which serves `checkout_base_url`. `public_addr` defaults to
  `127.0.0.1`, for a proxy on the same host.
- **Changing a default afterwards.** The setup command writes the defaults of the example configuration (limits, the
  confirmation tiers, `rates.margin_percent` of `"0"`, and so on). Change one by editing `etc/satbooth.json`
  (`docs/configuration.md`, "Configuration"; the file is the service's user's after the first start, so edit it as root)
  and restarting SatBooth. For `hub.env`, `bitcoin.conf` and the others, edit the file under `config/` the same way.
- **A second pairing or a rotated key,** with `satbooth-pair` (`docs/running.md`, "Pairing with the hub"), which the
  tools image also holds as `/satbooth-pair`.

## Release images

`deploy/image/Dockerfile` builds three images, `satbooth`, `satbooth-relay` and `satbooth-tools`. Each is `FROM scratch`: a static Go
binary and nothing else. They run as the numeric user `65532:65532` (there is no passwd file). Run them with a read-only root filesystem
(`--read-only`), all capabilities dropped (`--cap-drop ALL`) and `--security-opt no-new-privileges:true`.
`satbooth` also needs `--ulimit memlock=-1` (it refuses to start without it, unless `allow_swap` is set) and a data
volume owned by 65532 with mode 0700. These are run options, not image content. `scripts/image-check.sh` lists every
entry in every layer of a built image and fails on anything but the files below (regular files and directories only);
CI runs it on each image.

### Where the images live and how they are pinned

A release publishes the three images to `ghcr.io/satbooth/satbooth`, `ghcr.io/satbooth/satbooth-relay` and
`ghcr.io/satbooth/satbooth-tools`, for `linux/amd64` and `linux/arm64`, each as one multi-platform image tagged with the
release version. The bundles do not pull by tag. Each pins `tag@sha256:<digest>` at three sites: the service `satbooth`
of `deploy/public/compose.yml`, the service `relay` of `deploy/hub/compose.yml` and the `tools_image=` line of
`deploy/setup.sh`. A tag in a registry can be moved; a digest cannot name other content. `scripts/release-pins.sh show`
prints the three pinned refs of the checkout you are in, and `scripts/release-pins.sh check-format` checks that all
three are well formed and carry one version.

### Verify the images

You need no trust in the registry: the pinned digest is in the tag's tree, which you verified with `git tag -v`. Two
checks go further.

By attestation. The release workflow attests each image (a signed build provenance, stored beside the image in the
registry). For each ref printed by `scripts/release-pins.sh show`, remove the `:<tag>` part so that it reads
`<name>@sha256:<digest>`. This accepts only the release workflow of this repository, run from this tag on a
GitHub-hosted runner:

```sh
gh attestation verify oci://ghcr.io/satbooth/<name>@sha256:<digest> -R SatBooth/SatBooth \
  --signer-workflow SatBooth/SatBooth/.github/workflows/release.yml \
  --source-ref refs/tags/<tag> --deny-self-hosted-runners
```

The attestation proves where an image was built, not who authorised the release: the tag signature is that check.

By rebuild, which needs Docker with buildx and `jq`. The build is reproducible: the same sources give the same digests
on any machine. It writes large files, so give it a directory outside the checkout. Build and compare with the pins
(the second command prints `match` or `pinned X, built Y` for each image):

```sh
out=$(mktemp -d)
scripts/release-build.sh "$out"
scripts/release-pins.sh check "$out/digests.txt"
```

A rebuild checks the hosted runners that made the release. It runs the same pinned BuildKit and Go images, so it does
not check them: they are trusted by the release build and by every rebuild alike. The release notes of each release
print these commands with the refs filled in, but the signed tag's tree is authoritative, not the notes.

### Build from source

To run images you built yourself, not the release images, build them from this checkout. For the setup command, set
`SATBOOTH_BUILD=1` in the environment of `deploy/setup.sh`: it builds the tools image on every run instead of pulling
it. For each bundle, add that host's `compose.build.yml` after `compose.yml`:

```sh
docker compose -f deploy/public/compose.yml -f deploy/public/compose.build.yml --env-file <your env file> up -d --build
docker compose -f deploy/hub/compose.yml -f deploy/hub/compose.build.yml --env-file <your env file> up -d --build
```

The built images are named `satbooth:local` and `satbooth-relay:local`, and the build needs Docker to pull the pinned
Go builder. The override replaces the release image of every service that runs one: the public bundle's one-off service
too. The qualification suite runs the bundles this way.

### Files in each image

| Image | File | Reason |
|---|---|---|
| `satbooth` | `/satbooth` | The binary, built with `CGO_ENABLED=0 go build -trimpath -ldflags=-buildid=` from the vendored modules. |
| `satbooth` | `/etc/ssl/certs/ca-certificates.crt` | The CA bundle, copied from the pinned Go image (the `certs` stage, which always runs for `linux/amd64`, so the bytes do not depend on the build host). SatBooth makes outbound TLS connections: rate providers, webhooks, `wss://` relays and an `https://` bitcoind or wallet-rpc. |
| `satbooth-relay` | `/satbooth-relay` | The binary, built the same way. |
| `satbooth-tools` | `/satbooth-setup` | The setup command, built the same way. It has no `ENTRYPOINT`: `deploy/setup.sh` names the command. |
| `satbooth-tools` | `/satbooth-pair` | The pairing tool, built the same way. No CA bundle: neither tool uses the network. |

Directories that only hold these files (`/etc`, `/etc/ssl`, `/etc/ssl/certs`) are allowed. There is no tzdata (the
code never calls `time.LoadLocation`), no shell, no package manager and no other file.

### Pins

| Pin | Version | Why it is current |
|---|---|---|
| `golang:1.27.4-trixie@sha256:b54b2639...` (the `build` and `certs` stages) | Go 1.27.4 on Debian 13 | The newest Go 1.27 patch release at the time of writing; the regtest setup runs these two images and uses the same pin for its `volume-init` helper (the bitcoind and Monero helpers use `debian:13-slim`). Nothing from these stages ships except the binaries and the CA bundle, which the `certs` stage takes from the `linux/amd64` image without running anything. |
| `moby/buildkit:v0.34.1@sha256:99368f3e...` | BuildKit v0.34.1 | The builder of `scripts/release-lib.sh`, Docker's own BuildKit. It is pinned because layer compression and the exporter decide the release image digests. It is trusted to build, not for what operators run; a rebuild runs the same pinned image, so it does not check it. |
| `actions/checkout@3d3c42e5...` | v7.0.1 | The pin `check.yml` and `release.yml` use. |
| `actions/upload-artifact@cf430e03...`, `actions/download-artifact@9000827c...` | v7.0.2, v8.0.2 | `release.yml` moves each runner's digests to the job that compares them (`upload-artifact` is also used by `qualify.yml`). |
| `actions/attest-build-provenance@4d101475...` | v4.2.2 | `release.yml` attests each pushed image; the action wraps `actions/attest@508db95d...` (v4.2.1). |
| `ghcr.io/getalby/hub:v1.24.6@sha256:4bb27770...` (hub bundle) | Alby Hub v1.24.6 | The version qualified against (`qualify/REPORT.md`); the same pin as the regtest hubs. |
| `debian:13-slim@sha256:a72b4433...` (hub bundle) | Debian 13 | The base of the bitcoind and Monero images (decision 5), the `netns` holder and the hub bundle's `volume-init`; the same pin as the regtest helpers. |
| Bitcoin Core 32.1, Monero v0.18.5.4 (hub bundle) | release tarballs, SHA-256 pinned | The versions qualified against; the hashes are the regtest images'. |
| Fulcrum 2.1.4 (hub bundle) | release tarballs, SHA-256 pinned | The newest release (2026-10-18); the hashes are from its PGP-signed `shasums.txt`, see `docs/dependencies.md` §8. |

The public bundle's `volume-init` uses the Go pin above: its `chown` and `chmod` need a shell, which the release images
do not have, and this pinned image is the one the release build already uses, so the public host pulls one base image
besides `satbooth`.

The CA bundle is refreshed only when the Go digest is bumped. The digest is the full `sha256:` value in the Dockerfile;
update the tag and the digest together.

### Build and check

To build one image and run the checks that CI runs on each:

```sh
docker build -f deploy/image/Dockerfile --target satbooth -t satbooth:local .
docker build -f deploy/image/Dockerfile --target satbooth-relay -t satbooth-relay:local .
docker build -f deploy/image/Dockerfile --target satbooth-tools -t satbooth-tools:local .
scripts/image-check.sh satbooth:local /satbooth /etc/ssl/certs/ca-certificates.crt
scripts/image-check.sh satbooth-relay:local /satbooth-relay
scripts/image-check.sh satbooth-tools:local /satbooth-setup /satbooth-pair
scripts/image-run-test.sh satbooth:local satbooth-relay:local satbooth-tools:local
```

CI runs these checks on every pull request and every push to `main` (`image.yml`), with no registry login. Only the
release workflow (`.github/workflows/release.yml`, `docs/releasing.md`) pushes to a registry, and only for a release
tag.

## Per-host bundles

| Host | Bundle | Runs |
|---|---|---|
| hub (private) | `deploy/hub/compose.yml` | Alby Hub, `satbooth-relay`, bitcoind, Fulcrum (the hub's Electrum server), monerod, monero-wallet-rpc |
| public | `deploy/public/compose.yml` | `satbooth` |

Each bundle takes its operator values from an env file, passed with `--env-file`. The env files hold addresses and
paths only, never a secret. `satbooth-setup` writes the env file as `<dir>/compose.env` (stage 2 on the hub host,
stage 4 on the public host); written by hand, it is a copy of `example.env` with every value changed, kept outside the
repository. The sections below are the reference for every file and value, and say which stage writes each.

**The private link is the operator's job.** SatBooth on the public host reaches the hub host's relay (7448), bitcoind's
RPC (8332) and wallet-rpc (18083) over a private link: WireGuard, or a private network with a firewall that admits only
the public host to those ports. Setting that up is not this repository's job. The link carries only these three: they
bind the hub host's private address (`HUB_PRIVATE_ADDR`), so nothing listens on its public interfaces even before the
firewall.

**The hub's admin UI never goes over the link.** It controls the node's funds (channels, payments, new NWC
connections), and a fresh hub belongs to whoever opens its setup page first, so the public host, the most exposed
machine, must not reach it. It is published on `HUB_ADMIN_ADDR`, loopback in `example.env`, and the operator reaches it
through an SSH tunnel to the hub host:

```sh
ssh -L 8080:127.0.0.1:8080 <hub host>   # then open http://127.0.0.1:8080
```

**Published addresses are required values.** Every published port is written `"${ADDR:?…}:<port>:<port>"`: an empty
or missing address is an error, because Compose would read an empty host as every interface. `scripts/check.sh` runs
`docker compose config -q` on both bundles with `example.env`, and checks that an empty address is refused.

### Published ports

| Host | Address | Port | For |
|---|---|---|---|
| hub | `HUB_ADMIN_ADDR` (loopback) | 8080 | the hub's web UI and API: the operator's admin access (setup, unlock, channels, pairing, backups), through an SSH tunnel |
| hub | `HUB_PRIVATE_ADDR` | 7448 | the relay's client listener: SatBooth's NWC connections (NIP-42 AUTH, plain `ws://`) |
| hub | `HUB_PRIVATE_ADDR` | 8332 | bitcoind's RPC: SatBooth's on-chain rail, as its whitelisted user |
| hub | `HUB_PRIVATE_ADDR` | 18083 | wallet-rpc (`monero` profile): SatBooth's Monero rail, with the Digest login |
| public | `PUBLIC_ADDR` | `PUBLIC_PORT` | the checkout page (SatBooth's public listener) |

Nothing else is published. The relay's hub listener stays on loopback, monerod's RPC and every peer-to-peer port stay
inside the hub bundle (bitcoind, monerod and the hub's Lightning node connect out; none takes inbound peers), and the
merchant API is not published (see "The merchant API").

### The bind-mount rule

Configuration and secrets are host files, bind-mounted **read-only**: no secret goes in an `environment:` value, an
`env_file:`, the env file or argv. Compose ignores a `secrets:` entry's `uid`, `gid` and `mode` for a `file:` source
outside Swarm (`docs/running.md`, "Secrets in Docker Compose"), so the bundles do not use `secrets:`.

- Keep each secret file mode 0600. Each bundle's `volume-init` (a one-shot container, root with only `CHOWN`,
  `FOWNER` and `DAC_READ_SEARCH`, no network) gives the files to the user that reads them, and the directories mode
  0700, before anything else starts. `DAC_READ_SEARCH` lets it read through a directory it no longer owns: on the next
  `up` its own 0700 directories belong to another user (it grants no write). It never changes a file's mode: SatBooth's loaders, and the wallet-rpc entrypoint, still refuse a file
  with group or other bits, so a mistake shows instead of being repaired.
- On Linux a bind mount keeps the host's numeric owner and mode. Docker Desktop presents a shared file as owned by
  the user that reads it, with its host mode (observed on Docker Desktop with Engine 29.9.2: a 0600 file read as
  65532 was accepted by SatBooth, a 0644 one refused, `qualify/REPORT.md` "Deployment bundles"). The same files work
  on both.

### The hub host

**The first start sets the hub's password before anything else runs.** A fresh hub belongs to whoever opens its setup
page first. So the first `up` starts only the hub and what it needs (`volume-init`, `netns` and bitcoind), and the
first act after it is to set the unlock password over the SSH tunnel; only then does the rest start:

```sh
docker compose -f deploy/hub/compose.yml --env-file <your env file> up -d --build hub
ssh -L 8080:127.0.0.1:8080 <hub host>   # open http://127.0.0.1:8080 and set the unlock password now
docker compose -f deploy/hub/compose.yml --env-file <your env file> up -d --build
# or, with Monero on this host:
docker compose -f deploy/hub/compose.yml --env-file <your env file> --profile monero up -d --build
```

Later starts are the last command alone. (`satbooth-setup hub init` prints these commands with its own env file, and
`hub pair` prints the second start.) Until `relay.json` holds a real pair (step 5 below), the relay exits with
code 2 on every start (a configuration without a pair is refused) and `unless-stopped` restarts it in a loop; that is
expected before pairing, and it stops once the pairs are in and the relay is restarted. With the setup command the relay
is not started until stage 3 has written `relay.json`, so the loop does not arise.

**What can reach the hub's API.** The hub's HTTP listener binds every address of its namespace (getAlby/hub v1.24.6
`cmd/http/main.go:59`, `":"` and the port, with no setting for the address). So besides the tunnel, bitcoind (on the
`hub` network) and Fulcrum (on the `electrum` network) can connect to its API on 8080. That API still needs the
unlock password or a token, and the hub limits unlock attempts; nothing else of the hub is reachable from them, and the
relay's client listener binds `HUB_IP` only. A compromised bitcoind or Fulcrum can therefore try the API, and on a hub
that has not been set up yet, take it: hence the order above.

monerod and wallet-rpc are in the `monero` profile: Monero is optional, and they may run on a third private host
instead (`docs/monero.md`, "Setting up monerod and monero-wallet-rpc"). Without the profile, `monerod.conf`,
`wallet-rpc/` and the wallet are not needed (`MONERO_WALLET_DIR` must still name a directory, which may be empty). With
it, a missing configuration file stops the start with "bind source path does not exist": the bundle never lets Docker
create a missing file's path as a directory.

1. **Values** (`deploy/hub/example.env`; `satbooth-setup hub init` writes them to `compose.env` from its answers:
   `HUB_PRIVATE_ADDR` from `hub_private_addr`, `HUB_NETWORK` from `network` (`mainnet` is `bitcoin`), the three
   Docker values from their answers, and the two directories from the base directory, as `<dir>/config` and
   `<dir>/wallet`):

   | Value | Meaning |
   |---|---|
   | `HUB_PRIVATE_ADDR` | the hub host's address on the private link; the relay, bitcoind's RPC and (`monero` profile) wallet-rpc bind it |
   | `HUB_ADMIN_ADDR` | where the hub's admin UI is published: `127.0.0.1`, reached through an SSH tunnel; never `HUB_PRIVATE_ADDR` or a public address |
   | `HUB_SUBNET`, `HUB_IP` | the bundle's Docker network and the fixed address of the hub and the relay on it; `client_listen` in `relay.json` is `HUB_IP:7448` |
   | `ELECTRUM_SUBNET` | the bundle's internal network of the hub, bitcoind and Fulcrum (no route out) |
   | `HUB_NETWORK` | the hub's chain: `bitcoin` or `signet` (the hub maps `testnet` to signet, getAlby/hub v1.24.6 `lnclient/ldk/ldk.go:206-213`) |
   | `HUB_CONFIG_DIR` | host directory with `relay.json`, `bitcoin.conf`, `hub.env`, and with the `monero` profile `monerod.conf` and `wallet-rpc/` |
   | `MONERO_WALLET_DIR` | host directory with the view-only wallet `satbooth-view` and `satbooth-view.keys` (any directory without the `monero` profile) |

2. **Files in `HUB_CONFIG_DIR`.** `deploy/hub/config-example/` has examples of the three without secrets. Stage 2 of
   `satbooth-setup` writes `bitcoin.conf`, `hub.env` and `fulcrum.conf` (with the `rpcauth` lines and `rpcwhitelist`
   lines described below, byte for byte those of the example) and, when the public host's handoff says Monero is on,
   `monerod.conf` and `wallet-rpc/` (`rpc-login`, `wallet.pass`); stage 3 writes `relay.json`.

   | File | Read by | Holds |
   |---|---|---|
   | `relay.json` | the relay (65532) | the relay's configuration (`docs/running.md`, "Running the relay"): `hub_listen` `127.0.0.1:7447`, `client_listen` `HUB_IP:7448`, `client_relay_url` `ws://HUB_PRIVATE_ADDR:7448`, which must equal SatBooth's `nwc.relay_url` byte for byte, and the pairs. No secret. |
   | `bitcoin.conf` | bitcoind (10001) | `txindex=1` (Fulcrum needs it, and an unpruned node), `rpcbind=0.0.0.0`, `rpcport=8332` on every chain, `rpcallowip` for `HUB_SUBNET`, `ELECTRUM_SUBNET` and the public host's private address only, `rpcauth` lines for the users `hub` and `fulcrum`, each confined by `rpcwhitelist`: the hub's user to the 11 calls its chain source makes (the chain tip, headers and blocks, the mempool and its transactions, fee estimates, the broadcast; `docs/dependencies.md` §8), Fulcrum's to the 15 it makes (`config-example/bitcoin.conf`), SatBooth's user to its 12 (`docs/bitcoind.md`, "Setting up bitcoind"), and `rpcwhitelistdefault=0`, so `bitcoin-cli` with the cookie inside the container stays unrestricted. With `LDK_GOSSIP_SOURCE` left empty (below) the hub checks the channels it learns over gossip against the chain, which needs `getblockhash` and `gettxout`: without them every such check fails and the hub drops the announcement (lightning-block-sync 0.2.0 `src/gossip.rs`), so it learns no channels beyond its own to route through. |
   | `monerod.conf` (`monero` profile) | monerod (10002) | monerod's options; its RPC on `0.0.0.0:18081` with `confirm-external-bind`, for wallet-rpc on the `monero` network only (`--restricted-rpc` is added by `compose.yml`). |
   | `hub.env` | the hub (10003) | **secret, 0600:** `LDK_BITCOIND_RPC_PASSWORD=<the hub user's password>`. |
   | `fulcrum.conf` | Fulcrum (10004) | **secret, 0600:** `rpcuser = fulcrum`, `rpcpassword = <its password>`, `peering = false`, `announce = false`. |
   | `wallet-rpc/rpc-login` (`monero` profile) | wallet-rpc (10002) | **secret, 0600:** one line `<user>:<password>`, SatBooth's `monero.rpc_user` and the password in its `monero.rpc_password_file`. |
   | `wallet-rpc/wallet.pass` (`monero` profile) | wallet-rpc (10002) | **secret, 0600:** the view-only wallet's password. |

3. **bitcoind is dedicated to the hub and SatBooth**, with no wallet that holds keys loaded on it (`docs/bitcoind.md`,
   "Setting up bitcoind"): SatBooth's user is confined by `rpcwhitelist`, but the whitelist is per method, not per
   wallet. Make the watch-only wallet and import the descriptor with `docker compose … exec bitcoind bitcoin-cli
   -conf=/etc/bitcoin/bitcoin.conf -datadir=/data …` (inside the container `bitcoin-cli` uses bitcoind's cookie file, so
   no password is typed). Stage 2 prints the three commands (`createwallet`, `getdescriptorinfo`,
   `importdescriptors`); they are the operator's, since the descriptor is never in the setup command's output.
4. **The view-only wallet** is made by the operator (`docs/monero.md`, "Setting up monerod and monero-wallet-rpc"), with
   the bundle's own `monero-wallet-cli`, once monerod runs:

   ```sh
   docker compose -f deploy/hub/compose.yml --env-file <your env file> --profile monero run --rm --no-deps \
     --entrypoint monero-wallet-cli wallet-rpc --generate-from-view-key /wallet/satbooth-view \
     --password-file /etc/wallet-rpc/wallet.pass --restore-height <height> --daemon-address monerod:18081 \
     --log-file /tmp/wallet-cli.log
   ```

   It asks for the primary address and the private view key, and takes the new wallet's password from
   `wallet-rpc/wallet.pass` (`/etc/wallet-rpc/wallet.pass` in the container).
   wallet-rpc's entrypoint (`deploy/hub/monero/wallet-rpc.sh`) refuses to start until the
   wallet and both secret files exist with mode 0600. It reads the login with the shell's own `read` and gives it to
   wallet-rpc as `RPC_LOGIN` in its environment (monero v0.18.5.4 `src/rpc/rpc_args.cpp:226-228`), never in argv.
   It passes `--trusted-daemon`, since monerod is the bundle's own node.
5. **Set up the hub through the SSH tunnel** (`ssh -L 8080:127.0.0.1:8080 <hub host>`, then `http://127.0.0.1:8080`),
   as the first act after the first `up` (above) and before the public host is connected to the private link: the
   first visitor of a fresh hub sets its unlock password. Unlock it the same way after each hub start. Then pair
   SatBooth (`docs/running.md`, "Pairing with the hub", with `-hub
   http://127.0.0.1:8080` over the tunnel) and add the pairs to `relay.json`; restart the relay to load them
   (`docker compose … restart relay`). With the setup command, stage 1 prints the links, you copy the Wallet Public
   Keys from the hub's page, and stage 3 writes the pairs.

**The hub's environment.** The hub service starts from the regtest one (`dev/regtest/compose.yml`, `hub-satbooth`).
Each key is fixed in `compose.yml`, an operator value, a secret in `hub.env`, or left to the hub's default:

| Key | Regtest | Bundle |
|---|---|---|
| `WORK_DIR` | `/data` | fixed: `/data` (the `hub-data` volume) |
| `PORT` | `8080` | fixed: `8080` |
| `NETWORK` | `regtest` | operator: `HUB_NETWORK` |
| `LOG_LEVEL` | `4` | fixed: `4` |
| `LDK_BITCOIND_RPC_HOST` | `172.30.78.2` | fixed: `bitcoind` (the bundle's service) |
| `LDK_BITCOIND_RPC_PORT` | `18443` | fixed: `8332` (`bitcoin.conf` sets `rpcport=8332` on every chain) |
| `LDK_BITCOIND_RPC_USER` | `hub`, whitelisted as in the bundle | fixed: `hub` |
| `LDK_BITCOIND_RPC_PASSWORD` | a test string | secret: `hub.env` |
| `LDK_GOSSIP_SOURCE` | empty | left out: unset and empty are the same, gossip over the peer-to-peer network (`lnclient/ldk/ldk.go:253`), whose channel announcements the hub checks with `getblockhash` and `gettxout` (both in the hub user's `rpcwhitelist`) |
| `SEND_EVENTS_TO_ALBY` | `false` | fixed: `false` |
| `AUTO_LINK_ALBY_ACCOUNT` | `false` | fixed: `false` |
| `MEMPOOL_API` | unreachable, on purpose | left out: the hub's default (`https://mempool.space/api`); set it in `hub.env` to use your own |
| `LDK_ELECTRUM_SERVER` | not set | fixed: `tcp://fulcrum:50001`, the bundle's own Electrum server (below); unset, the hub would use Alby's (`ssl://electrum.getalbypro.com:50002`) |
| `LDK_ESPLORA_SERVER` | not set | left out: set, it would take the address lookups away from Fulcrum (`chain/address_lookup.go:20-26`) |
| `RELAY` | `ws://127.0.0.1:7447` | fixed: `ws://127.0.0.1:7447` (the relay's hub listener) |
| `AUTO_UNLOCK_PASSWORD` | a test string | left out: the operator unlocks the hub in its UI after each hub start |

The hub loads `hub.env` because it reads `.env` from its working directory (`service/service.go:56` in getAlby/hub
v1.24.6, `godotenv.Load(".env")`, which never overrides a variable that is already set), and the bundle mounts the file
there. Without `AUTO_UNLOCK_PASSWORD`, a restarted hub is locked until the operator unlocks it, and SatBooth cannot
start (or reach the wallet) until then; adding it to `hub.env` makes the hub unlock itself, at the cost of keeping its
unlock password on the host.

**Why a `netns` container.** The relay's hub listener must be on loopback (it needs no login), so the hub and the relay
share one network namespace. A container that joins another's namespace loses its network when that container
restarts, and gets it back when it restarts itself (observed on Engine 29.9.2: after the owner restarted, the joiner
had no address). So neither the hub nor the relay owns it: `netns` (the pinned `debian:13-slim`, running `sleep` as
65534, read-only, no capabilities) holds the namespace and the two published ports of the hub and the relay, and both
join it, as a Kubernetes pod's `pause` container does.

**Three networks.** `hub` holds `netns` (the hub and the relay) and bitcoind; `electrum`, internal (no route out),
holds `netns`, bitcoind and Fulcrum; `monero` holds only monerod and wallet-rpc. Observed on the bundle (regtest): from
the hub's namespace, bitcoind and Fulcrum answer and monerod's name does not resolve, nor does its address answer;
Fulcrum reaches bitcoind and nothing outside.
monerod's RPC has no login, and whoever can call it unrestricted can point it at a chain of their own making
(`set_bootstrap_daemon`), which wallet-rpc would then report as payments. So no container but wallet-rpc reaches it,
and the bundle starts monerod with `--restricted-rpc` as well: that keeps every call the wallet makes (monero
v0.18.5.4 `src/rpc/core_rpc_server.h:113-189`, `src/wallet/wallet2.cpp:3177`), and hides only pool transactions still
in their Dandelion++ stem until they are relayed (`core_rpc_server.cpp:651-654`).

**The Electrum server.** With bitcoind as its chain source, the hub's Lightning node (LDK) follows the chain, channel
closes and breaches included, from bitcoind alone (`lnclient/ldk/ldk.go:217-232`, `SetChainSourceBitcoindRpc` at
`:226`, in getAlby/hub v1.24.6). Only the hub's own on-chain address lookups, whether an address has been used (its
address API and swaps, `api/address_lookup.go:9-16`, `swaps/swaps_service.go:1598-1612`), need an address index, which
bitcoind lacks; they go to `LDK_ELECTRUM_SERVER` (`chain/address_lookup.go:20-26`, Electrum protocol over `tcp://` or
`ssl://`, `chain/electrum.go`). The bundle runs Fulcrum for them, so they stay on the host. Fulcrum needs bitcoind with
`txindex=1` and unpruned, its own index on disk (its README gives 133 GB for Bitcoin as of August 2023, more since) and
at least 1 GB of RAM; it builds the index on first start, which takes hours to days. Until then the hub's lookups
fail, and nothing of SatBooth's depends on them.

**The node images** (`deploy/hub/bitcoind/Dockerfile`, `deploy/hub/monero/Dockerfile`, `deploy/hub/fulcrum/Dockerfile`)
download the official release tarballs of Bitcoin Core 32.1 and Monero v0.18.5.4 (the qualified versions) and Fulcrum
2.1.4, check them against pinned SHA-256 hashes, and copy the binaries onto `debian:13-slim` pinned by digest (decision
5). The download tools stay in a build stage. bitcoind runs as 10001, monerod and wallet-rpc as 10002, the hub
as 10003, Fulcrum as 10004 and the relay as 65532, each with a read-only root, no capabilities and `no-new-privileges`. No configuration, password or
regtest setting is in an image.

### The public host

`satbooth-setup public finish` prints the start command below with the env file it wrote, and writes `satbooth.json`
(stage 4) with the values described here, from the answers and the hub's Wallet Public Keys.

```sh
docker compose -f deploy/public/compose.yml --env-file <your env file> up -d --build
```

| Value | Meaning |
|---|---|
| `PUBLIC_ADDR`, `PUBLIC_PORT` | where the checkout page is published: `127.0.0.1` behind a TLS reverse proxy on the host that serves `checkout_base_url`, or the host's public address |
| `SATBOOTH_SUBNET`, `SATBOOTH_IP` | the bundle's Docker network and SatBooth's fixed address on it |
| `SATBOOTH_ETC` | host directory with `satbooth.json` and its secret files, mounted read-only at `/etc/satbooth`; `satbooth-setup` writes the secret files and `api-keys.json` at stage 1 and `satbooth.json` at stage 4 |
| `SATBOOTH_BACKUPS` | host directory for backups, mounted at `/backups` in `satbooth-oneoff` only |

`satbooth.json` (`docs/configuration.md`, "Configuration") names its files under `/etc/satbooth/`, sets `data_dir` to
`/data` (the `satbooth-data` volume), `public_listen` to `SATBOOTH_IP:8080` and `private_listen` to `SATBOOTH_IP:8081`:
SatBooth refuses a wildcard listener, so it listens on its fixed address in the bundle's network, and the bundle
publishes port 8080 only. `nwc.relay_url`, `onchain.rpc_url` and `monero.wallet_rpc_url` point at `HUB_PRIVATE_ADDR`
(ports 7448, 8332 and 18083). The service runs as it ships: user 65532, read-only root, no capabilities,
`no-new-privileges`, `memlock: -1`, and a 60 s stop grace (the worst-case shutdown with alerts on is 40 s).

### The merchant API

The private listener is not published. The merchant's application reaches it at `http://SATBOOTH_IP:8081` from the
public host itself (Linux routes the host to its bridge networks), or from a container attached to the bundle's
network (`satbooth-public_satbooth`). A merchant application on another host needs a way in that this bundle does not
open: publishing port 8081 on the private link's address is the operator's decision, never on a public address.

### Backups

`satbooth backup` runs in a one-off container of the image, without a network, with the service stopped
(`docs/backup-and-restore.md`, "Backup and restore"):

```sh
docker compose -f deploy/public/compose.yml --env-file <your env file> stop satbooth
docker compose -f deploy/public/compose.yml --env-file <your env file> --profile oneoff run --rm satbooth-oneoff \
  backup -config /etc/satbooth/satbooth.json -to /backups/<name>
docker compose -f deploy/public/compose.yml --env-file <your env file> start satbooth
```

The backup is `SATBOOTH_BACKUPS/<name>`: a directory of the image's user 65532, mode 0700, whose files are 0600, so
copying it off the host needs root. A directory without `manifest.json` is not a backup.

### Restore

The data directory is the named volume `satbooth-data`, and the image has no shell, `rm` or `cp`. The restore runs in
the bundle's `volume-init` service instead (the pinned builder image, which has `sh`, `cp` and `sha256sum`), as the
image's user 65532 and without a network. `scripts/qualify.sh` runs exactly these commands in its public-bundle group
(backup, restore, start, the same orders).

```sh
dc() { docker compose -f deploy/public/compose.yml --env-file <your env file> "$@"; }

# 1. Stop SatBooth (unless-stopped does not restart a manual stop).
dc stop satbooth

# Only on a new host or a new volume: create the volume and give it, and /backups, to 65532.
dc run --rm --no-deps volume-init

# 2 and 3. As 65532, without a network: check both files of the ONE backup <name> against its manifest, remove the
# live databases with their -wal and -shm files, copy both files, and check the copies.
dc run --rm --no-deps -T --user 65532:65532 --entrypoint sh volume-init -c '
  set -eu
  b=/backups/$1
  check() { # <file>: the manifest entry named after the file holds its SHA-256
    h=$(sha256sum "$1" | cut -d" " -f1)
    grep -q "{\"name\":\"$(basename "$1")\",\"bytes\":[0-9]*,\"sha256\":\"$h\"}" "$b/manifest.json"
  }
  check "$b/store.db"
  check "$b/ledger.db"
  rm -f /data/store.db* /data/ledger.db*
  cp -p "$b/store.db" "$b/ledger.db" /data/
  check /data/store.db
  check /data/ledger.db
  ls -ln /data' sh <name>
```

**With refunds on (a `payout` block in `satbooth.json`), do not run the next block yet:** replace the approval keys
first (below). Without a `payout` block, run it now.

```sh
# 4. Start. (start runs the existing container. On a new host, where the bundle never ran, the container does not
# exist yet: dc up -d satbooth creates and starts it instead. qualify runs the existing-host path.)
dc start satbooth
```

- `satbooth.lock` stays; only `store.db*` and `ledger.db*` go. The `-wal` and `-shm` files must go with them: after a
  crash a left-over `-wal` belongs to the old database.
- A backup directory copied in from elsewhere must be the image user's first: as root on the host,
  `chown -R 65532:65532` it inside `SATBOOTH_BACKUPS`.
- The command stops at the first file that does not match the manifest, before anything is removed if the backup
  itself does not match.
- The start then logs no `startup failed` with reason `pair`. If it does, the two files are not from one backup:
  restore both from one backup again. Backups made after `<name>` belong to a timeline the restore gave up; the pair
  stamp refuses a file from one of them next to a file of the restored pair.

**With refunds on: one rule, before the first start after the restore.** The restore puts the store back to the
backup, and "already claimed" is recorded only there: an approval signed before the restore, for a refund claimed
after the backup, could be claimed again with a new invoice (`docs/backup-and-restore.md`, "Backup and restore" has the
reasons and the reconciliation; `docs/threat-model.md` §4, "Restoring a backup while refunds are in use"). So, after the
restore command and before the start block:

1. On the admin system make a new approval key with `satbooth-approve keygen -out <a new path>` (it refuses an
   existing path and prints the public key). The private key stays with the admin and signs no refund approval yet;
   destroy the old signing keys.
2. As root on the host, edit `<SATBOOTH_ETC>/satbooth.json` (it is mounted read-only into the container, and
   `satbooth-setup` wrote it mode 0600 for user 65532: keep its owner and mode). Replace `payout.approval_keys` with
   the new public keys only, and keep the `payout` block. (`approval_keys` in the setup answers is for a new
   set-up; for a restore, edit the file.) A start without the block is not an option: it is refused while the
   restored store holds a refund with no result.
3. Run the start block above. An approval signed with an old key is refused at claim (`invalid_approval`). A refund
   the restored store holds `claimed` ends `failed` before anything is sent (`key_revoked`, or `expired_unsent` when its
   approval has expired). A refund it holds `sent` is looked up at the hub: `settled` or `failed` ends it. `pending`
   leaves it open (an `ALERT` once an hour has passed since the send) and a void is refused until the hub settles or
   fails the payment. No record, or `expired`, logs an `ALERT` and leaves it open until a void approval, signed with a
   new key, ends it (accepted from 11 minutes after the send). An answer that does not fit the refund (or a malformed
   one) logs an `ALERT`, leaves it open and makes a void answer `hub_unavailable`: a person investigates
   (`docs/refunds.md`, "Voiding a refund"). The hub unreachable: the refund stays open and is looked up again; a void
   answers `hub_unavailable`.
4. **Approve no refund with the new key until the payouts made after the backup are reconciled** (a void approval
   moves no money and may be signed at any time). Before reconciling, wait at least 11 minutes after the old SatBooth
   stopped or its host went down, whichever is later (when unsure, count from the time you are sure). The hub does not
   read a request's expiration: only SatBooth's relay, on the hub host and by that host's clock, stops serving a pay
   request past its send time plus `payout.timeout_seconds` (10 to 600). The 11 minutes are the longest timeout, 10
   minutes, plus a minute for the difference between the two hosts' clocks; both hosts must keep time with NTP, and
   neither clock may have been stepped by hand. Otherwise a request sent before the restore can still reach the hub,
   and it and the replacement refund would both pay. A payout the hub still shows `pending` counts as paid until the
   hub shows it `failed`: reconcile again when it has resolved, and approve nothing for that order before. The wait
   does not cover a hub that was stopped or cut off while it held such a request: it can still pay it when it resumes,
   and the list shows nothing before. Before approving anything for an order with a refund in the hub's list that the
   restored store does not know, reconcile again. The backup's time is
   `created_at` (Unix seconds) in its `manifest.json`, in `SATBOOTH_BACKUPS/<name>`. In the hub, list the payments of
   the payout connection made at or after that time, and match each to the claim that carried its invoice, from the
   admin system's or the merchant app's own record (the
   refund view never carries the invoice). For a refund the restored store does not know, or shows `failed` or open
   although the hub paid it, count it as paid in every later approval for that order. The store and the ledger never
   learn such a payout and nobody corrects them by hand: the order's `refunded_msat` stays too low for good.
5. Only then may the admin system sign refund approvals with the new key. Never put the old keys back: they would
   make every approval signed before the restore valid again.

Orders, invoices and payments recorded after the backup are gone from the restored store: a payment to one of them is
not recorded and not alerted, and the merchant handles it by hand (`docs/backup-and-restore.md`, "Backup and restore"
says where to look). A payment after the backup to an invoice, address or subaddress that the backup holds is found
again, and one to something issued after the backup is not, even when its order is in the backup: look for it as for a
lost order. On-chain and Monero a payment that is found again is first seen at the restore, so inside the order's 7-day
watch window after its expiry it is recorded late (`order.late` and an `ALERT`), and past the window only an `ALERT`
names its order, although `order.paid` may already have been sent: compare with the webhooks received since
`created_at`, and credit or refund by hand. The addresses and subaddresses issued after the backup are not issued again
while bitcoind's wallet and the Monero wallet keep their state. If either is rebuilt as well, its next index must be set
above everything issued since the backup (`docs/bitcoind.md`, "Setting up bitcoind", step 6, and `docs/monero.md`,
"Setting up monerod and monero-wallet-rpc", start-up check 5): an address shown to the customer of an order the restore
lost must never go to a new order, because a late payment to it would mark that order paid.

### Restarts

Every long-running service has `restart: unless-stopped`: Docker restarts it when it exits, and after the daemon or the
host restarts, but not after a manual stop. `on-failure` would also restart a failed start, but "doesn't restart the
container if the daemon restarts" (Docker docs, `engine/containers/start-containers-automatically.md`): at start the
daemon restarts a container only if its policy says so for the last exit code (moby docker-v29.9.2 `daemon/daemon.go:577`
with `daemon/internal/restartmanager/restartmanager.go:81-91`), and SatBooth exits 0 when the daemon stops it, so it
would stay down after a reboot. After a daemon restart, the hub and the relay wait up to 5 s for `netns`, whose
namespace they join, to be started first (`daemon/daemon.go:663-675`, `daemon/container.go:190-198`; read in the
source, not observed).

**SatBooth refuses to start while the hub does not answer** (`docs/running.md`, "A supervisor"), so
while the hub is down Docker restarts it in a loop. Compose sets no delay for that loop outside Swarm: it passes only
the policy's name and maximum retries to the engine, and drops `deploy.restart_policy.delay` and `window`
(docker/compose v5.6.1 `pkg/compose/create.go:696-721`). The engine, for every policy, waits 100 ms before the first
restart and doubles the wait up to 1 minute, and resets it to 100 ms when the container ran 10 s or more
(`daemon/internal/restartmanager/restartmanager.go:12-14`, `:66-78`; Docker docs, `docker container run`, "Restart
policies"). So the loop is as slow as each refused start. Observed on Engine 29.9.2, with a refused SatBooth container
under `--restart on-failure` (the same backoff):

| Case | Each start | Loop |
|---|---|---|
| the hub host does not answer (the connection to the relay times out) | 10 s: the NWC client's bound on its first connection (`nwc/client.go:192-193`) | one start every 10.2 s, about 350 an hour. Each container ran just over 10 s, so the wait went back to 100 ms every time; a few milliseconds less and it would grow to 1 minute |
| the relay refuses the connection at once | under 1 s | 7 starts in the first 10 s, then waits of 13, 26 and 51 s, then one start a minute |
| the relay answers but the hub does not (`get_info` times out) | the lower of `nwc.timeout_seconds` (at least 10) and 30 s | one start per timeout plus 100 ms (not measured) |

A hub whose `get_info` names no network, or another network than `nwc.network`, refuses the start as soon as it has
answered (the loop of a relay that refuses at once). The invoice connection is checked first and the payout connection
only after it passes, so one start names one connection (reason `invoice_connection` or `payout_connection`); the cause
is in the `error` field of the `satbooth failed` line. The causes are `the invoice connection's get_info does not state
the network; pair it with satbooth-pair -connection invoice` (`payout` in both places for the payout connection; the
connection was paired without "Read your node info", whenever it was paired: on the connection's page in the hub,
click Edit Connection, open Custom, tick "Read your node info" and click Save, without picking another permission
group; or pair it again), `the invoice connection's hub is on the network "mainnet", not nwc.network "signet"` (the two
names are printed only when the hub's is one of `mainnet`, `testnet`, `signet` and `regtest`) and `the invoice
connection's hub reports a network SatBooth does not know`. The start does not record the network in the data directory.

Each refused start logs `memory locked; core dumps disabled` and `startup failed` with its reason
(`wallet_timeout`, `wallet_other`, `invoice_connection`, …), and advances the pair stamp's generation in both
databases (harmless: both files advance together). **None of these refusals is an `ALERT:` line**, so none reaches the
alert webhook. Watch the container's restart count (`docker inspect -f '{{.RestartCount}}'`) or its `startup failed`
lines. The one refusal that logs an `ALERT:` line on each start is bitcoind's wallet behind SatBooth's record (`ALERT:
wallet next_index is behind SatBooth's record; addresses are not issued`, `docs/bitcoind.md` "Setting up bitcoind"); it
is queued for the alert webhook, but the alert deliverer starts only once every start-up check has passed, so it is sent
only after a start succeeds.
