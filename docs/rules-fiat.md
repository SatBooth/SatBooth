# Rules for fiat pricing

- **The fetcher.** With the `rates` block, SatBooth asks every configured provider in parallel at startup and then
  every `fetch_seconds`, one request per provider and asset each time (BTC, and XMR when the `monero` block is set),
  and keeps each provider's last good answer. Creating an
  order reads only that cache: an order never waits on a provider, and a flood of orders makes no extra request.
  `satbooth` starts whether or not a provider answers, so priced orders answer 503 `rate_unavailable` until the first
  fetch has landed (under a second in the qualification run; at most the 10 s provider timeout) (*observed*:
  `qualify/REPORT.md`, plan 7, the time from the start to the first quote). A provider that fails keeps its previous
  rates, which stop counting once they are older than `max_age_seconds`.
- **The rules, per asset and currency, at order time:** the rates no older than `max_age_seconds` and no more than
  60 s ahead of the clock count; at least `min_sources` of them must be left, from distinct providers; if the highest
  and the lowest differ by more than `max_spread_percent` of the lowest, nothing is priced; otherwise the median prices
  the order (for an even count, the lower mean of the two middle rates, to the smallest unit of the decimal). There is
  no outlier dropping. Any failure is 503 `rate_unavailable`; explicit orders are never affected.
- **Monero prices.** XMR is priced under the same rules, from the sources that quote it: Kraken (USD, EUR), Bitfinex
  (USD) and the custom API (see the table under [The `rates` block](configuration.md)); Coinbase and Yadio do not quote XMR. An order gets
  Monero only when its `rails` name `monero`, so a bitcoin sale never depends on the XMR sources. **A pair that fewer
  than `min_sources` of the configured sources can ever quote is *unreachable*:** XMR in EUR with Kraken and Bitfinex
  alone, for example, or in any code but USD and EUR without the custom API. An unreachable pair is not fetched, raises
  no `ALERT`, and a priced order naming `monero` in that currency is a permanent 400 `invalid_currency` (do not retry
  it). 503 `rate_unavailable` means an outage of a reachable pair (*observed*: `qualify/REPORT.md`, "Monero end to end",
  `TestMoneroFiat`).
- **The rate is locked at creation** until the order's expiry, and it stays on the record: the price, the currency,
  the median, each source's rate and time, and the margin are stored with the order in their own table
  (`order_quotes`), which nothing updates or deletes. A restart does not change it (*observed*: `qualify/REPORT.md`,
  plan 7, an order read after a restart with every provider's rate moved), nor does a change of `margin_percent`.
  The rates are stored per asset (`order_quote_rates`, one row each for BTC and XMR). The API (`pricing`) and every
  webhook carry it. An on-chain or Monero payment first seen before the expiry counts at the locked amount even when
  it confirms days later, inside the watch window, and a Lightning payment of the order's invoice is the locked amount
  whenever it settles.
- **Pick `expires_in` for the market.** The price can move against the merchant for as long as the order can be
  paid, and on-chain a payment seen before the expiry still counts once it confirms. In a volatile market, set
  `expires_in` as short as the customer needs to pay (for example 900 s, 15 minutes, rather than the default 3600),
  and let `margin_percent` absorb what is left. After an expiry, a new priced order (with a new `merchant_ref`) gets
  a new quote at the rate of that moment; to keep the old price, your app sends the old amounts as explicit amounts.
- **One lying provider among three blocks pricing, by design.** A provider whose rate is off by more than
  `max_spread_percent` from the others fails the spread rule for every order in that currency until it is removed
  from `providers` (and `satbooth` restarted); it is never voted out by the other two, and with two providers either
  one blocks. The `ALERT` below names it as `low` or `high`. Inside the bound a wrong rate moves the median
  undetected, by up to half of `max_spread_percent` when only two rates are fresh (threat model §10, threat 1).
- **Parallel-market currencies** such as ARS and VES may never price: the exchanges and Yadio can quote rates more
  than 2% apart there (threat model §10, threat 1).
- **Yadio's timestamp lags.** Yadio stamps its rates with its own time, and that time ran about 3.5 minutes (210 s)
  behind the request on 2026-11-05 (*observed*, threat model §10, threat 2). A rate is read for up to `fetch_seconds`
  after its fetch, so with the defaults (`max_age_seconds` 300, `fetch_seconds` 60) Yadio's rate is about 270 s old
  at worst and still counts, with little room: once its lag passes about 240 s it stops counting and the order is
  priced from the others, or refused if they are fewer than `min_sources` (*inferred*). Lower `max_age_seconds`
  only together with a faster `fetch_seconds`, and not below what Yadio's lag allows; below that, accept that Yadio
  often does not count. Under the defaults it still counts. A provider that hovers at the age bound can make the `ALERT` below come and go.
- **Terms.** Yadio asks to be credited with a link wherever its data is shown (see [Credits and terms](configuration.md)
  under [The `rates` block](configuration.md)). The market-data terms of Kraken, Coinbase and Bitfinex are the
  operator's to check for their own use.
- **Alerts and log lines.** Each is a fixed text; it names providers, an asset and a currency, never a rate, a URL or
  a provider's text:
  - `ALERT: fiat pricing is unavailable for a currency` (error level), with `asset`, `currency`, `reason` and
    `providers`. `reason` is `too_few` (fewer than `min_sources` fresh rates) or `disagree` (the spread rule failed);
    `providers` names the providers whose fresh rates were considered, and for `disagree` `low` and `high` name the
    providers with the lowest and the highest rate. It is logged at the end of a fetch when a currency changes state:
    on entering `too_few` or `disagree`, on a change between the two, and on the first fetch after a start if pricing
    is unavailable then. An unreachable XMR pair (above) never logs it;
  - `fiat pricing is available for a currency` (info), with `asset`, `currency` and `providers`, when a currency
    that was unavailable can be priced again (not on the first fetch);
  - `rate fetch failed` (warning, then debug), with `provider`, `asset`, `class` (`transport`, `status`, `too_large`,
    `shape` or `vendor`) and `status` (the HTTP status of a non-200, non-3xx answer; 0 otherwise: a redirect is class
    `transport` with `status` 0): at warning level for the first failure of a provider's run of failures for an asset,
    then at debug level (left out of the default log) until that provider answers again for it, so its next failure is
    a warning again.

What each fiat phase proves against the fake rate server, and a live run against the real providers, is in the
"Fiat pricing (plan 7)" section of [`qualify/REPORT.md`](../qualify/REPORT.md); the XMR rate's phase is `TestMoneroFiat`
in "Monero end to end".
