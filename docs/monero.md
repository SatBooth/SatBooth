# Setting up monerod and monero-wallet-rpc (Monero)

SatBooth talks to one `monero-wallet-rpc` that holds the merchant's **view-only** wallet, over the private link.
wallet-rpc reads the chain from the operator's own `monerod`. SatBooth never talks to monerod, holds no Monero key, and
its configuration carries only the wallet's primary address. Running monerod and wallet-rpc is the operator's job; the
hub host's Compose bundle runs both with its `monero` profile (`deploy/README.md`, "The hub host"): monerod with
`--restricted-rpc` on a network only wallet-rpc reaches, and wallet-rpc with full RPC, its login read from a 0600 file
into `RPC_LOGIN` by its entrypoint, never argv.
(Each Monero fact below is marked *source*, read in the Monero v0.18.5.4 source, or *observed*, seen in qualification on
regtest, `qualify/REPORT.md` "Monero end to end".)

**Where they run.** monerod and wallet-rpc go on a third private host if you can afford one, else on the hub's private
host. **Never on the public host** that runs SatBooth: whoever holds wallet-rpc's login can fake payments (below).

1. **monerod.** Your own node, synced, on the private network. A pruned node is enough (`--prune-blockchain`).
   wallet-rpc refreshes from it every 20 seconds (*source*: `DEFAULT_AUTO_REFRESH_PERIOD`,
   `src/wallet/wallet_rpc_server.cpp:64`). monerod (not wallet-rpc) refuses `non-interactive` in its config file
   (*observed*: "Option 'non-interactive' is not allowed in the config file"): give it on monerod's command line.
2. **A fresh wallet used only for shop income.** The private view key shows every incoming payment of the whole
   wallet, all its accounts. If the key leaks (a SatBooth or wallet-rpc compromise), all of those payments are
   exposed, not only the shop's. Make a new wallet for the shop, and keep its seed offline as you would any wallet's.
3. **The view-only wallet**, made by the operator from that wallet's primary address and private view key (no spend
   key), with `monero-wallet-cli`:
   - `--generate-from-view-key <wallet file>`, which asks for the address and the view key, with `--restore-height`
     set to the height at which the wallet was made; or
   - `--generate-from-json <file>`: a JSON object with `version` 1, `filename`, `address`, `viewkey` and
     `scan_from_height`. **The new wallet's password is the JSON's `password` member** (empty when absent);
     `--password-file` is not read for it (*source*: `generate_from_json`, `src/wallet/wallet2.cpp:542`, the
     password at `:576`; *observed*). The file then holds the view key and the password: keep it 0600, or delete it once the wallet
     exists.
4. **Do not hand out subaddresses of SatBooth's account from your own wallet.** SatBooth issues the subaddresses of
   `account_index` itself, one per order. The view-only wallet skips the indexes it has seen receive money
   (*derived from observed indexes*: after a payment to indexes 1 and 2 from the merchant's own wallet, a later
   phase's orders got the logged indexes 15 to 17, which, counted back over the orders pressed before them, puts
   SatBooth's first index at 3), but
   an index you hand out that has not been paid yet is issued again to an order, and a payment to it counts for that
   order.
5. **wallet-rpc**, with its login in a config file. Write the file with an editor (not `echo`, so the password does
   not land in shell history), owned by wallet-rpc's user, mode 0600, for example `/etc/monero/wallet-rpc.conf`:

       rpc-login=satbooth:<password>

   and start wallet-rpc with it:

       monero-wallet-rpc --config-file /etc/monero/wallet-rpc.conf --wallet-file <wallet file> \
         --password-file <file> --daemon-address <monerod's private IP>:18081 --rpc-bind-ip <private IP> \
         --rpc-bind-port 18083 --confirm-external-bind --non-interactive

   - **The password is never on a command line.** wallet-rpc reads `rpc-login` from its config file (*source*: the
     config file is parsed into the same options, `src/wallet/wallet_args.cpp:173`, which hold `rpc-login`,
     `src/wallet/wallet_rpc_server.cpp:5093`, `src/rpc/rpc_args.cpp:227`; *observed*: a throwaway v0.18.5.4 wallet-rpc
     with only that file answered 200 to the Digest login and 401 without it or with a wrong password,
     `qualify/REPORT.md`). Under systemd, `EnvironmentFile=` naming a 0600 file that holds `RPC_LOGIN=satbooth:<password>`
     works too: wallet-rpc reads `RPC_LOGIN` when `--rpc-login` is absent (*source*: `rpc_args.cpp:226-228`; regtest
     uses it). Never `--rpc-login` in argv, `RPC_LOGIN=… monero-wallet-rpc` typed in a shell (history, and argv of a
     wrapper such as `sudo`), or `Environment=` in a unit file (readable by every user through `systemctl show`).
     Put the same password in SatBooth's `monero.rpc_password_file`.
   - Bind it to the private link only, and firewall it so that nothing but SatBooth's host reaches it.
   - Add `--trusted-daemon` only for your own, local monerod.
   - Do not use `--disable-rpc-login` (SatBooth refuses a wallet-rpc that asks for no login) or `--restricted-rpc`
     (it denies `create_address`, `get_transfers`, `get_transfer_by_txid` and `store`, *source*:
     `wallet_rpc_server.cpp:761-763`, `:2083-2087`, `:2906-2910`, `:2983-2987`; SatBooth stops with `monero: wallet-rpc
     runs with --restricted-rpc; SatBooth needs full RPC on the private link`).
   - Any other option can go in the same `--config-file`, without the leading dashes (`non-interactive=1` included:
     wallet-rpc accepts it there, unlike monerod; *observed* in the same throwaway run).

**What SatBooth calls.** Exactly these methods, in JSON-RPC 2.0 to `/json_rpc` with HTTP Digest, and its client refuses
any other before sending it: `get_version`, `get_height`, `get_address`, `create_address`, `store`, `get_transfers`,
`get_transfer_by_txid` and `sign_transfer` (only with an empty `unsigned_txset`, as the view-only probe). **wallet-rpc
has no per-method whitelist**, unlike bitcoind's `rpcwhitelist`: whoever holds the login can call any method. They can
point the wallet at a daemon they run (`set_daemon`) and so fake Monero payments to SatBooth, stop or rescan the wallet,
and read the view key (`query_key`). They cannot spend: the wallet has no spend key. Threat model §0 counts the login
as part of a SatBooth compromise, which already writes the ledger; that bound holds only while nothing but SatBooth
reaches wallet-rpc.

**The startup check.** At every start SatBooth checks, in this order:
1. wallet-rpc's version has major 1 (*observed*: v0.18.5.4 answers 1.32; the minor is logged);
2. the wallet's primary address equals `address`, and `account_index` exists in the wallet;
3. the wallet is view-only: `sign_transfer` with an empty set must answer -29, the watch-only code (*source*:
   `wallet_rpc_server.cpp:1382-1387`; *observed*: a full wallet answers -39 and is refused). Nothing is signed and no
   key is asked for;
4. the wallet's height is above 0;
5. the wallet knows SatBooth's highest recorded subaddress index, with the recorded subaddress. A wallet that does not
   (a wallet file restored from a backup, or wallet-rpc killed before it stored a new subaddress) is brought back:
   SatBooth calls `create_address` once per missing index until the wallet reaches the record, checks that the
   subaddress at the recorded index is the recorded one (subaddresses are deterministic: *observed*), stores the
   wallet and logs `ALERT: wallet-rpc had forgotten issued subaddresses; re-created up to the record` with the count.
   **After SatBooth's own store was restored from a backup (see [Backup and restore](backup-and-restore.md)) and the wallet was rebuilt as
   well, this step is not enough:** it brings the wallet up to the restored store's highest index and no further,
   so the next subaddress would be one that an order lost in the restore already showed to its customer, and a
   late payment to it would mark the new order paid. Before the start, make extra subaddresses in the view wallet's
   `account_index` until its last index is above the restored store's highest index (`SELECT max(idx) FROM
   monero_subaddresses` in `store.db`) by a margin generously over the number of Monero orders that could have been
   created since the backup (most Monero orders in a day, times the days since the backup's `created_at`, times ten),
   and keep them: wallet-rpc's `create_address` takes a `count` (1 to 65,536) and must be followed by `store`, or a
   killed wallet-rpc forgets them; or open the view wallet's file in `monero-wallet-cli` while wallet-rpc is stopped
   (wallet-rpc holds the file), select the account, make them with `address mnew <count>` (or `address new`, one at
   a time), and `save`. SatBooth then finds
   the recorded index known and issues above the wallet's last one. Raise the lookahead of the merchant's own
   (spending) wallet above the margin, not of this view wallet (see "The merchant's own wallet" below).
   A mismatch stops the start. The wallet is also stored after every 100 indices created, so a restore of many
   indices (a wallet restored from keys for a shop that issued thousands) that runs past the start's 30-second bound
   keeps its progress: the start fails with `monero: wallet-rpc is unavailable` and step 5, and the next start goes on
   from where it stopped. Such a restore may take several starts.

Any failure stops the start with a fixed cause: `monero: wallet-rpc has no wallet open; start it with --wallet-file`,
`monero: wallet-rpc refused SatBooth's login`, `monero: the wallet has no account account_index`, `monero: the wallet's
primary address is not the configured one`, `monero: the wallet is not view-only`, and so on; for a call that failed,
`monero: wallet-rpc is unavailable` with the step's number. A wallet below SatBooth's recorded height (a restored wallet
still rescanning) passes, with `ALERT: the wallet's height is below SatBooth's cursor; payments are found again as it
catches up`. While running, a pass runs the check again whenever the last one failed or the wallet is behind on
subaddresses; every other pass first asks wallet-rpc for the account's base address and compares it with the one the
last passed check kept, and so does every press that needs a new subaddress, before `create_address`; after
`create_address` the press checks the base address that `get_address` returns with the new subaddress against the same
one, so a wallet swapped between the two calls is caught too. A wallet replaced
while SatBooth runs is caught at the next pass or press, before anything is recorded or shown: the press answers "busy,
try again" and no subaddress of the other wallet reaches a customer. **If the check fails for any other reason than
subaddresses (the wallet replaced, the login changed, wallet-rpc unreachable), or the base address differs, Monero
watching pauses:** each pass logs `ALERT: Monero watching is paused: the wallet failed its startup check` with a
one-word `reason`, and records nothing, marks nothing and settles nothing until a check passes. A wrong wallet would
answer "not found" for every payment and read as every payment gone, so SatBooth waits instead; payments are found once
the right wallet is back (a payment first seen late by SatBooth is `late`, see [Rules for Monero payments](rules-monero.md)). No
subaddress is issued while the check fails (`ALERT: the wallet-rpc check failed; subaddresses are not issued` when it
fails while behind on subaddresses).

**The merchant's own wallet.** SatBooth never reuses a subaddress: an order gets the next index of `account_index` when
the customer first presses "Pay with Monero", and orders never paid leave unused indexes behind. A Monero wallet
restored from its seed looks 200 indexes past the last one it saw used, in 50 accounts (*source*:
`SUBADDRESS_LOOKAHEAD_MINOR` 200 and `SUBADDRESS_LOOKAHEAD_MAJOR` 50, `src/wallet/wallet2.cpp:128-129`), and does not
see payments beyond that. **Raise the lookahead** of the merchant's own wallet above the longest run SatBooth may leave:
in `monero-wallet-cli`, with the wallet open, `set subaddress-lookahead 50:1000` (*source*:
`src/simplewallet/simplewallet.cpp:4045`; the `--subaddress-lookahead` flag cannot be combined with `--wallet-file`,
`:4817`). SatBooth logs `ALERT: unused Monero subaddresses exceed unused_alert; raise the merchant wallet's subaddress
lookahead`, with the count, when the issued indexes above the highest one that received a payment number
`unused_alert` (default 100, half the default lookahead) or more, then once a day while they still do (and again on the
first pass after a restart).

**Versions.** Qualified against monerod and monero-wallet-rpc v0.18.5.4 (the official release binaries, SHA-256 pinned
from the PGP-signed `hashes.txt`). The v0.19 release (the FCMP++ and Carrot hard fork) has not been qualified; monerod
must move to it at the fork, so SatBooth needs a qualification run against it before then. Its Carrot address scheme
keeps existing addresses, subaddresses and keys working: "maintains backwards compatibility with existing addresses",
and "preexisting Cryptonote addresses, subaddresses, and integrated addresses. No wallet key migration is required" (the
Carrot specification, jeffro256/carrot `carrot.md` at 08094f7c70fb, lines 3 and 25). The fork was in public beta on
2026-10-26 (seraphis-migration/monero `v0.19.0.0-beta.3.1`, "FCMP++ & Carrot beta stressnet v3.1").
