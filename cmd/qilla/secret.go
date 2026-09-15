package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/install"
)

var secretNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// cmdSecret: qilla secret set <name> (reads stdin) | list | rm <name>
// Secrets are encrypted with systemd-creds (host key / TPM), loaded into
// qilla.service as $CREDENTIALS_DIRECTORY/<name>, visible to gather.sh only.
func cmdSecret(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: qilla secret set <name> < file | list | rm <name>")
	}
	dir := install.CredsDir(filepath.Dir(config.DefaultPath()))
	switch args[0] {
	case "list":
		for _, n := range install.SecretNames(filepath.Dir(config.DefaultPath())) {
			fmt.Println(n)
		}
		return nil
	case "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: qilla secret rm <name>")
		}
		if err := os.Remove(filepath.Join(dir, args[1]+".cred")); err != nil {
			return err
		}
		fmt.Println("removed; run `qilla init` to update the unit, then restart qilla.service")
		return nil
	case "set":
		if len(args) != 2 || !secretNameRe.MatchString(args[1]) {
			return fmt.Errorf("usage: qilla secret set <name>   (name: [a-z0-9_-], value on stdin)")
		}
		if _, err := exec.LookPath("systemd-creds"); err != nil {
			return fmt.Errorf("systemd-creds not found: secrets need systemd ≥ 250")
		}
		val, err := io.ReadAll(os.Stdin)
		if err != nil || len(strings.TrimSpace(string(val))) == 0 {
			return fmt.Errorf("empty secret on stdin")
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		out := filepath.Join(dir, args[1]+".cred")
		cmd := exec.Command("systemd-creds", "encrypt", "--user", "--name="+args[1], "-", out)
		cmd.Stdin = strings.NewReader(strings.TrimRight(string(val), "\r\n"))
		if b, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("systemd-creds encrypt: %v: %s", err, strings.TrimSpace(string(b)))
		}
		os.Chmod(out, 0o600)
		fmt.Printf("stored %s (encrypted). Run `qilla init` to add it to the unit, then `systemctl --user restart qilla.service`.\n", out)
		fmt.Printf("gather.sh reads it as: $(cat \"$QILLA_SECRETS_DIR/%s\")\n", args[1])
		return nil
	}
	return fmt.Errorf("unknown secret command %q", args[0])
}
