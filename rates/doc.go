// Package rates owns fiat pricing in SatBooth: the exact decimal that carries prices, rates and margins,
// the one conversion from a fiat price to satoshis (Sats), and the exchange rates it is fed.
//
// Money never passes through floating point. A price or a rate is a Decimal: an exact big integer of
// units at a fixed scale of 10^18, parsed from decimal text by a byte-level grammar and printed back
// canonically. A margin is an integer of basis points. Sats takes the ceiling of an exact rational, so a
// conversion never underprices and never rounds a positive price down to 0. A source scan in the tests
// keeps float types and float conversions out of rates, api, store and checkout.
//
// The rate is designed per asset: a provider fetches the rates of one asset against a list of
// currencies, and the cache is keyed by asset and currency. BTC and XMR are built: each asset has the providers that
// quote it and its own entries, and there is one conversion for both.
package rates
