package setup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Stage is the stage a host has reached, as setup/state.json records it.
type Stage string

const (
	StageNone       Stage = ""
	StagePublicInit Stage = "public-init"
	StagePublicDone Stage = "public-finish"
	StageHubInit    Stage = "hub-init"
	StageHubPair    Stage = "hub-pair"
)

// Report is what a stage hands to the command to print. It never holds a secret, a descriptor or a Monero address.
type Report struct {
	Digest string   // of the handoff this stage wrote or read; "" when none
	Links  []string // stage 1: the invoice link, then the payout link (if refunds)
	Next   []string // the operator's next steps, one per line
}

var (
	ErrWrongStage = errors.New("setup: this stage does not follow the one recorded in setup/state.json")
	ErrExists     = errors.New("setup: a file this stage writes already exists") // wrapped with the names
	ErrIncomplete = errors.New("setup: an earlier run of this stage did not finish; run it with -discard-incomplete")
	ErrNotAStage  = errors.New("setup: this directory was set up for the other host")
	ErrDigest     = errors.New("setup: the handoff file's digest is not the one given; nothing was written")
	ErrMismatch   = errors.New("setup: the two hosts were given different values") // wrapped with the field's name
)

// The names inside the base directory. Every name is relative to it and slash-separated; everything the stages
// and Discard do with them goes through one os.Root opened on the base directory, so no name resolves outside it.
const (
	setupDir    = "setup"
	stateName   = "setup/state.json"
	stateTmp    = "setup/state.json.tmp"
	journalName = "setup/journal.json"
	journalTmp  = "setup/journal.json.tmp"
)

const (
	hostPublic = "public"
	hostHub    = "hub"
)

// IsHex reports whether s is exactly n lowercase hex characters.
func IsHex(s string, n int) bool { return isHex(s, n) }

// CheckHostDir is the rule for the answers' host_dir, for the command's -host-dir flag.
func CheckHostDir(s string) error { return checkHostDir(s) }

// stepHook and discardHook are for tests only. stepHook is called after the journal is in place, after each file is
// written to the temporary directory, after each file is linked into place and after the new state is in place;
// discardHook after each removal of Discard. An error from either stops the work where it is, as a crash would.
var (
	stepHook    func(step int) error
	discardHook func() error
)

// spec is what one stage may create: the one set of names its journal may hold.
type spec struct {
	host  string
	prev  Stage
	dirs  []string // directories the stage creates (a first stage creates setup/ itself)
	into  []string // directories a later stage writes into: they must exist, owned by the user, mode 0700
	files []string // every file the stage may create, with the ones only an option needs
}

var specs = map[Stage]*spec{
	StagePublicInit: {host: hostPublic, prev: StageNone,
		dirs: []string{"setup", "etc", "backups", "setup/api-keys"},
		files: []string{stateName, stateTmp, "etc/nwc-client.key", "etc/payout.key", "etc/webhook.secret",
			"etc/alerts.secret", "etc/bitcoind.pass", "etc/api-keys.json", "setup/public-to-hub.json",
			"setup/api-keys/orders-write.key", "setup/api-keys/orders-read.key", "setup/api-keys/refunds-write.key",
			"setup/api-keys/refunds-read.key"}},
	StageHubInit: {host: hostHub, prev: StageNone,
		dirs: []string{"setup", "config", "wallet", "config/wallet-rpc", "setup/carry"},
		files: []string{stateName, stateTmp, "config/hub.env", "config/fulcrum.conf", "config/bitcoin.conf",
			"config/monerod.conf", "config/wallet-rpc/rpc-login", "config/wallet-rpc/wallet.pass",
			"setup/carry/wallet-rpc.pass", "setup/read-public-to-hub.json", "compose.env"}},
	StageHubPair: {host: hostHub, prev: StageHubInit, into: []string{"setup", "config"},
		files: []string{stateTmp, "config/relay.json", "setup/hub-to-public.json"}},
	StagePublicDone: {host: hostPublic, prev: StagePublicInit, into: []string{"setup", "etc"},
		files: []string{stateTmp, "etc/satbooth.json", "etc/wallet-rpc.pass", "setup/read-hub-to-public.json", "compose.env"}},
}

func (s *spec) first() bool { return s.prev == StageNone }

func tmpDirName(s Stage) string { return path.Join(setupDir, "tmp-"+string(s)) }

// maxTmpFiles bounds the temporary names a journal may hold.
const maxTmpFiles = 64

// allowsDir and allowsFile report whether a journal of the stage may name it.
func (s *spec) allowsDir(stage Stage, n string) bool {
	return n == tmpDirName(stage) || slices.Contains(s.dirs, n)
}

func (s *spec) allowsFile(stage Stage, n string) bool {
	if slices.Contains(s.files, n) {
		return true
	}
	rest, ok := strings.CutPrefix(n, tmpDirName(stage)+"/")
	if !ok {
		return false
	}
	i, err := strconv.Atoi(rest)
	return err == nil && i >= 0 && i < maxTmpFiles && strconv.Itoa(i) == rest
}

// state is setup/state.json: the stage reached, the answers as given and the public values later stages need. It
// never holds a secret. It does hold the descriptor and the Monero address (the operator's answers): the file is
// 0600 in the setup directory, which no service mounts, and nothing prints it.
type state struct {
	Version       int            `json:"version"`
	Host          string         `json:"host"`
	Stage         Stage          `json:"stage"`
	Public        *PublicAnswers `json:"public_answers,omitempty"`
	HubURL        string         `json:"hub_url,omitempty"`
	InvoiceClient string         `json:"invoice_client,omitempty"`
	PayoutClient  string         `json:"payout_client,omitempty"`
	Hub           *HubAnswers    `json:"hub_answers,omitempty"`
	Handoff       *PublicToHub   `json:"handoff,omitempty"`
}

func (s *state) marshal() []byte {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		panic("setup: " + err.Error())
	}
	return append(b, '\n')
}

// maxState bounds state.json and journal.json.
const maxState = 64 << 10

// stateWire is state.json as it is read: the nested objects stay raw, so that each goes through its own strict
// parser (an answers file, a handoff) and nothing in them is read loosely.
type stateWire struct {
	Version       int             `json:"version"`
	Host          string          `json:"host"`
	Stage         Stage           `json:"stage"`
	Public        json.RawMessage `json:"public_answers"`
	HubURL        string          `json:"hub_url"`
	InvoiceClient string          `json:"invoice_client"`
	PayoutClient  string          `json:"payout_client"`
	Hub           json.RawMessage `json:"hub_answers"`
	Handoff       json.RawMessage `json:"handoff"`
}

// parseState reads state.json strictly (one object, exact keys, each at most once, nothing after it) and
// revalidates what it holds with the parsers of the answers and the handoff.
func parseState(b []byte) (*state, error) {
	bad := errors.New("setup: setup/state.json is not a state file of this version")
	var w stateWire
	if err := strictWalk(b, &w, maxState, []string{"version", "host", "stage"},
		[]string{"public_answers", "hub_url", "invoice_client", "payout_client", "hub_answers", "handoff"}, nil); err != nil || w.Version != 1 {
		return nil, bad
	}
	s := state{Version: 1, Host: w.Host, Stage: w.Stage, HubURL: w.HubURL, InvoiceClient: w.InvoiceClient, PayoutClient: w.PayoutClient}
	switch {
	case s.Host == hostPublic && (s.Stage == StagePublicInit || s.Stage == StagePublicDone) && w.Public != nil && w.Hub == nil && w.Handoff == nil:
		a, err := ParsePublicAnswers(w.Public)
		if err != nil || !isHex(s.InvoiceClient, 64) || (s.PayoutClient != "") != (len(a.ApprovalKeys) > 0) ||
			(s.PayoutClient != "" && !isHex(s.PayoutClient, 64)) {
			return nil, bad
		}
		if _, err := ParseHub(s.HubURL); err != nil {
			return nil, bad
		}
		s.Public = a
	case s.Host == hostHub && (s.Stage == StageHubInit || s.Stage == StageHubPair) && w.Hub != nil && w.Handoff != nil && w.Public == nil &&
		s.HubURL == "" && s.InvoiceClient == "" && s.PayoutClient == "":
		a, err := ParseHubAnswers(w.Hub)
		if err != nil {
			return nil, bad
		}
		h, err := ParsePublicToHub(w.Handoff)
		if err != nil || a.CheckAgainst(h) != nil {
			return nil, bad
		}
		s.Hub, s.Handoff = a, h
	default:
		return nil, bad
	}
	return &s, nil
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("setup: " + err.Error())
	}
	return b
}

// journal is setup/journal.json: what a run that has not finished may have created, and the exact bytes of the
// state file it replaces (null for a first stage).
type journal struct {
	Version   int      `json:"version"`
	Stage     Stage    `json:"stage"`
	Dirs      []string `json:"dirs"`
	Files     []string `json:"files"`
	SHA256    []string `json:"sha256"` // of the bytes the run writes to the file of the same index: 64 lowercase hex
	PrevState *string  `json:"prev_state"`
}

var errBadJournal = errors.New("setup: setup/journal.json is empty or is not a journal of this stage's host and version: " +
	"inspect it and delete it by hand; nothing was removed")

// parseJournal reads a journal strictly: one object of this version, a known stage, only names that stage may
// create (and among them setup/state.json.tmp, which every journal the writer produces lists), and a prev_state that
// is this host's state at the stage's predecessor (null exactly for a first stage).
func parseJournal(raw []byte) (*journal, error) {
	var j journal
	if err := strictWalk(raw, &j, maxState, []string{"version", "stage", "dirs", "files", "sha256", "prev_state"}, nil,
		[]string{"prev_state"}); err != nil || j.Version != 1 {
		return nil, errBadJournal
	}
	sp := specs[j.Stage]
	if sp == nil {
		return nil, errBadJournal
	}
	for _, n := range j.Dirs {
		if !sp.allowsDir(j.Stage, n) {
			return nil, errBadJournal
		}
	}
	if len(j.SHA256) != len(j.Files) {
		return nil, errBadJournal
	}
	for i, n := range j.Files {
		if !sp.allowsFile(j.Stage, n) || !isHex(j.SHA256[i], 64) || j.SHA256[i] == sumHex(nil) { // no file written is empty
			return nil, errBadJournal
		}
	}
	if !slices.Contains(j.Files, stateTmp) { // the writer always journals the state file's scratch name
		return nil, errBadJournal
	}
	if sp.first() != (j.PrevState == nil) {
		return nil, errBadJournal
	}
	if j.PrevState != nil {
		prev, err := parseState([]byte(*j.PrevState))
		if err != nil || prev.Host != sp.host || prev.Stage != sp.prev {
			return nil, errBadJournal
		}
	}
	return &j, nil
}

// LockDir opens the base directory, checks the object it opened and takes its lock (ErrBusy when another run holds
// it). The object is a real directory (not a symbolic link: the path's own Lstat must be the same file as the root's)
// owned by the effective user with mode exactly 0700. The root is opened first, so nothing is checked on one object and
// used on another. Everything else a stage does goes through the returned Lock's root.
func LockDir(dir string) (*Lock, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errors.New("setup: the base directory (-dir) does not exist or is not a real directory")
	}
	ri, rerr := root.Stat(".")
	li, lerr := os.Lstat(dir)
	if rerr != nil || lerr != nil || !ri.IsDir() || !li.IsDir() || !os.SameFile(li, ri) {
		root.Close()
		return nil, errors.New("setup: the base directory (-dir) does not exist or is not a real directory")
	}
	if !ownedBy(ri) {
		root.Close()
		return nil, errors.New("setup: the base directory (-dir) belongs to another user: give it back to the user running " +
			"this command (chown), or use a fresh directory")
	}
	if ri.Mode().Perm() != 0o700 {
		root.Close()
		return nil, errors.New("setup: the base directory (-dir) must have mode 0700: run chmod 700 on it")
	}
	lock, err := lockBase(root)
	if err != nil {
		root.Close()
		return nil, err
	}
	return &Lock{root: root, lock: lock}, nil
}

// ErrBusy is returned when another satbooth-setup run holds the base directory.
var ErrBusy = errors.New("setup: another satbooth-setup run is using this directory")

// Lock is a base directory, opened and checked, with the exclusive lock one run holds on it until Close. The stages,
// Preflight and Discard are its methods and work only through its own root, so nothing runs on a directory the run
// does not hold. The lock is the process-independent flock of the Lock's own descriptor: a second LockDir of the same
// directory, from this process or another, is refused with ErrBusy.
type Lock struct {
	root   *os.Root // unexported: a caller gets the stages and Close, not the os.Root API
	lock   *os.File
	closed bool
}

// ErrClosed is returned by a method of a Lock that was closed; nothing is touched.
var ErrClosed = errors.New("setup: the directory lock is closed")

// errRelease is the one error Close returns: the operating system's own would carry the directory's path.
var errRelease = errors.New("setup: cannot release the directory lock")

// Close releases the lock and closes the root. A second Close does nothing and returns nil.
func (l *Lock) Close() error {
	if l.closed {
		return nil
	}
	l.closed = true
	lerr := l.lock.Close() // closing the descriptor drops the advisory lock
	if err := l.root.Close(); err != nil || lerr != nil {
		return errRelease
	}
	return nil
}

// lockBase takes the one lock of a base directory: an exclusive, non-blocking advisory lock (flock) on a descriptor of
// the directory itself, held until the run closes it, so a second stage or discard on the same directory is refused
// before it reads or writes anything. No lock file is made: nothing new appears in the directory. The descriptor is
// opened through root, so it is the object that was checked.
func lockBase(root *os.Root) (*os.File, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, errors.New("setup: cannot lock the directory: " + errReason(err))
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	switch {
	case err == nil:
		return f, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		f.Close()
		return nil, ErrBusy
	}
	f.Close()
	return nil, errors.New("setup: cannot lock the directory: " + errReason(err))
}

// errReason is the operating system's reason of err without any path.
func errReason(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return err.Error()
}

func ownedBy(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}

// checkDir requires name to be a real directory owned by the user with mode exactly 0700.
func checkDir(root *os.Root, name string) error {
	fi, err := root.Lstat(name)
	switch {
	case err != nil && errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("setup: %s does not exist: set up from a fresh directory", name)
	case err != nil || !fi.IsDir():
		return fmt.Errorf("setup: %s must be a real directory (not a symbolic link): set up from a fresh directory", name)
	case !ownedBy(fi):
		return fmt.Errorf("setup: %s belongs to another user: give it back to the user running this command (chown), "+
			"or set up from a fresh directory", name)
	case fi.Mode().Perm() != 0o700:
		return fmt.Errorf("setup: %s must have mode 0700: run chmod 700 on it, or set up from a fresh directory", name)
	}
	return nil
}

// lexists reports whether anything, a symbolic link included, is at name; an error other than "not there" (a parent
// that is not a directory) counts as something being in the way.
func lexists(root *os.Root, name string) bool {
	_, err := root.Lstat(name)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}

// readSmall reads a regular file of at most 64 KiB, refusing a symbolic link (the open does not follow one, and the
// opened file is checked).
func readSmall(root *os.Root, name string) ([]byte, error) {
	fi, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi2, err := f.Stat()
	if err != nil || !fi2.Mode().IsRegular() || !os.SameFile(fi, fi2) {
		return nil, errors.New("not the regular file that was inspected")
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	if err != nil || len(b) > 64<<10 {
		return nil, errors.New("unreadable or too large")
	}
	return b, nil
}

func notThere(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// Preflight runs the checks every stage starts with and nothing else: the base directory, the recorded stage, a
// journal left behind, and the directories the stage writes into. It changes nothing; the command calls it before
// it asks a question.
func (l *Lock) Preflight(stage Stage) error {
	_, _, err := l.begin(stage)
	return err
}

// begin does the first checks of every stage and returns the state (nil for a first stage) and its raw bytes, which
// the journal keeps. It changes nothing.
func (l *Lock) begin(stage Stage) (*state, []byte, error) {
	if l.closed {
		return nil, nil, ErrClosed
	}
	if specs[stage] == nil {
		return nil, nil, errors.New("setup: unknown stage")
	}
	return check(l.root, stage)
}

// check is the reading part of begin, on the Lock's root.
func check(root *os.Root, stage Stage) (st *state, raw []byte, err error) {
	sp := specs[stage]
	// A later stage reads and writes only in directories that are the user's own, 0700 and not links: setup/ is
	// checked before anything is read through it, the others once the stage is known to follow.
	if !sp.first() && lexists(root, setupDir) {
		if err = checkDir(root, setupDir); err != nil {
			return nil, nil, err
		}
	}
	if fi, e := root.Lstat(setupDir); e == nil && !fi.IsDir() && sp.first() { // a link or a file where setup/ would be made
		return nil, nil, fmt.Errorf("%w: %s", ErrExists, setupDir)
	}
	raw, rerr := readSmall(root, stateName)
	switch {
	case notThere(rerr):
	case rerr != nil:
		if sp.first() && lexists(root, stateName) {
			return nil, nil, fmt.Errorf("%w: %s", ErrExists, stateName)
		}
		return nil, nil, errors.New("setup: cannot read setup/state.json")
	default:
		if st, err = parseState(raw); err != nil {
			if sp.first() { // something else is at setup/state.json: in the way of the first stage
				err = fmt.Errorf("%w: %s", ErrExists, stateName)
			}
			return nil, nil, err
		}
		if st.Host != sp.host {
			return nil, nil, ErrNotAStage
		}
	}
	got := StageNone
	if st != nil {
		got = st.Stage
	}
	if jraw, jerr := readSmall(root, journalName); !notThere(jerr) {
		// A journal wins over a wrong stage: a run that died after its state was replaced looks like one.
		if jerr != nil {
			return nil, nil, fmt.Errorf("%w (setup/journal.json is unreadable: inspect it and delete it by hand)", ErrIncomplete)
		}
		j, perr := parseJournal(jraw)
		switch {
		case perr != nil:
			return nil, nil, fmt.Errorf("%w (setup/journal.json is empty or not a journal: inspect it and delete it by hand)", ErrIncomplete)
		case j.Stage != stage:
			return nil, nil, fmt.Errorf("%w (the unfinished run is of stage %s)", ErrIncomplete, j.Stage)
		}
		return nil, nil, ErrIncomplete
	}
	if got != sp.prev {
		return nil, nil, ErrWrongStage
	}
	for _, d := range sp.into {
		if err = checkDir(root, d); err != nil {
			return nil, nil, err
		}
	}
	if sp.first() {
		var clash []string
		for _, d := range sp.dirs {
			if lexists(root, d) && !(d == setupDir && onlyLeftoverJournal(root)) {
				clash = append(clash, d)
			}
		}
		if len(clash) > 0 {
			return nil, nil, existsErr(root, clash)
		}
		return nil, nil, nil
	}
	return st, raw, nil
}

// existsErr is ErrExists for the names in the way. An empty setup/ is the one a crash can leave behind: say so.
func existsErr(root *os.Root, clash []string) error {
	err := fmt.Errorf("%w: %s", ErrExists, strings.Join(clash, ", "))
	if slices.Contains(clash, setupDir) && emptyDir(root, setupDir) {
		err = fmt.Errorf("%w (an empty setup/ was left behind by an interrupted run or discard: remove it with rmdir "+
			"and run the stage again)", err)
	}
	return err
}

// emptyDir reports whether name is a real directory with no entry.
func emptyDir(root *os.Root, name string) bool {
	fi, err := root.Lstat(name)
	if err != nil || !fi.IsDir() {
		return false
	}
	d, err := root.Open(name)
	if err != nil {
		return false
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	return err == nil && len(names) == 0
}

// onlyLeftoverJournal reports whether setup/ holds exactly a journal.json.tmp.
func onlyLeftoverJournal(root *os.Root) bool {
	d, err := root.Open(setupDir)
	if err != nil {
		return false
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	return err == nil && len(names) == 1 && names[0] == "journal.json.tmp"
}

// clearLeftoverJournal removes a journal.json.tmp (no journal.json exists): nothing else was written before the
// rename that puts the journal in place, except setup/ itself when a first stage made it.
func clearLeftoverJournal(root *os.Root, sp *spec) error {
	fi, err := root.Lstat(journalTmp)
	if err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: %s", ErrExists, journalTmp)
	}
	first := sp.first() && onlyLeftoverJournal(root)
	if err := root.Remove(journalTmp); err != nil {
		return err
	}
	if first {
		if err := root.Remove(setupDir); err != nil {
			return err
		}
		return syncDir(root, ".")
	}
	return syncDir(root, setupDir)
}

// checkDigest compares what the operator typed with the digest of the handoff, ignoring ASCII spaces.
func checkDigest(handoff []byte, typed string) error {
	t := strings.ReplaceAll(typed, " ", "")
	if !isHex(t, 64) || t != strings.ReplaceAll(Digest(handoff), " ", "") {
		return ErrDigest
	}
	return nil
}

// file is one file a stage writes.
type file struct {
	name string
	data []byte
}

// plan is a stage's whole write set.
type plan struct {
	stage Stage
	dirs  []string // created by the stage, parents first; for a first stage the first is "setup"
	files []file
	next  *state
	prev  []byte // the raw state this stage replaces; nil for a first stage
}

// commit writes a plan: (3) refuse if a target exists, (4) journal, (5) write every file into the temporary
// directory, (6) link each into place, (7) replace state.json, (8) remove the journal. A name that appears after the
// check makes the commit undo what it did and return ErrExists.
func (p *plan) commit(root *os.Root) error {
	first := p.prev == nil
	tmpDir := tmpDirName(p.stage)
	targets := append([]string{}, p.dirs...)
	targets = append(targets, tmpDir, stateTmp, journalTmp)
	for _, f := range p.files {
		targets = append(targets, f.name)
	}
	if first {
		targets = append(targets, stateName)
	}
	sp := specs[p.stage]
	leftover := lexists(root, journalTmp) // a run that died before its journal was in place wrote nothing else
	var clash []string
	for _, t := range targets {
		if t == journalTmp || first && t == setupDir && leftover && onlyLeftoverJournal(root) {
			continue
		}
		if lexists(root, t) {
			clash = append(clash, t)
		}
	}
	if len(clash) > 0 {
		return existsErr(root, clash)
	}
	if leftover {
		if err := clearLeftoverJournal(root, sp); err != nil {
			return err
		}
	}

	var w work
	// raced handles a step that failed because its name appeared after the check: the run undoes its own work.
	raced := func(name string, err error) error {
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		if uerr := undo(root, p, &w); uerr != nil {
			return fmt.Errorf("%w: %s (the run could not undo its work: run it with -discard-incomplete)", ErrExists, name)
		}
		return fmt.Errorf("%w: %s", ErrExists, name)
	}
	if first {
		if err := mkdir(root, setupDir); err != nil {
			return raced(setupDir, err)
		}
		w.dirs = append(w.dirs, setupDir)
	}
	j := journal{Version: 1, Stage: p.stage, Dirs: append(append([]string{}, p.dirs...), tmpDir)}
	j.Files, j.SHA256 = p.journalFiles()
	if !first {
		s := string(p.prev)
		j.PrevState = &s
	}
	jb, err := json.MarshalIndent(&j, "", "  ")
	if err != nil {
		return errors.New("setup: cannot encode the journal")
	}
	// The journal goes in through a temporary name and a link: a torn write never leaves a journal.json.
	if err := writeNew(root, journalTmp, append(jb, '\n')); err != nil {
		return raced(journalTmp, err)
	}
	w.jtmp = true
	if err := root.Link(journalTmp, journalName); err != nil {
		return raced(journalName, err)
	}
	w.journal = true
	if err := root.Remove(journalTmp); err != nil {
		return err
	}
	w.jtmp = false
	if err := syncDir(root, setupDir); err != nil {
		return err
	}
	if err := hook(4); err != nil {
		return err
	}

	for _, d := range p.dirs {
		if d == setupDir {
			continue
		}
		if err := mkdir(root, d); err != nil {
			return raced(d, err)
		}
		w.dirs = append(w.dirs, d)
	}
	if err := mkdir(root, tmpDir); err != nil {
		return raced(tmpDir, err)
	}
	w.dirs = append(w.dirs, tmpDir)
	for i, f := range p.files {
		tp := path.Join(tmpDir, strconv.Itoa(i))
		if err := writeNew(root, tp, f.data); err != nil {
			return raced(tp, err)
		}
		w.tmps = append(w.tmps, tp)
		if err := hook(5); err != nil {
			return err
		}
	}
	for i, f := range p.files {
		tp := path.Join(tmpDir, strconv.Itoa(i))
		if err := root.Link(tp, f.name); err != nil {
			return raced(f.name, err)
		}
		w.linked = append(w.linked, f.name)
		if err := root.Remove(tp); err != nil {
			return err
		}
		if err := hook(6); err != nil {
			return err
		}
	}
	if err := root.Remove(tmpDir); err != nil {
		return err
	}
	if err := syncAround(root, p.files, w.dirs); err != nil {
		return err
	}
	if err := replaceState(root, p.next.marshal()); err != nil {
		return err
	}
	if err := hook(7); err != nil {
		return err
	}
	if err := root.Remove(journalName); err != nil {
		return err
	}
	return syncDir(root, setupDir)
}

func sumHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// journalFiles lists every file the run may create with the SHA-256 of the exact bytes it writes to it (the content is
// generated before the journal is): state.json.tmp and, for a first stage, state.json hold the new state, each
// temporary file the content of its final file. A hash of a secret's file reveals nothing of the secret.
func (p *plan) journalFiles() (names, sums []string) {
	add := func(n string, b []byte) {
		names = append(names, n)
		sums = append(sums, sumHex(b))
	}
	state := p.next.marshal()
	add(stateTmp, state)
	if p.prev == nil {
		add(stateName, state)
	}
	for _, f := range p.files {
		add(f.name, f.data)
	}
	for i, f := range p.files {
		add(path.Join(tmpDirName(p.stage), strconv.Itoa(i)), f.data)
	}
	return names, sums
}

// isScratch reports whether name is one of the command's own scratch names: setup/state.json.tmp and the files directly
// inside the stage's temporary directory. They are the command's bookkeeping, not something a stage leaves for a
// service: Discard and undo remove them whatever their content (a torn write is still the run's own).
func isScratch(stage Stage, name string) bool {
	return name == stateTmp || path.Dir(name) == tmpDirName(stage)
}

// ownScratch reports whether name is a regular file of the command's own scratch, reached through real directories.
func ownScratch(root *os.Root, name string) bool {
	if !realParents(root, name) {
		return false
	}
	fi, err := root.Lstat(name)
	return err == nil && fi.Mode().IsRegular()
}

// ownContent reports whether name is a regular file, reached through real directories, whose bytes hash to sum.
func ownContent(root *os.Root, name, sum string) bool {
	if !realParents(root, name) {
		return false
	}
	b, err := readSmall(root, name)
	return err == nil && sumHex(b) == sum
}

// work is what a run has made so far, for undo: only names it created itself.
type work struct {
	dirs    []string // directories it made, in order (a first stage's setup/ first, the temporary directory last)
	tmps    []string // temporary files it wrote
	linked  []string // files it linked into place
	jtmp    bool     // journal.json.tmp is its own
	journal bool     // journal.json is its own
}

// syncAround fsyncs every directory in which the run created, linked or removed an entry, the base directory
// included.
func syncAround(root *os.Root, files []file, created []string) error {
	set := map[string]bool{".": true, setupDir: true}
	for _, f := range files {
		set[path.Dir(f.name)] = true
	}
	for _, d := range created {
		set[d] = true
		set[path.Dir(d)] = true
	}
	var names []string
	for d := range set {
		names = append(names, d)
	}
	sort.Strings(names)
	for _, d := range names {
		if err := syncDir(root, d); err != nil && !notThere(err) { // the temporary directory is gone by now
			return err
		}
	}
	return nil
}

// undo removes what this run made, nothing else: the files it linked, its temporary files, the directories it made
// (a directory that holds something of another program stays), and last its journal, only when every removal before
// it worked: if one failed the journal stays, and -discard-incomplete finishes the job.
func undo(root *os.Root, p *plan, w *work) error {
	var failed error
	rm := func(name string, dir bool) {
		err := root.Remove(name)
		if err == nil || notThere(err) || dir && errors.Is(err, syscall.ENOTEMPTY) {
			return
		}
		if failed == nil {
			failed = err
		}
	}
	names, sums := p.journalFiles()
	sumOf := map[string]string{}
	for i, n := range names {
		sumOf[n] = sums[i]
	}
	own := func(n string) { // a final file is removed only if it still has the content the run wrote
		if lexists(root, n) && (isScratch(p.stage, n) && ownScratch(root, n) || ownContent(root, n, sumOf[n])) {
			rm(n, false)
		}
	}
	for _, n := range w.linked {
		own(n)
	}
	for _, n := range w.tmps {
		own(n)
	}
	madeSetup := false
	for i := len(w.dirs) - 1; i >= 0; i-- {
		if w.dirs[i] == setupDir {
			madeSetup = true
			continue
		}
		rm(w.dirs[i], true)
	}
	if w.jtmp {
		rm(journalTmp, false)
	}
	if failed == nil && w.journal {
		rm(journalName, false)
	}
	if failed == nil && madeSetup {
		rm(setupDir, true)
	}
	seen := map[string]bool{".": true, setupDir: true}
	for _, f := range p.files {
		seen[path.Dir(f.name)] = true
	}
	for d := range seen {
		syncDir(root, d) // best effort: a directory removed above is gone
	}
	return failed
}

func hook(step int) error {
	if stepHook != nil {
		return stepHook(step)
	}
	return nil
}

// replaceState writes state.json through setup/state.json.tmp (which must not exist) and a rename.
func replaceState(root *os.Root, b []byte) error {
	if err := writeNew(root, stateTmp, b); err != nil {
		return err
	}
	if err := root.Rename(stateTmp, stateName); err != nil {
		return err
	}
	return syncDir(root, setupDir)
}

// writeNew creates name with mode 0600 whatever the umask, never replacing anything, and fsyncs it.
func writeNew(root *os.Root, name string, data []byte) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// mkdir creates name with mode 0700 whatever the umask.
func mkdir(root *os.Root, name string) error {
	if err := root.Mkdir(name, 0o700); err != nil {
		return err
	}
	return root.Chmod(name, 0o700)
}

func syncDir(root *os.Root, name string) error {
	d, err := root.Open(name)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// realParents reports whether every directory above name is a real directory: a symbolic link there, even one that
// stays inside the base directory, is never followed.
func realParents(root *os.Root, name string) bool {
	for d := path.Dir(name); d != "."; d = path.Dir(d) {
		if fi, err := root.Lstat(d); err != nil || !fi.IsDir() {
			return false
		}
	}
	return true
}

// Discard removes exactly what the dead run of the given stage journaled and that exists (its files, then the
// directories it made that are empty), puts back the exact state.json bytes the run replaced, and removes the
// journal last. It works only when journal.json exists and is a journal of this stage: a journal of another stage, one
// with a name the stage never creates, or with a prev_state that is not this host's state at the predecessor stage,
// is refused and nothing is removed. A journal.json.tmp alone (the run died before the journal was in place) is
// removed. An empty or unparsable journal is refused: the operator inspects it and deletes it by hand.
func (l *Lock) Discard(stage Stage) (removed, kept []string, err error) {
	if l.closed {
		return nil, nil, ErrClosed
	}
	sp := specs[stage]
	if sp == nil {
		return nil, nil, errors.New("setup: unknown stage")
	}
	root := l.root
	if !lexists(root, setupDir) {
		return nil, nil, errors.New("setup: there is nothing to discard: no earlier run of a stage was left unfinished")
	}
	if err := checkDir(root, setupDir); err != nil { // never through a link, even one that stays inside the directory
		return nil, nil, err
	}
	for _, d := range sp.into { // the directories the stage writes into, before the journal is read
		if err := checkDir(root, d); err != nil {
			return nil, nil, err
		}
	}
	raw, err := readSmall(root, journalName)
	if notThere(err) {
		if lexists(root, journalTmp) {
			if err := clearLeftoverJournal(root, sp); err != nil {
				return nil, nil, err
			}
			return []string{journalTmp}, nil, nil
		}
		if emptyDir(root, setupDir) {
			return nil, nil, errors.New("setup: there is nothing to discard: setup/ is empty (an interrupted run or discard left it): remove it with rmdir")
		}
		return nil, nil, errors.New("setup: there is nothing to discard: no earlier run of a stage was left unfinished")
	}
	if err != nil {
		return nil, nil, errBadJournal
	}
	j, err := parseJournal(raw)
	if err != nil {
		return nil, nil, err
	}
	if j.Stage != stage {
		return nil, nil, fmt.Errorf("setup: the unfinished run is of stage %s, not of this stage: discard it with that stage; nothing was removed", j.Stage)
	}
	// Every directory the journal names that exists is checked too. config/wallet-rpc belongs to another user after
	// the bundle's first start: only a symbolic link there is refused.
	for _, d := range j.Dirs {
		if !lexists(root, d) {
			continue
		}
		if d == "config/wallet-rpc" {
			if fi, err := root.Lstat(d); err != nil || !fi.IsDir() {
				return nil, nil, fmt.Errorf("setup: %s must be a real directory, not a symbolic link; nothing was removed", d)
			}
			continue
		}
		if err := checkDir(root, d); err != nil {
			return nil, nil, err
		}
	}
	step := func(name string) error {
		removed = append(removed, name)
		if discardHook != nil {
			return discardHook()
		}
		return nil
	}
	synced := map[string]bool{}
	for i, n := range j.Files {
		fi, err := root.Lstat(n)
		if err != nil || fi.IsDir() {
			continue
		}
		if isScratch(stage, n) { // the command's own scratch: removed whatever its content, never reported
			if !ownScratch(root, n) {
				continue
			}
		} else if !ownContent(root, n, j.SHA256[i]) { // a final name with other content: someone else's, or changed since
			kept = append(kept, n)
			continue
		}
		if err := root.Remove(n); err != nil {
			return removed, kept, fmt.Errorf("setup: cannot remove %s", n)
		}
		synced[path.Dir(n)] = true
		if err := step(n); err != nil {
			return removed, kept, err
		}
	}
	dirs := append([]string{}, j.Dirs...)
	sort.Slice(dirs, func(a, b int) bool { return len(dirs[a]) > len(dirs[b]) })
	for _, n := range dirs {
		if n == setupDir {
			continue
		}
		if fi, err := root.Lstat(n); err != nil || !fi.IsDir() || !realParents(root, n) {
			continue
		}
		if err := root.Remove(n); err == nil { // a directory that holds something else stays
			synced[path.Dir(n)] = true
			if err := step(n + "/"); err != nil {
				return removed, kept, err
			}
		}
	}
	if j.PrevState != nil {
		if err := replaceState(root, []byte(*j.PrevState)); err != nil {
			return removed, kept, errors.New("setup: cannot put setup/state.json back")
		}
		if discardHook != nil {
			if err := discardHook(); err != nil {
				return removed, kept, err
			}
		}
	}
	for d := range synced {
		if d != setupDir {
			if err := syncDir(root, d); err != nil && !notThere(err) { // a directory Discard removed needs none
				return removed, kept, err
			}
		}
	}
	if err := syncDir(root, setupDir); err != nil {
		return removed, kept, err
	}
	if err := root.Remove(journalName); err != nil {
		return removed, kept, errors.New("setup: cannot remove setup/journal.json")
	}
	if slices.Contains(j.Dirs, setupDir) { // a first stage made setup/ itself: it goes with its journal
		if err := root.Remove(setupDir); err == nil {
			removed = append(removed, setupDir+"/")
		}
		return removed, kept, syncDir(root, ".")
	}
	return removed, kept, syncDir(root, setupDir)
}
