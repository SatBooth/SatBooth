package setup

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// The link format is from getAlby/hub 921003b frontend/src/screens/apps/NewApp.tsx:90-136 and 225-230:
// request_methods and notification_types are space-separated, max_amount is in msat (the hub floors it
// /1000 into maxAmountSat) and budget_renewal is one of daily|weekly|monthly|yearly|never.
const (
	// Both links list get_info: the hub gives the node's network in a get_info answer only to a connection that holds
	// that scope, and SatBooth refuses at start a connection that does not state the network nwc.network names.
	invoiceMethods = "make_invoice lookup_invoice list_transactions get_info"
	payoutMethods  = "pay_invoice lookup_invoice get_info"
	maxAmountSat   = MaxBudgetSat
)

// MaxBudgetSat is the largest payout budget in sats: its msat value must fit an int64.
const MaxBudgetSat = math.MaxInt64 / 1000

// ErrHubURL is returned for a hub URL that is not an http or https URL with a host and no query or fragment.
var ErrHubURL = errors.New("setup: the hub URL must be an http or https URL with a host and no query or fragment")

// ParseHub accepts an http or https URL with a host and no userinfo, query or fragment, and trims one trailing "/".
func ParseHub(s string) (string, error) {
	if s == "" || strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r == 0x7f }) || strings.ContainsAny(s, "?#") {
		return "", ErrHubURL
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return "", ErrHubURL
	}
	s = strings.TrimSuffix(s, "/")
	if strings.HasSuffix(s, "/") {
		return "", ErrHubURL
	}
	return s, nil
}

// InvoiceLink and PayoutLink return the hub's new-connection link for a client public key (64 lowercase hex).
// base is the hub's URL as satbooth-pair's -hub takes it. maxSat is 1..math.MaxInt64/1000.
func InvoiceLink(base, pubkey, name string) (string, error) {
	q := url.Values{}
	q.Set("name", name)
	q.Set("request_methods", invoiceMethods)
	q.Set("notification_types", "payment_received")
	return link(base, pubkey, q)
}

func PayoutLink(base, pubkey, name string, maxSat uint64) (string, error) {
	if maxSat == 0 || maxSat > maxAmountSat {
		return "", fmt.Errorf("setup: the payout budget must be between 1 and %d sats", uint64(maxAmountSat))
	}
	q := url.Values{}
	q.Set("name", name)
	q.Set("request_methods", payoutMethods)
	q.Set("max_amount", strconv.FormatUint(maxSat*1000, 10))
	q.Set("budget_renewal", "never")
	return link(base, pubkey, q)
}

func link(base, pubkey string, q url.Values) (string, error) {
	b, err := ParseHub(base)
	if err != nil {
		return "", err
	}
	if !isHex(pubkey, 64) {
		return "", errors.New("setup: the client public key must be 64 lowercase hex characters")
	}
	q.Set("pubkey", pubkey)
	return b + "/apps/new?" + q.Encode(), nil
}
