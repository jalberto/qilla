package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/install"
)

func storeSecret(t *testing.T, configDir, name, val string) {
	t.Helper()
	os.MkdirAll(install.CredsDir(configDir), 0o700)
	cmd := exec.Command("systemd-creds", "encrypt", "--user", "--name="+name, "-",
		filepath.Join(install.CredsDir(configDir), name+".cred"))
	cmd.Stdin = strings.NewReader(val)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("systemd-creds unusable: %v: %s", err, b)
	}
}

func TestRoutineSecretsManifestFilter(t *testing.T) {
	cfgDir, dir := t.TempDir(), t.TempDir()
	os.MkdirAll(install.CredsDir(cfgDir), 0o700)
	for _, n := range []string{"karakeep", "gcal", "other"} {
		os.WriteFile(filepath.Join(install.CredsDir(cfgDir), n+".cred"), []byte("x"), 0o600)
	}
	// no manifest → every stored secret
	if got := routineSecrets(dir, cfgDir); !strings.Contains(strings.Join(got, ","), "other") || len(got) != 3 {
		t.Fatalf("no manifest must mount all secrets, got %v", got)
	}
	os.WriteFile(filepath.Join(dir, "routine.toml"), []byte(
		"name = \"n\"\n[requires]\nsecrets = [\"gcal\"]\n[[optional]]\nname = \"kk\"\nsecrets = [\"karakeep\"]\n"), 0o644)
	got := routineSecrets(dir, cfgDir)
	if strings.Join(got, ",") != "gcal,karakeep" {
		t.Fatalf("manifest must scope the mount, got %v", got)
	}
}

func TestGatherMountsSecretsWithoutSystemd(t *testing.T) {
	if _, err := exec.LookPath("systemd-creds"); err != nil {
		t.Skip("systemd-creds not available")
	}
	e := setup(t, config.Routine{Kind: config.KindScript},
		"#!/bin/sh\nprintf '{\"tok\":\"%s\",\"perm\":\"%s\"}' \"$(cat \"$QILLA_SECRETS_DIR/tok\")\" \"$(stat -c %a \"$QILLA_SECRETS_DIR/tok\")\"\n")
	t.Setenv("CREDENTIALS_DIRECTORY", "") // no systemd: qilla must mount them itself
	storeSecret(t, filepath.Dir(e.cfg.Path), "tok", "s3cr3t")
	out, ok, err := e.w.Gather(context.Background(), "r")
	if err != nil || !ok {
		t.Fatalf("gather: %v (ok=%v)", err, ok)
	}
	if !strings.Contains(out, `"tok":"s3cr3t"`) || !strings.Contains(out, `"perm":"600"`) {
		t.Fatalf("gather must read the mounted secret at 0600: %s", out)
	}
	// the mount is removed once the gather is done
	es, _ := os.ReadDir(e.w.secretsRoot())
	for _, d := range es {
		if strings.HasPrefix(d.Name(), "r-") {
			t.Fatalf("secrets mount left behind: %s", d.Name())
		}
	}
}

func TestGatherKeepsSystemdCredentialsDirectory(t *testing.T) {
	e := setup(t, config.Routine{Kind: config.KindScript},
		"#!/bin/sh\nprintf '{\"d\":\"%s\"}' \"$QILLA_SECRETS_DIR\"\n")
	t.Setenv("CREDENTIALS_DIRECTORY", "/run/creds/qilla")
	out, _, err := e.w.Gather(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"d":"/run/creds/qilla"`) {
		t.Fatalf("under systemd the credentials directory is used as is: %s", out)
	}
}
