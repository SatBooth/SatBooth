package setup

import "io"

// The helpers below take the directory's lock for one call and run the method of the same name, as the tests did
// before every stage became a method of *Lock. They are test code: the command calls LockDir and the methods.

// Preflight takes the directory's lock for the call and runs Lock.Preflight. It is refused with ErrBusy while a Lock
// of the directory is held, this process's too.
func Preflight(dir string, stage Stage) error {
	l, err := LockDir(dir)
	if err != nil {
		return err
	}
	defer l.Close()
	return l.Preflight(stage)
}

// Discard takes the directory's lock for the call and runs Lock.Discard. It is refused with ErrBusy while a
// Lock of the directory is held, this process's too.
func Discard(dir string, stage Stage) (removed, kept []string, err error) {
	l, err := LockDir(dir)
	if err != nil {
		return nil, nil, err
	}
	defer l.Close()
	return l.Discard(stage)
}

// PublicInit takes the directory's lock for the call and runs Lock.PublicInit. It is refused with ErrBusy while a
// Lock of the directory is held, this process's too.
func PublicInit(dir string, a *PublicAnswers, hubURL string, rand io.Reader) (*Report, error) {
	l, err := LockDir(dir)
	if err != nil {
		return nil, err
	}
	defer l.Close()
	return l.PublicInit(a, hubURL, rand)
}

// PublicFinish takes the directory's lock for the call and runs Lock.PublicFinish. It is refused with ErrBusy while a
// Lock of the directory is held, this process's too.
func PublicFinish(dir string, handoff []byte, digest string, walletRPCPass []byte) (*Report, error) {
	l, err := LockDir(dir)
	if err != nil {
		return nil, err
	}
	defer l.Close()
	return l.PublicFinish(handoff, digest, walletRPCPass)
}

// HubInit takes the directory's lock for the call and runs Lock.HubInit. It is refused with ErrBusy while a
// Lock of the directory is held, this process's too.
func HubInit(dir string, a *HubAnswers, handoff []byte, digest string, rand io.Reader) (*Report, error) {
	l, err := LockDir(dir)
	if err != nil {
		return nil, err
	}
	defer l.Close()
	return l.HubInit(a, handoff, digest, rand)
}

// HubPair takes the directory's lock for the call and runs Lock.HubPair. It is refused with ErrBusy while a
// Lock of the directory is held, this process's too.
func HubPair(dir string, invoiceWallet, payoutWallet string) (*Report, error) {
	l, err := LockDir(dir)
	if err != nil {
		return nil, err
	}
	defer l.Close()
	return l.HubPair(invoiceWallet, payoutWallet)
}
