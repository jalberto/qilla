package worker

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jalberto/qilla/internal/install"
	"github.com/jalberto/qilla/internal/manifest"
)

// secretsRoot is the parent of the directories qilla mounts secrets into when
// systemd did not (i.e. `qilla run` from a terminal). It is tmpfs when
// XDG_RUNTIME_DIR exists, the state dir otherwise, and is always denied to the
// sandboxed model in settingsFile.
func (w *Worker) secretsRoot() string {
	if rd := os.Getenv("XDG_RUNTIME_DIR"); rd != "" {
		return filepath.Join(rd, "qilla", "secrets")
	}
	return filepath.Join(w.Cfg().StateDir, "secrets")
}

// routineSecrets is which secrets a routine may see: the names its manifest
// declares (requires + optional), or every stored secret when the bundle has
// no manifest.
func routineSecrets(dir, configDir string) []string {
	all := install.SecretNames(configDir)
	m, err := manifest.Load(dir)
	if err != nil || m == nil {
		return all
	}
	want := map[string]bool{}
	for _, n := range m.Requires.Secrets {
		want[n] = true
	}
	for _, o := range m.Optional {
		for _, n := range o.Secrets {
			want[n] = true
		}
	}
	var out []string
	for _, n := range all {
		if want[n] {
			out = append(out, n)
		}
	}
	return out
}

// mountSecrets materializes the routine's secrets as one 0600 file per name in
// a private directory and returns it with a cleanup func. Empty string means
// there is nothing to mount. Plaintext comes from systemd-creds; a secret that
// cannot be decrypted is logged and skipped, so a gather that does not need it
// still runs.
func (w *Worker) mountSecrets(dir, name string) (string, func()) {
	configDir := filepath.Dir(w.Cfg().Path)
	names := routineSecrets(dir, configDir)
	if len(names) == 0 {
		return "", func() {}
	}
	root := w.secretsRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		log.Printf("routine %s: secrets dir: %v", name, err)
		return "", func() {}
	}
	mnt, err := os.MkdirTemp(root, name+"-")
	if err != nil {
		log.Printf("routine %s: secrets dir: %v", name, err)
		return "", func() {}
	}
	cleanup := func() { os.RemoveAll(mnt) }
	n := 0
	for _, s := range names {
		val, err := decryptSecret(configDir, s)
		if err != nil {
			log.Printf("routine %s: secret %s: %v", name, s, err)
			continue
		}
		if err := os.WriteFile(filepath.Join(mnt, s), val, 0o600); err != nil {
			log.Printf("routine %s: secret %s: %v", name, s, err)
			continue
		}
		n++
	}
	if n == 0 {
		cleanup()
		return "", func() {}
	}
	return mnt, cleanup
}

// decryptSecret returns the plaintext of one stored secret.
func decryptSecret(configDir, name string) ([]byte, error) {
	cred := filepath.Join(install.CredsDir(configDir), name+".cred")
	cmd := exec.Command("systemd-creds", "decrypt", "--user", "--name="+name, cred, "-")
	out, err := cmd.Output()
	if err != nil {
		msg := ""
		if ee, ok := err.(*exec.ExitError); ok {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		return nil, fmt.Errorf("systemd-creds decrypt: %v: %s", err, msg)
	}
	return out, nil
}
