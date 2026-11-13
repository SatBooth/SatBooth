// Command satbooth-setup sets up the two hosts of a SatBooth deployment in four stages. It generates every secret,
// writes every configuration file, never overwrites anything, opens no network connection and runs no other
// program. It prints commands for the operator to run, never a secret, a descriptor or a Monero address.
//
//	satbooth-setup public init   -dir <dir> [-host-dir <path>] [-answers <file>] [-hub-url http://127.0.0.1:8080]
//	satbooth-setup hub init      -dir <dir> [-host-dir <path>] -handoff <file> [-digest <digest>] [-answers <file>]
//	satbooth-setup hub pair      -dir <dir> [-invoice-wallet <hex>] [-payout-wallet <hex>]
//	satbooth-setup public finish -dir <dir> -handoff <file> [-digest <digest>] [-wallet-rpc-pass <file>]
//	any stage: -discard-incomplete
package main

import (
	"bufio"
	crand "crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/satbooth/satbooth/nostr"
	"github.com/satbooth/satbooth/setup"
)

// randSource is the secrets' source; tests may replace it.
var randSource io.Reader = crand.Reader

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

const usageText = `usage:
  satbooth-setup public init   -dir <dir> [-host-dir <path>] [-answers <file>] [-hub-url http://127.0.0.1:8080]
  satbooth-setup hub init      -dir <dir> [-host-dir <path>] -handoff <file> [-digest <digest>] [-answers <file>]
  satbooth-setup hub pair      -dir <dir> [-invoice-wallet <hex>] [-payout-wallet <hex>]
  satbooth-setup public finish -dir <dir> -handoff <file> [-digest <digest>] [-wallet-rpc-pass <file>]
  any stage: -discard-incomplete (remove what an unfinished run of the stage left)`

// run returns the exit code: 0 done, 2 usage, 1 anything else. Standard input answers the questions; the prompts go
// to stderr and never repeat what was typed.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	usage := func(msg string) int {
		fmt.Fprintln(stderr, "satbooth-setup:", msg)
		fmt.Fprintln(stderr, usageText)
		return 2
	}
	if len(args) < 2 {
		return usage("a host and a stage are required")
	}
	host, stage := args[0], args[1]
	fs := flag.NewFlagSet("satbooth-setup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("dir", "", "")
	discard := fs.Bool("discard-incomplete", false, "")
	var answers, hubURL, hostDir, handoff, digest, invoiceW, payoutW, walletPass *string
	var st setup.Stage
	str := func(name string) *string { return fs.String(name, "", "") }
	switch host + " " + stage {
	case "public init":
		answers, hubURL, hostDir = str("answers"), str("hub-url"), str("host-dir")
		st = setup.StagePublicInit
	case "hub init":
		answers, hostDir, handoff, digest = str("answers"), str("host-dir"), str("handoff"), str("digest")
		st = setup.StageHubInit
	case "hub pair":
		invoiceW, payoutW = str("invoice-wallet"), str("payout-wallet")
		st = setup.StageHubPair
	case "public finish":
		handoff, digest, walletPass = str("handoff"), str("digest"), str("wallet-rpc-pass")
		st = setup.StagePublicDone
	default:
		return usage("unknown stage")
	}
	if err := fs.Parse(args[2:]); err != nil {
		return usage("bad flags")
	}
	if fs.NArg() > 0 {
		return usage("unexpected argument")
	}
	given := map[string]bool{} // a flag given with an empty value is not an absent one
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if *dir == "" {
		return usage("-dir is required")
	}
	for _, name := range []string{"host-dir", "hub-url", "answers", "wallet-rpc-pass", "digest"} {
		if given[name] && fs.Lookup(name).Value.String() == "" {
			return usage("-" + name + " is given with an empty value")
		}
	}
	if handoff != nil && *handoff == "" && !*discard {
		return usage("-handoff is required")
	}
	if *discard {
		lock, err := setup.LockDir(*dir)
		if err != nil {
			return fail(stderr, err)
		}
		defer lock.Close()
		removed, kept, err := lock.Discard(st)
		if err != nil {
			return fail(stderr, err)
		}
		for _, n := range removed {
			fmt.Fprintln(stdout, "removed", n)
		}
		for _, n := range kept {
			fmt.Fprintln(stdout, "kept", n)
		}
		if len(kept) > 0 {
			fmt.Fprintln(stdout, "Each kept file was not written by the unfinished run (it has other content): "+
				"it is still there, and the stage will not run until you move or delete it.")
		}
		fmt.Fprintln(stdout, "The unfinished run is discarded.")
		return 0
	}
	// Everything that can be refused without an answer is refused before the first question.
	if hostDir != nil && *hostDir != "" {
		if err := setup.CheckHostDir(*hostDir); err != nil {
			return fail(stderr, err)
		}
	}
	url := "http://127.0.0.1:8080"
	if hubURL != nil && *hubURL != "" {
		url = *hubURL
	}
	if hubURL != nil {
		if _, err := setup.ParseHub(url); err != nil {
			return fail(stderr, err)
		}
	}
	// The directory is locked here, before the first question, and the lock is kept through the answers and the stage:
	// a second run on it is refused before it asks anything.
	lock, err := setup.LockDir(*dir)
	if err != nil {
		return fail(stderr, err)
	}
	defer lock.Close()
	if err := lock.Preflight(st); err != nil {
		return fail(stderr, err)
	}
	in := newPrompter(stdin, stderr)

	var rep *setup.Report
	switch host + " " + stage {
	case "public init":
		var a *setup.PublicAnswers
		if a, err = publicAnswers(*answers, *hostDir, in); err != nil {
			return fail(stderr, err)
		}
		rep, err = lock.PublicInit(a, url, randSource)
	case "hub init":
		var h []byte
		var a *setup.HubAnswers
		if h, err = readHandoff(*handoff); err != nil {
			return fail(stderr, err)
		}
		if a, err = hubAnswers(*answers, *hostDir, in); err != nil {
			return fail(stderr, err)
		}
		d := *digest
		if !given["digest"] {
			if d, err = in.digest(); err != nil {
				return fail(stderr, err)
			}
		}
		rep, err = lock.HubInit(a, h, d, randSource)
	case "hub pair":
		inv, pay := *invoiceW, *payoutW
		if !given["invoice-wallet"] {
			if inv, err = in.wallet("invoice Wallet Public Key (64 lowercase hex characters)", false); err != nil {
				return fail(stderr, err)
			}
		}
		if !given["payout-wallet"] {
			if pay, err = in.wallet("payout Wallet Public Key (empty when refunds are off)", true); err != nil {
				return fail(stderr, err)
			}
		}
		rep, err = lock.HubPair(inv, pay)
	case "public finish":
		var h, pass []byte
		if h, err = readHandoff(*handoff); err != nil {
			return fail(stderr, err)
		}
		if *walletPass != "" {
			if pass, err = readPassFile(*walletPass); err != nil {
				return fail(stderr, err)
			}
		}
		d := *digest
		if !given["digest"] {
			if d, err = in.digest(); err != nil {
				return fail(stderr, err)
			}
		}
		rep, err = lock.PublicFinish(h, d, pass)
	}
	if err != nil {
		return fail(stderr, err)
	}
	printReport(stdout, host+" "+stage, rep)
	return 0
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "satbooth-setup:", err)
	return 1
}

func printReport(w io.Writer, stage string, r *setup.Report) {
	if len(r.Links) > 0 {
		fmt.Fprintln(w, "Pairing links (the invoice link, then the payout link with refunds):")
		for _, l := range r.Links {
			fmt.Fprintln(w, " ", l)
		}
		fmt.Fprintln(w)
	}
	if r.Digest != "" {
		what := "Digest of the handoff file this stage wrote"
		if stage == "hub init" || stage == "public finish" {
			what = "Digest of the handoff file this stage read (it matched the one you gave)"
		}
		fmt.Fprintf(w, "%s:\n  %s\n\n", what, r.Digest)
	}
	fmt.Fprintln(w, "Next:")
	for i, n := range r.Next {
		fmt.Fprintf(w, "  %d. %s\n", i+1, n)
	}
}

// readHandoff reads a handoff file: a regular file, at most 4097 bytes (the parser refuses one above 4096).
func readHandoff(p string) ([]byte, error) {
	return readFile(p, 4097, false)
}

// readPassFile reads the carried wallet-rpc password: a regular file that only its owner can access.
func readPassFile(p string) ([]byte, error) {
	return readFile(p, 129, true)
}

func readFile(p string, max int64, private bool) ([]byte, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("cannot open a file given on the command line")
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, errors.New("a file given on the command line is not a regular file")
	}
	if private && fi.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("the carried password file must be mode 0600 (no group or other access)")
	}
	b, err := io.ReadAll(io.LimitReader(f, max))
	if err != nil {
		return nil, errors.New("cannot read a file given on the command line")
	}
	return b, nil
}

// prompter asks on stderr and reads standard input line by line.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
}

func newPrompter(in io.Reader, out io.Writer) *prompter {
	return &prompter{in: bufio.NewReader(in), out: out}
}

var errEOF = errors.New("the input ended before every question was answered")

// line prints the question and reads one line, without its line ending.
func (p *prompter) line(question string) (string, error) {
	fmt.Fprint(p.out, question+" ")
	s, err := p.in.ReadString('\n')
	if err != nil && (err != io.EOF || s == "") {
		fmt.Fprintln(p.out)
		return "", errEOF
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// wallet asks for a Wallet Public Key until it is one (or, when optional, empty).
func (p *prompter) wallet(what string, optional bool) (string, error) {
	for {
		s, err := p.line("The hub's " + what + ":")
		if err != nil {
			return "", err
		}
		s = strings.TrimSpace(s)
		if s == "" && optional {
			return "", nil
		}
		if setup.IsHex(s, 64) && nostr.ValidatePubKey(s) == nil {
			return s, nil
		}
		fmt.Fprintln(p.out, "  Refused: a Wallet Public Key is 64 lowercase hex characters and a valid public key.")
	}
}

// digest asks for the digest the other host printed, until it has the shape of one.
func (p *prompter) digest() (string, error) {
	for {
		s, err := p.line("The digest the other host printed (eight groups of eight hex characters):")
		if err != nil {
			return "", err
		}
		if setup.IsHex(strings.ReplaceAll(s, " ", ""), 64) {
			return s, nil
		}
		fmt.Fprintln(p.out, "  Refused: a digest is 64 lowercase hex characters, in groups or not.")
	}
}
