package rates

import (
	"errors"
	"math/big"
)

const (
	// MaxSats is the bitcoin supply in satoshis. rates imports no SatBooth package; store.MaxSat is defined from it.
	MaxSats int64 = 2_100_000_000_000_000
	// MaxPiconero is the most piconero one amount may be: 2^53 − 1, about 9,007 XMR (plan 8, decision 13: the
	// largest integer that JavaScript's JSON.parse still reads exactly). Monero has no fixed supply, so the bound is
	// SatBooth's own; store.MaxPiconero is the same number.
	MaxPiconero int64 = 9_007_199_254_740_991
	// MinMarginBps and MaxMarginBps bound the margin, in basis points (-50 % to +50 %); the store's quote check and
	// its SQL CHECK use them.
	MinMarginBps, MaxMarginBps int64 = -5000, 5000
)

var (
	ErrZero   = errors.New("rates: price or rate is zero")
	ErrMargin = errors.New("rates: margin out of range")
	ErrRange  = errors.New("rates: amount above the asset's bound")
)

var (
	satsPerCoin     = big.NewInt(100_000_000)
	piconeroPerCoin = big.NewInt(1_000_000_000_000)
	bpsPerOne       = big.NewInt(10_000)
	bigOne          = big.NewInt(1)
	bigMaxSats      = big.NewInt(MaxSats)
	bigMaxPiconero  = big.NewInt(MaxPiconero)
)

// ValidCurrency reports whether s is a fiat currency code: exactly 3 ASCII bytes in A to Z, and neither BTC nor
// XMR (the assets are not currencies). It is the one definition of the rule; store's SQL CHECK mirrors it.
func ValidCurrency(s string) bool {
	if len(s) != 3 || s == AssetBTC || s == AssetXMR {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

// Convert converts a price in a currency to base units of asset at rate (the currency per coin) with a margin in
// basis points: satoshis for AssetBTC (10^8 per coin, at most MaxSats), piconero for AssetXMR (10^12 per coin, at
// most MaxPiconero). Any other asset is ErrAsset, checked first. It is the one place the formula is written: with
// P and R the units of price and rate,
//
//	num = P × 10^scale × (10000 + marginBps), den = R × 10000, amount = ceil(num / den)
//
// (the decimals' own scales cancel). The ceiling never underprices, and a positive result is at least 1. The checks
// run in this order: ErrAsset, ErrZero when price or rate is zero, ErrMargin when marginBps is outside MinMarginBps
// to MaxMarginBps, ErrRange when the amount exceeds the asset's bound. All arithmetic is on big integers; the result
// becomes an int64 only after the bound check.
func Convert(asset string, price, rate Decimal, marginBps int64) (int64, error) {
	switch asset {
	case AssetBTC:
		return baseUnits(price, rate, marginBps, satsPerCoin, bigMaxSats)
	case AssetXMR:
		return baseUnits(price, rate, marginBps, piconeroPerCoin, bigMaxPiconero)
	}
	return 0, ErrAsset
}

// Sats is Convert(AssetBTC, …): a price in satoshis.
func Sats(price, rate Decimal, marginBps int64) (int64, error) {
	return Convert(AssetBTC, price, rate, marginBps)
}

// Piconero is Convert(AssetXMR, …): a price in piconero.
func Piconero(price, rate Decimal, marginBps int64) (int64, error) {
	return Convert(AssetXMR, price, rate, marginBps)
}

// baseUnits is Convert's formula for scale base units per coin, at most bound.
func baseUnits(price, rate Decimal, marginBps int64, scale, bound *big.Int) (int64, error) {
	if price.IsZero() || rate.IsZero() {
		return 0, ErrZero
	}
	if marginBps < MinMarginBps || marginBps > MaxMarginBps {
		return 0, ErrMargin
	}
	factor := new(big.Int).Add(bpsPerOne, big.NewInt(marginBps))
	num := new(big.Int).Mul(price.units, scale)
	num.Mul(num, factor)
	den := new(big.Int).Mul(rate.units, bpsPerOne)
	num.Add(num, den)
	num.Sub(num, bigOne)
	n := num.Quo(num, den)
	if n.Cmp(bound) > 0 {
		return 0, ErrRange
	}
	return n.Int64(), nil
}
