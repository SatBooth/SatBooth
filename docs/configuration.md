# Configuration

One JSON file, at most 1 MiB, with no secrets: every secret is in a file of its own, named here. Unknown, duplicate and
case-variant keys are refused. Example:

    {
      "public_listen": "192.0.2.10:8080",
      "private_listen": "192.0.2.11:8081",
      "checkout_base_url": "https://pay.example.com",
      "data_dir": "/var/lib/satbooth",
      "nwc": {
        "relay_url": "ws://192.0.2.20:7777",
        "wallet_pubkey": "<64 hex characters: the hub's Wallet Public Key>",
        "client_key_file": "/etc/satbooth/nwc-client.key",
        "network": "testnet"
      },
      "api_keys_file": "/etc/satbooth/api-keys.json",
      "webhooks": [
        { "name": "main", "url": "https://merchant.example.com/satbooth-hook", "secret_file": "/etc/satbooth/webhook.secret" }
      ],
      "limits": { "daily_msat": 100000000000, "max_msat": 10000000000 },
      "onchain": {
        "rpc_url": "http://192.0.2.20:18332",
        "rpc_user": "satbooth",
        "rpc_password_file": "/etc/satbooth/bitcoind.pass",
        "wallet": "satbooth",
        "descriptor": "wpkh(tpubD6NzVbkrYhZ4XgiXtGrdW5XDAPFCL9h7we1vwNCpn8tGbBcgfVYjXyhWo4E1xkh56hjod1RhGjxbaTLV3X4FyWuejifB9jusQ46QzG87VKp/0/*)",
        "network": "testnet"
      },
      "monero": {
        "wallet_rpc_url": "http://192.0.2.20:18083",
        "rpc_user": "satbooth",
        "rpc_password_file": "/etc/satbooth/wallet-rpc.pass",
        "address": "4kcfbiPsJFptZtA4KDH26YKnA9APQpr28aLUWg4PJ5Q9a7KN35LsHRRQ7MVDS1gXyYNVo8PqLy639jeTZQN6urMNCVRUWMc",
        "network": "mainnet",
        "account_index": 0,
        "confirmations": [
          {"up_to_piconero": 1000000000000, "confirmations": 1},
          {"up_to_piconero": 10000000000000, "confirmations": 3},
          {"up_to_piconero": 0, "confirmations": 10}
        ],
        "unused_alert": 100,
        "poll_seconds": 10
      },
      "rates": {
        "providers": ["kraken", "coinbase", "yadio", "bitfinex"],
        "custom_url_file": "/etc/satbooth/rates.url",
        "currencies": ["USD", "EUR"],
        "margin_percent": "1",
        "min_sources": 2,
        "max_spread_percent": "2",
        "max_age_seconds": 300,
        "fetch_seconds": 60
      },
      "payout": {
        "relay_url": "ws://192.0.2.20:7777",
        "wallet_pubkey": "<64 hex characters: the payout connection's Wallet Public Key>",
        "client_key_file": "/etc/satbooth/payout.key",
        "network": "testnet",
        "approval_keys": ["17a51b772fb3161b469ec4ec170385184e1655a279a7bd16dfff09276406f61c"],
        "max_msat": 10000000,
        "daily_msat": 100000000,
        "budget_alert_msat": 5000000
      }
    }

**Replace the example descriptor.** Its `tpub` is BIP-32 test vector 1, whose private key is published with the
vector: anyone can spend from it. Put your own account's extended public key there, and never fund an address
derived from the example.

**Replace the example approval key.** It is an example only: never configure a public key whose private key you did
not make yourself. The example is the SHA-256 of the ASCII string `satbooth example approval key: nobody holds its
private key`, a valid Ed25519 point whose private key nobody holds (`cmd/satbooth`'s `TestExampleApprovalKey`
recomputes it); so no approval can be signed for it, and SatBooth with it pays no refund. Replace it with your own key
from `satbooth-approve keygen`, made on the admin system, in `payout.approval_keys` (see [Refunds](refunds.md)).

**Replace the example Monero address.** It is a made-up address of the right shape (the fake wallet-rpc of the tests,
`monerotest`), with no wallet behind it. Put your own view-only wallet's primary address there (see [Setting up
monerod and monero-wallet-rpc](monero.md)).

| Key | Meaning |
|---|---|
| `public_listen` | `host:port` of the checkout page; the host must be a specific IP address (no hostname, no wildcard) |
| `private_listen` | `host:port` of the merchant API; the same rule; it must differ from `public_listen`. Put it on WireGuard or a private network, never on the internet |
| `checkout_base_url` | the public origin of the checkout page, `http` or `https`, no query or fragment. An order's `checkout_url` is this plus `/c/<token>` |
| `data_dir` | the directory of `ledger.db` and `store.db` |
| `nwc.relay_url` | the `ws://` or `wss://` URL of the wallet relay. **The relay checks the client's NIP-42 AUTH URL byte for byte against its `client_relay_url`**, so this string must equal that one exactly (scheme, host, port, trailing slash). Behind a proxy that rewrites the URL, set the relay's `client_relay_url` to this string |
| `nwc.wallet_pubkey` | the hub connection's "Wallet Public Key", 64 hex characters |
| `nwc.client_key_file` | the file made by `satbooth-pair` |
| `nwc.timeout_seconds` | optional, 10 to 600, default 70: how long a wallet call waits for its answer |
| `nwc.network` | required: `mainnet`, `testnet`, `signet` or `regtest`, the network the hub is on. SatBooth refuses every invoice the hub makes for another network or another amount (see [Rules for Lightning payments](rules-lightning.md)). With an `onchain` block, `onchain.network` must equal it, and with a `payout` block, `payout.network` must equal it: a difference is a configuration error naming both keys. `monero.network` is Monero's own and is not compared. The data directory records the network at the first start that passes its checks, permanently, and a start under another network is refused ("this data directory belongs to …"). A store made before the record that already holds invoices is recorded only under their network: another `nwc.network` is refused ("this data directory holds invoices of another network") and records nothing. At every start SatBooth asks the hub for its network (`get_info` on the invoice connection, and on the payout connection with a `payout` block) and refuses to start when the hub names another network than this key, or none (see [Pairing with the hub](running.md)). A wrong `nwc.network` therefore does not start and records nothing |
| `api_keys_file` | see above |
| `webhooks` | required: a list of 1 to 8 endpoints `{"name", "url", "secret_file"}` that receive every event (see [Webhooks](webhooks.md) and the rules below). It replaces the key `webhook` |
| `webhooks[].name` | 1 to 64 characters of `[A-Za-z0-9._-]`, unique in the list |
| `webhooks[].url` | where this endpoint's webhooks are posted; `http` or `https`, no user info or fragment, **a query allowed** (a token, for example), at most 512 bytes, unique in the list. The webhook URLs are the only destinations of outbound requests besides the relay and, with the `rates` block, the rate providers and, with the `alerts` block, `alerts.url` |
| `webhooks[].secret_file` | see above; unique in the list |
| `alerts` | optional: `{"url", "secret_file"}`, with a `webhooks` entry's rules and a URL different from every endpoint's (see [Alerts](alerts.md)) |
| `limits` | the limits, below |
| `invoice_expiry_minutes` | optional, 15 to 30, default 20 |
| `allow_swap` | optional, default false; see [Locked memory](running.md) |
| `rates` | optional: fiat pricing, below ("The `rates` block"). Without it, an order with `price` is refused (`fiat_disabled`) |
| `onchain` | optional: the on-chain rail, below. Without it, an order with `onchain_sat` is refused (`onchain_disabled`), and `satbooth` refuses to start (reason `onchain`) while the store holds an order with `onchain_sat` inside its 7-day watch window without a final event, or a payment first seen before its order's expiry that is not settled yet, whatever its window (a stuck payment, see [The watch window](rules-onchain.md); a dead one counts until a pass has settled it): nothing else would settle it, so restore the block |
| `payout` | optional: Lightning refunds, below ("The `payout` block"). Without it every refund route answers 404 `refunds_disabled`, and `satbooth` refuses to start (reason `payout`, `the store holds refunds that are not resolved yet, and the configuration has no payout block; restore it`) while the store holds a refund with no result |
| `monero` | optional: the Monero rail, below. Without it, an order with `monero_piconero` (or a priced order whose `rails` name `monero`) is refused (`monero_disabled`), and `satbooth` refuses to start (reason `monero`, `the store holds Monero orders that are not settled yet, and the configuration has no monero block; restore it`) while the store holds an order with `monero_piconero` inside its 7-day watch window without a final event, or a stuck Monero payment that is not settled: restore the block |

**Rules of `webhooks`.** Every entry has a `name`, a `url` and a `secret_file`, and no other key. Names are unique, and
so are URLs and secret file paths. No two secrets are equal as HMAC keys (a secret that is another followed by zero
bytes is the same key, and is refused too), and none equals the alert secret. `alerts.url` differs from every
endpoint's URL. A message that names an entry gives its position in the list, counted from 0: `webhooks[0]` is the
first entry. A configuration with the old key `webhook` is refused, with or without `webhooks` beside it, with the
message `config: webhook is replaced by webhooks (a list of endpoints)`. Change

    "webhook": { "url": "https://merchant.example.com/satbooth-hook", "secret_file": "/etc/satbooth/webhook.secret" }

to

    "webhooks": [{ "name": "main", "url": "https://merchant.example.com/satbooth-hook", "secret_file": "/etc/satbooth/webhook.secret" }]

and keep the name `main`, so that the attempts recorded so far stay with the endpoint (see [Webhooks](webhooks.md)).

None of the limits is per IP; `max_open_per_account` is the one per-account limit, keyed by the merchant's
`account_id` (threat model §1). The others are global. A zero or missing field takes the default, except `daily_msat`
and `max_msat`, which are required. A refused `limits` block stops the start with one fixed text,
`config: limits are invalid (daily_msat and max_msat are required; see the limits package for the bounds)`, which
names no key: check each value against the table.

| `limits` key | Default | Meaning |
|---|---|---|
| `rate_per_sec` | 1 | invoice creations per second (token bucket); up to 1,000,000 |
| `burst` | 10 | bucket size; 1 to 1,048,576 |
| `max_open` | 1000 | open invoices, all classes; 1 to 2^30 |
| `new_share` | 0.2 | the most of `max_open` that accounts with no paid order (the `new` class) may hold; `known` accounts may use any slot, including unused `new` ones. The number of `new` slots is `floor(new_share × max_open)`, and must be at least 1 and below `max_open`. With the default share, a `max_open` below 5 is refused at startup: set an explicit share. Unused `new` slots are lent to `known` accounts: only `max_open` caps them |
| `daily_count` | 5000 | invoice attempts per UTC day; an attempt counts once the wallet has been asked, and the count is read back from the store after a restart |
| `daily_msat` | required | msat attempted per UTC day |
| `min_msat` | 1000 | smallest order amount |
| `max_msat` | required | largest order amount, at most 2^53 |
| `max_open_per_account` | 5 | open invoices one `account_id` may hold, both classes, on top of `max_open` and `new_share`; 1 to `max_open`. When `max_open` is below 5, the default is `max_open` |

An order amount is a whole number of sats (a multiple of 1000 msat), because the hub refuses partial sats.

The `onchain` block. A zero or missing number, and an empty `confirmations` list, take the default; inside a tier,
`confirmations` is required (0 is a value, not the default).

| `onchain` key | Default | Meaning |
|---|---|---|
| `rpc_url` | required | bitcoind's RPC URL, `http` or `https`, with a host and no user info, query or fragment, at most 512 bytes. SatBooth adds `/wallet/<wallet>` for the wallet calls. Reach it over the private link, never the internet |
| `rpc_user` | required | SatBooth's own RPC user (see [Setting up bitcoind](bitcoind.md)), 1 to 64 characters of `[A-Za-z0-9._-]` |
| `rpc_password_file` | required | the file with that user's password (see [Secret files](running.md)) |
| `wallet` | required | the watch-only wallet's name, 1 to 64 characters of `[A-Za-z0-9._-]`, not `.` or `..` |
| `descriptor` | required | `wpkh(KEY/0/*)` or `tr(KEY/0/*)`: one extended public key, `xpub` on `mainnet` and `tpub` on the other networks, with an optional key origin `[fingerprint/path]` (`h` or `'` for hardened steps), at most 256 characters. A trailing `#checksum` is accepted and dropped unchecked; bitcoind's own checks follow at startup. Anything with private key material (`xprv`, `tprv` and the other private versions, a WIF, a private key behind a public version byte) is refused before any call to bitcoind, and never logged |
| `network` | required | `mainnet`, `testnet`, `signet` or `regtest`; it must match bitcoind's chain. **testnet4 is not supported** |
| `confirmations` | 1 / 3 / 6 | tiers of `{"up_to_sat": N, "confirmations": M}`: an order of up to N sat needs M confirmations. The default is 1 up to 1,000,000 sat, 3 up to 10,000,000 sat, 6 above. `confirmations` is 0 to 100 and never decreases; **every tier needs the key**; 0 means the payment counts as soon as SatBooth sees it in bitcoind's mempool (see "0-conf risk" under [Rules for on-chain payments](rules-onchain.md)); the last tier's value, at least 1, is the depth at which a payment is settled (`SettledDepth`: a last tier of 0 settles at 1). `up_to_sat` increases strictly, and the last tier has `up_to_sat` 0 (no upper bound). The order's `onchain_sat` picks the tier. Raising a 0 tier later reverses the payments it recorded from the mempool that are not mined yet (`order.reverted` when the order stops reading paid, then `order.paid_again` once they are mined at the new tier) |
| `gap_alert` | 1000 | 1 to 1,000,000: the run of unused addresses that logs an `ALERT` (see [The merchant's wallet](bitcoind.md)) |
| `poll_seconds` | 10 | 2 to 60: how often the watcher starts a pass |
| `poll_rate` | 5 | above 0, at most 10,000: tracked payments refreshed a second, all together (one `gettransaction` each, plus `getmempoolentry` for one with no confirmation; one `getmempoolinfo` a pass; and in the pass that marks a payment gone, one more `getmempoolinfo`, `getmempoolentry` and `gettransaction` for it, then one `gettxspendingprevout`) |
| `timeout_seconds` | 30 | 1 to 600: the bound on every call to bitcoind |

The `monero` block. As for `onchain`, a zero or missing number, and an empty `confirmations` list, take the default;
inside a tier, `confirmations` is required. A refused block stops the start (exit code 2) with a fixed text that names
the key and never the value, for example `config: monero: address must be the wallet's primary address for network`.

| `monero` key | Default | Meaning |
|---|---|---|
| `wallet_rpc_url` | required | monero-wallet-rpc's URL, under the rules of `onchain.rpc_url` (`http` or `https`, a host, no user info, query or fragment, at most 512 bytes). SatBooth posts to `/json_rpc` below it. Reach it over the private link, never the internet |
| `rpc_user` | required | the user of wallet-rpc's `--rpc-login`, 1 to 64 characters of `[A-Za-z0-9._-]` |
| `rpc_password_file` | required | the file with that login's password (see [Secret files](running.md)) |
| `address` | required | the wallet's primary address (account 0's, whatever `account_index` is): 95 characters of the base58 alphabet, starting with `4` on `mainnet` and `regtest`, `9` or `A` on `testnet`, `5` on `stagenet`. A subaddress is refused. SatBooth never decodes it: the startup check compares it with what wallet-rpc answers |
| `network` | required | `mainnet`, `testnet`, `stagenet` or `regtest` (regtest uses mainnet's address format) |
| `account_index` | 0 | the wallet account whose subaddresses SatBooth issues; it must exist in the wallet |
| `confirmations` | 1 / 3 / 10 | tiers of `{"up_to_piconero": N, "confirmations": M}`, under the rules of `onchain.confirmations` (M is 0 to 100 and never decreases, `up_to_piconero` increases strictly and is 0 in the last tier, the last tier's value, at least 1, is the settled depth). The default is 1 up to 1 XMR (10^12 piconero), 3 up to 10 XMR, 10 above. 0 means a payment counts as soon as wallet-rpc lists it in the tx pool (see [Rules for Monero payments](rules-monero.md)) |
| `unused_alert` | 100 | 1 to 10,000: the run of unused subaddresses that logs an `ALERT` (see [The merchant's own wallet](monero.md) under [Setting up monerod and monero-wallet-rpc](monero.md)) |
| `poll_seconds` | 10 | 2 to 60: how often the watcher starts a pass |
| `poll_rate` | 5 | 1 to 10,000: tracked payments refreshed a second, all together (one `get_transfer_by_txid` each) |
| `timeout_seconds` | 30 | 1 to 600: the bound on every call to wallet-rpc |

The `rates` block (fiat pricing; see [Rules for fiat pricing](rules-fiat.md) for what happens at order time). A zero or missing
number, and a missing or empty percentage, take the default. A refused block stops the start (exit code 2) with a
fixed text that names the key and never the value.

| `rates` key | Default | Meaning |
|---|---|---|
| `providers` | required | the built-in providers, each at most once: `kraken` (Kraken's public ticker, the mid of the best bid and ask), `coinbase` (Coinbase's exchange rates), `yadio` (Yadio's exchange rates) and `bitfinex` (Bitfinex's public tickers, the mid of the best bid and ask). The providers and the custom API (`custom_url` or `custom_url_file`) together must number at least `min_sources`. Which assets and currencies each quotes is in the table below |
| `custom_url` | none | optional: your own rate API, in the format below. `https` only, with a host, no user info, query or fragment, no spaces or control characters, at most 512 bytes; the URL is never logged. **A query is refused** with `config: rates.custom_url must not have a query: a URL that carries a token goes in rates.custom_url_file, a file with mode 0600` |
| `custom_url_file` | none | optional, in place of `custom_url` (setting both is refused): a file holding your rate API's URL, under the rules of the secret files above. Use it whenever the URL carries a token: the file is loaded in-process at startup, never logged, and the configuration file stays free of secrets |
| `currencies` | required | 1 to 32 distinct codes of 3 uppercase letters (`A` to `Z`), neither `BTC` nor `XMR`: the currencies an order's `price` may be in, and the only ones asked of the providers or kept from their answers. For bitcoin, a code that fewer than `min_sources` of the configured sources can ever quote stops the start with `config: rates.currencies has a code that fewer than min_sources providers quote for BTC`, followed by the lists of the table providers configured, for example `(kraken quotes only AUD, CAD, CHF, EUR, GBP, JPY and USD; bitfinex quotes only EUR, GBP and USD)`. Coinbase, Yadio and the custom API are taken to quote any code for bitcoin, since that cannot be known before a fetch. For Monero there is no startup rule: a code too few sources can quote for XMR is *unreachable* (see [Rules for fiat pricing](rules-fiat.md)) |
| `margin_percent` | `"0"` | a decimal string, -50 to 50 with at most 2 decimals, applied after the median: `"1.5"` charges 1.5% more. **It may be negative** (a discount): `"-5"` charges 5% less. A JSON number here (`1.5` without quotes) is refused with the generic `config: not a valid configuration object` |
| `min_sources` | 2 | 2 to 5: the fewest fresh rates that price an order, per asset. Never 1, so no source, the custom API included, ever prices alone |
| `max_spread_percent` | `"2"` | a decimal string, 0.01 to 50 with at most 2 decimals: when the highest and the lowest fresh rate differ by more than this share of the lowest, no order is priced in that currency |
| `max_age_seconds` | 300 | 30 to 3600: a rate older than this (by the provider's own timestamp, or by the fetch time for Kraken, Coinbase and Bitfinex, which send none), or stamped more than 60 s ahead of SatBooth's clock, does not count |
| `fetch_seconds` | 60 | 10 to 600, and at most half of `max_age_seconds`: how often each provider is asked. **The default trips this rule when `max_age_seconds` is below 120**: such a block is refused with the `fetch_seconds` text (`config: rates.fetch_seconds must be 10 to 600 and at most half of max_age_seconds`) even though it does not set `fetch_seconds`; set both |

What each source quotes. Bitcoin is always fetched; Monero only when the `monero` block is set.

| Source | Bitcoin (BTC) | Monero (XMR) |
|---|---|---|
| `kraken` | AUD, CAD, CHF, EUR, GBP, JPY, USD | EUR, USD |
| `bitfinex` | EUR, GBP, USD | USD |
| `coinbase` | any code it lists | none |
| `yadio` | any code it lists | none |
| the custom API | any code it answers | any code it answers |

XMR prices in a currency only when at least `min_sources` of the configured sources quote XMR in it, and only Kraken,
Bitfinex and the custom API quote XMR. So with the built-in providers alone, XMR prices only in USD (Kraken and
Bitfinex, at `min_sources` 2); EUR needs a second source beside Kraken, which today only the custom API can be.

The custom rate API answers a `GET` of its URL with the query parameter `asset` added (`?asset=BTC` or `?asset=XMR`,
or `&asset=…` after a query the URL already has) with HTTP 200 and a JSON object with exactly these keys:

    {"asset":"BTC","rates":{"USD":"76022.31","EUR":"67979.91"},"timestamp":1793902508}

- `asset`: the asset the rates are for; it must be the one the `asset` parameter asked for (`BTC` for bitcoin, `XMR`
  for Monero);
- `rates`: an object of currency code to a decimal string, the currency per one coin of that asset: 1 to 20 digits,
  optionally a point and 1 to 18 digits (no sign, no exponent, no space). A code not in `currencies` is ignored; a value
  that is not such a string, or is zero, is skipped;
- `timestamp`: an integer, Unix seconds, when the rates were computed; it stamps every rate, and `max_age_seconds`
  measures from it.

An unknown or missing key, a wrong type, a duplicate key, invalid UTF-8 or another asset refuses the whole answer,
as do a body over 256 KiB, a redirect and any other status than 200. The custom API is one source among the others,
under the same median, freshness and spread rules.

**Egress.** The rate providers are outbound destinations of the public host. The egress firewall must allow HTTPS to
`api.kraken.com`, `api.coinbase.com`, `api.yadio.io` and `api-pub.bitfinex.com` (those of them you list) and to the
custom URL's host. Every
request is a `GET` over HTTPS (TLS 1.2 or later), follows no redirect, takes no proxy from the environment, waits at
most 10 s, reads at most 256 KiB, and carries `User-Agent: SatBooth` and nothing else that names the deployment; it
carries only the asset and the currency codes, never an order or an amount.

**Credits and terms.** Rates from [yadio.io](https://yadio.io) are used with attribution, as its terms ask; an
operator who enables `yadio` credits yadio.io, with a link, wherever they show its data (`pricing.sources` names it).
The checkout page does it by itself: it shows "Rates: yadio.io", with yadio.io as a link, on an order that Yadio
helped price. The market-data terms of Kraken, Coinbase and Bitfinex are the operator's to check for their own use
(threat model §10, threat 7).

The `payout` block (Lightning refunds; see [Refunds](refunds.md) for how they work and the operator's steps). It holds no secret.
A refused block stops the start (exit code 2) with a fixed text that names the key and never the value.

| `payout` key | Default | Meaning |
|---|---|---|
| `relay_url` | required | the `ws://` or `wss://` URL of the wallet relay for the payout connection, under the rules of `nwc.relay_url` (it must equal the relay's `client_relay_url` byte for byte). Usually the same relay and URL as `nwc.relay_url`, with the payout connection as a second pair |
| `wallet_pubkey` | required | the payout connection's "Wallet Public Key", 64 hex characters; it must differ from `nwc.wallet_pubkey` |
| `client_key_file` | required | the payout connection's client key, made by `satbooth-pair -connection payout`; it must differ from the invoice connection's |
| `timeout_seconds` | 70 | 10 to 600: refund calls are cut at 15 s, or at this value if it is lower (8 s for the void's lookup); the value also sets the request's expiration: a refund's request expires at sent_at + this value |
| `network` | required | `mainnet`, `testnet`, `signet` or `regtest`: the network of the approvals and of the claimant's invoice |
| `approval_keys` | required | 1 to 8 distinct Ed25519 public keys, 64 lowercase hex characters each: the admin system's keys. Rotate one by editing the list and restarting; a claimed refund whose key was removed fails `key_revoked` before it is sent |
| `max_msat` | required | the most one refund may pay, a whole number of sats, above 0 and at most `daily_msat` |
| `daily_msat` | required | the most the refunds claimed on one UTC day may add up to (every refund that has not failed counts), a whole number of sats, at most 2^53 |
| `budget_alert_msat` | 0 | 0 to 2^53: an `ALERT` when the payout budget left at the hub after a refund falls below this (0: never) |
