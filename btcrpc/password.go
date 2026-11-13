package btcrpc

import (
	"errors"
	"io"
	"os"
	"syscall"
)

// ErrPasswordFile means the password file was refused: not a regular file, readable or writable by group
// or others, or its content is not 1 to 128 bytes of printable ASCII. The content is never in the error.
var ErrPasswordFile = errors.New("btcrpc: password file refused: it must be a regular file with mode 0600 or stricter holding 1 to 128 printable ASCII bytes (0x21 to 0x7e) and an optional final newline")

const maxPassword = 128

// LoadPassword reads the RPC password from path: a regular file (a symbolic link is refused) with mode
// 0600 or stricter, holding 1 to 128 bytes of printable ASCII (0x21 to 0x7e) and an optional final newline,
// which is stripped. The caller owns the returned slice and should clear it after New copies it.
func LoadPassword(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) // O_NONBLOCK: opening a FIFO must not wait for a writer
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) {
			return nil, errors.New("btcrpc: opening the password file: " + pe.Err.Error())
		}
		return nil, errors.New("btcrpc: opening the password file")
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, errors.New("btcrpc: reading the password file")
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&^0o600 != 0 {
		return nil, ErrPasswordFile
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxPassword+2)) // the password, a newline, and one byte too many
	defer clear(raw)
	if err != nil {
		return nil, errors.New("btcrpc: reading the password file")
	}
	if n := len(raw); n > 0 && raw[n-1] == '\n' {
		raw = raw[:n-1]
	}
	if len(raw) < 1 || len(raw) > maxPassword {
		return nil, ErrPasswordFile
	}
	for _, b := range raw {
		if b < 0x21 || b > 0x7e {
			return nil, ErrPasswordFile
		}
	}
	return append([]byte(nil), raw...), nil
}
