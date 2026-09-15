package main

import (
	"strings"
	"testing"
)

func TestSetPasswordHashNeverDuplicatesWeb(t *testing.T) {
	cases := map[string]string{
		"existing":  "[web]\nlisten = \"127.0.0.1:7433\"\npassword_hash = \"\"\n",
		"commented": "[web]\nlisten = \"x\"\n# password_hash = \"\"\n",
		"no line":   "[web]\nlisten = \"x\"\n\n[agents.chief]\nmodel = \"m\"\n",
		"no table":  "vault = \"~/Notes\"\n",
	}
	for name, in := range cases {
		out := setPasswordHash(in, "$2a$10$abc")
		if strings.Count(out, "[web]") != 1 {
			t.Errorf("%s: [web] appears %d times:\n%s", name, strings.Count(out, "[web]"), out)
		}
		if strings.Count(out, "password_hash = \"$2a$10$abc\"") != 1 {
			t.Errorf("%s: hash line count wrong:\n%s", name, out)
		}
		// the hash line must sit inside [web], i.e. before any other table header that follows [web]
		w := strings.Index(out, "[web]")
		h := strings.Index(out, "password_hash = \"$2a")
		a := strings.Index(out, "[agents")
		if h < w || (a > 0 && h > a) {
			t.Errorf("%s: hash line not inside [web]:\n%s", name, out)
		}
	}
}
