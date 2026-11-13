// Command satbooth-pair is the operator's pairing tool. It creates (or reuses) SatBooth's NWC client key in a
// 0600 file and prints a link to the Alby Hub's own "new connection" page with the public key, name, methods
// and budget filled in. The secret key never leaves the file: it is not printed, not sent to the hub and not
// part of the link.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"

	"github.com/satbooth/satbooth/nostr"
	"github.com/satbooth/satbooth/setup"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run returns the exit code: 0 on success, 2 for a usage error, 1 for any other failure. No path writes the
// secret key to stdout or stderr: the key is only ever handed to nostr.WriteNewSecretKeyFile, and errors
// from the nostr package never contain it.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("satbooth-pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	keyPath := fs.String("key", "", "path of SatBooth's NWC client key file (created with mode 0600 if missing)")
	hub := fs.String("hub", "", "base URL of the Alby Hub, such as https://hub.example")
	conn := fs.String("connection", "", "invoice (receive only) or payout (pay_invoice with a budget)")
	name := fs.String("name", "", "connection name shown in the hub (default \"SatBooth invoice\" or \"SatBooth payout\")")
	amount := fs.String("max-amount-sat", "", "payout budget in sats, never renewed (required for payout, refused for invoice)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	usage := func(msg string) int {
		fmt.Fprintln(stderr, "satbooth-pair:", msg)
		fmt.Fprintln(stderr, "usage: satbooth-pair -key <path> -hub <url> -connection invoice|payout [-name <text>] [-max-amount-sat <n>]")
		return 2
	}
	if fs.NArg() != 0 {
		return usage("unexpected argument")
	}
	if *keyPath == "" {
		return usage("-key is required")
	}
	base, err := parseHub(*hub)
	if err != nil {
		return usage(err.Error())
	}

	var n uint64
	switch *conn {
	case "invoice":
		if *amount != "" {
			return usage("-max-amount-sat is only for -connection payout")
		}
	case "payout":
		// Without a budget the hub would apply its own default; a payout connection must carry an explicit,
		// non-zero cap that the operator chose.
		var err error
		n, err = strconv.ParseUint(*amount, 10, 63)
		if err != nil || n == 0 || n > setup.MaxBudgetSat {
			return usage(fmt.Sprintf("-connection payout needs -max-amount-sat between 1 and %d", uint64(setup.MaxBudgetSat)))
		}
	default:
		return usage("-connection must be invoice or payout")
	}

	key, err := loadOrCreate(*keyPath)
	if err != nil {
		fmt.Fprintln(stderr, "satbooth-pair:", err)
		return 1
	}
	pub := key.PublicKeyHex()
	var link string
	if *conn == "invoice" {
		link, err = setup.InvoiceLink(base, pub, orDefault(*name, "SatBooth invoice"))
	} else {
		link, err = setup.PayoutLink(base, pub, orDefault(*name, "SatBooth payout"), n)
	}
	if err != nil {
		fmt.Fprintln(stderr, "satbooth-pair:", err)
		return 1
	}

	fmt.Fprintf(stdout, "client public key: %s\n", pub)
	fmt.Fprintln(stdout, link)
	fmt.Fprintln(stdout, "Open the link in the hub and check the methods and the budget. "+setup.TickSentence+" Click Connect, then copy "+
		"\"Wallet Public Key\" from the connection's \"Connection Details\" into SatBooth's configuration and into the relay's pairs.")
	return 0
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// loadOrCreate loads the key file, or generates and writes a new one when nothing exists at the path. A
// dangling symlink counts as existing: Lstat succeeds, LoadSecretKeyFile refuses it and nothing is written.
func loadOrCreate(path string) (*nostr.SecretKey, error) {
	if _, err := os.Lstat(path); err == nil {
		return nostr.LoadSecretKeyFile(path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("cannot inspect key file: %w", err)
	}
	k, err := nostr.GenerateSecretKey()
	if err != nil {
		return nil, err
	}
	if err := nostr.WriteNewSecretKeyFile(path, k); err != nil {
		return nil, err
	}
	return k, nil
}

// parseHub accepts an http or https URL with a host and no userinfo, query or fragment, and trims one
// trailing "/".
func parseHub(s string) (string, error) {
	h, err := setup.ParseHub(s)
	if err != nil {
		return "", errors.New("-hub must be an http or https URL with a host and no query or fragment")
	}
	return h, nil
}
