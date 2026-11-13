package api

import "os"

func writeFile0600(p, body string) error {
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		return err
	}
	return os.Chmod(p, 0o600)
}

func readFile(p string) ([]byte, error) { return os.ReadFile(p) }

func renameFile(a, b string) error { return os.Rename(a, b) }
