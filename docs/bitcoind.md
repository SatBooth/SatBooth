# Setting up bitcoind (on-chain)

**Use a bitcoind dedicated to the hub and SatBooth, with no wallet that holds keys loaded on it.** (Each bitcoind fact
in this section is marked *source*, read in the Bitcoin Core 32.1 source, or *observed*, seen in qualification on
regtest.) SatBooth's RPC user is confined by `rpcwhitelist`, but the whitelist is per method, not per wallet (*source*:
`src/httprpc.cpp:107-118`; *observed*: `getnewaddress` as `satbooth` worked on another wallet), and `listdescriptors`
with `private` true returns the private descriptors of any loaded wallet that holds keys and is not locked
(*source*: `src/wallet/rpc/backup.cpp:479-519`; not observed). So a node that also serves a spending wallet lets a
stolen SatBooth password read that wallet's keys, unless the wallet is encrypted and stays locked. The hub needs the
same node as its chain source, and SatBooth refuses to start without that node (with the `onchain` block, bitcoind down at
boot stops the whole service, Lightning included; see [A supervisor](running.md)). The hub host's Compose bundle runs such a
bitcoind (`deploy/README.md`, "The hub host").

SatBooth needs a Bitcoin Core node it can reach over the private link (qualified against Core 32.1, `qualify/REPORT.md`
"On-chain end to end"). The operator makes the wallet and imports the descriptor with `bitcoin-cli`; SatBooth's own RPC
user cannot. The examples are on testnet, like the example configuration in [Configuration](configuration.md): the
`bitcoin-cli` lines carry `-testnet`. Drop it on mainnet, use `-signet` or `-regtest` on those networks, or
`-conf=<file>` when the node's configuration (with its `chain=`) is elsewhere; without the right flag `bitcoin-cli`
talks to the mainnet port and wallet directory.

1. **A dedicated account.** In the merchant's wallet, make a new account (its own extended public key) for SatBooth,
   and export that account's extended public key with its key origin: on testnet a `tpub` with BIP-84 coin type `1h`,
   for example `wpkh([d34db33f/84h/1h/1h]tpub.../0/*)`; on mainnet an `xpub` with coin type `0h`, for example
   `wpkh([d34db33f/84h/0h/1h]xpub.../0/*)`. **No other wallet may issue addresses from it**: a wallet that hands out
   addresses from the same `<key>/0/*` chain reuses SatBooth's addresses, and its receipts count toward SatBooth's
   orders.
2. **SatBooth's RPC user, in `bitcoin.conf`:**

       rpcauth=satbooth:<salt>$<hash>
       rpcwhitelist=satbooth:getnewaddress,getaddressinfo,deriveaddresses,listsinceblock,gettransaction,getmempoolentry,getblockchaininfo,getwalletinfo,listdescriptors,getdescriptorinfo,getmempoolinfo,gettxspendingprevout
       rpcwhitelistdefault=0

   - `share/rpcauth/rpcauth.py satbooth`, in the Bitcoin Core source, prints the `rpcauth` line and a new password
     (source: `src/httprpc.cpp:278` names it). Put the password in `onchain.rpc_password_file`, mode 0600. An
     `rpcauth` line joins any `rpcuser`/`rpcpassword` pair in the same credential list, so both users work (source:
     `src/httprpc.cpp:248-308`; observed).
   - The whitelist is exactly these twelve methods. SatBooth's client allows the same twelve and refuses any other
     before sending it. **Upgrading from a build with ten:** add `getmempoolinfo` to the line (SatBooth asks it each
     pass whether bitcoind has finished loading its mempool after a restart, and marks no payment gone until it has),
     and `gettxspendingprevout` (SatBooth asks it before it marks a payment gone, to tell a payment that bitcoind's
     wallet has not yet seen replaced from one that left the mempool); **from a build with eleven:** add
     `gettxspendingprevout`. Without either, the startup check fails with the reason `onchain`. SatBooth's on-chain rail
     needs Bitcoin Core 31 or newer: `gettxspendingprevout` takes its `mempool_only` option from Core 31, and the
     startup check refuses an older node through this call. For this user bitcoind answers any other method with HTTP
     403 (observed: `sendtoaddress`, `createwallet`, `importdescriptors` and `stop`). A second `rpcwhitelist` line for
     the same user narrows the list to the methods
     both lines name (source: `src/httprpc.cpp:315-332`).
   - `rpcwhitelistdefault=0` keeps every user without a whitelist (your own, and the cookie `bitcoin-cli` uses)
     unrestricted. Without it, a single `rpcwhitelist` line locks out every other user (source: `src/httprpc.cpp:314`;
     observed with it: the full user was unaffected).
   - The hub bundle confines the hub's own user too, to the eleven calls its chain source makes, a different list
     from SatBooth's (`deploy/hub/config-example/bitcoin.conf`; the source lines are in `docs/dependencies.md` §8).
     With `LDK_GOSSIP_SOURCE` left empty, as in the bundle, the hub checks channel announcements with `getblockhash`
     and `gettxout`, both in that list.
   - The whitelist reaches every wallet the node has loaded: see the first paragraph of this section.
3. **The watch-only wallet**, with the full user:

       bitcoin-cli -testnet -named createwallet wallet_name=satbooth disable_private_keys=true blank=true descriptors=true load_on_startup=true

   It has no keys and no descriptor until the import below (observed: after the import it holds exactly one).
   `load_on_startup` matters: SatBooth's user cannot load a wallet, so after a bitcoind restart the wallet must come
   back by itself (observed: the test's `miner` wallet, made without it, had to be loaded again after each bitcoind
   kill, `scripts/qualify.sh`; the watch-only wallets made with it came back).
4. **The checksum.** `importdescriptors` requires the descriptor's `#checksum` (source: `src/wallet/rpc/backup.cpp:164`).
   Get it with

       bitcoin-cli -testnet getdescriptorinfo "wpkh([d34db33f/84h/1h/1h]tpub.../0/*)"

   and use the `descriptor` field of the answer, which carries it.
5. **The import**, once:

       bitcoin-cli -testnet -named -rpcwallet=satbooth importdescriptors requests='[{"desc":"<the descriptor with its #checksum>","active":true,"internal":false,"timestamp":"now"}]'

   `"now"` scans no history, which a new account does not need (source: `src/wallet/rpc/backup.cpp:135-144`). With no
   `range`, the import covers indexes 0 to 999 and starts at `next_index` 0 (source: `src/wallet/rpc/backup.cpp:178-191`);
   bitcoind extends the range itself as SatBooth asks for addresses (observed: `listdescriptors` shows the range
   `[0, next_index + 998]` after each new address; source: `src/wallet/scriptpubkeyman.cpp:948`, `:1121`, as cited in
   `qualify/REPORT.md`).

   **Import exactly one receive descriptor.** SatBooth's startup check counts the wallet's active descriptors that
   are not `internal`: it needs exactly one, and it must be the configured one, so a second active receive descriptor
   (a `tr` beside the `wpkh`, or another account) stops SatBooth at startup (`onchain/check.go`). A change descriptor
   (`"internal":true`) or an inactive one is ignored; the watch-only wallet never spends, so it needs none.
6. **Never re-import without `next_index`.** A wallet restored from an older backup, or the descriptor imported again
   without `next_index`, moves the wallet's `next_index` back (observed: to just past the last paid address, not to 0),
   and bitcoind would hand out addresses SatBooth already showed. SatBooth refuses to start then (`ALERT: wallet
   next_index is behind SatBooth's record; addresses are not issued`). To recover, import the same descriptor again
   with `"next_index"` set to SatBooth's highest recorded index plus 1 (`SELECT max(idx) FROM addresses` in
   `store.db`), and a `"range"` that contains it and is not smaller than the wallet's current one (observed: a
   smaller range is refused; `next_index` must lie inside the range, source: `src/wallet/rpc/backup.cpp:194-198`).
   A rebuilt wallet knows no transactions until it is rescanned, and step 5's `"timestamp":"now"` scans none: import
   with a `"timestamp"` (Unix seconds) at or before the creation of the oldest order that can still be paid (its
   expiry plus 7 days not yet passed) and of every output the store still tracks, so that bitcoind rescans the
   payments the restored store will look for; when in doubt, the time the descriptor was first imported.

   **After SatBooth's own store was restored from a backup (see [Backup and restore](backup-and-restore.md)), do not use that plus 1.** The
   restored store's highest index is the backup's: addresses of orders made after the backup were shown to customers
   and the store no longer knows them. Set `"next_index"` to the restored store's highest index plus a margin that
   is generously above the number of on-chain addresses that could have been issued since the backup (an address is
   issued when a customer first presses "Pay with bitcoin": take the most on-chain orders you ever had in a day,
   times the days since the backup's `created_at`, times ten), and make the `"range"` at least `[0, next_index +
   1000]` so that it contains it. Why: an address already shown to the customer of an order the restore lost must
   never be handed to a new order, because a late payment to it would mark the new order paid; SatBooth cannot know
   that address, and refuses only an index that is not above the store's own highest. The skipped indexes stay unused
   in the merchant's own wallet: raise its gap limit above the margin, or it will not find later payments (see "The
   merchant's wallet" below).

At every start SatBooth checks, in this order: bitcoind's chain is the configured `network`; the wallet is a
descriptor wallet with private keys disabled; `getdescriptorinfo` of the configured descriptor has no private keys
and is ranged; the wallet has exactly one active receive descriptor, and it is that one (so a `wpkh` and a `tr`
descriptor need two wallets and two services); and its `next_index` is above every index SatBooth recorded. Any
failure stops the start. It logs `ALERT: bitcoind is in initial block download or behind its headers` when the
node is syncing or more than 3 blocks behind its headers, and goes on.

**The merchant's wallet.** SatBooth never reuses an address: an order gets the next index when the customer first presses
"Pay" on its page, and orders that are never paid leave unused addresses behind. A wallet restored from the account's seed
stops looking after a run of unused addresses (its gap limit, often 20). **Raise the gap limit** of the merchant's
wallet above the longest run SatBooth may leave, or it will not find later payments. SatBooth logs `ALERT: unused
on-chain addresses exceed gap_alert; raise the merchant wallet's gap limit`, with the count, when the issued addresses
above the highest one that received a payment first number `gap_alert` or more, then once a day while they still do
(and again on the first pass after a restart).
