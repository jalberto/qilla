package manifest

import (
	"strings"
	"testing"
)

const capSample = `
name = "newsletters-act"

[capabilities]
http  = { hosts = ["nasdxp:3000", "api.notion.com"], methods = ["GET", "POST"] }
write = { paths = ["Library/Newsletters/"] }
exec  = ["msgvault", "qilla"]
`

// rows renders the check as "item|status" lines, for stable assertions.
func rows(rs []Result) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Item+"|"+r.Status)
	}
	return out
}

func has(rs []Result, item string) *Result {
	for i := range rs {
		if rs[i].Item == item {
			return &rs[i]
		}
	}
	return nil
}

func TestCapabilitiesParse(t *testing.T) {
	m, err := Load(bundle(t, capSample))
	if err != nil {
		t.Fatal(err)
	}
	c := m.Capabilities
	if c == nil || c.HTTP == nil || c.Write == nil {
		t.Fatalf("capabilities: %+v", c)
	}
	if len(c.HTTP.Hosts) != 2 || c.HTTP.Hosts[0] != "nasdxp:3000" {
		t.Errorf("hosts = %v", c.HTTP.Hosts)
	}
	if got := c.HTTP.MethodList(); strings.Join(got, ",") != "GET,POST" {
		t.Errorf("methods = %v", got)
	}
	if len(c.Write.Paths) != 1 || c.Write.Paths[0] != "Library/Newsletters/" {
		t.Errorf("write paths = %v", c.Write.Paths)
	}
	if strings.Join(c.Exec, ",") != "msgvault,qilla" {
		t.Errorf("exec = %v", c.Exec)
	}
}

func TestCapabilitiesCheckRows(t *testing.T) {
	m, err := Load(bundle(t, capSample))
	if err != nil {
		t.Fatal(err)
	}
	rs := m.Check(Env{}, nil)
	for _, want := range []string{
		"cap http  hosts=nasdxp:3000,api.notion.com methods=GET,POST|ok",
		"cap write paths=Library/Newsletters/|ok",
		"cap exec msgvault,qilla|ok",
	} {
		found := false
		for _, got := range rows(rs) {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing row %q in %v", want, rows(rs))
		}
	}
	if Failed(rs) {
		t.Errorf("a valid declaration must not fail the check: %v", rows(rs))
	}
}

func TestCapabilitiesMethodsDefaultToGET(t *testing.T) {
	m, err := Load(bundle(t, "[capabilities]\nhttp = { hosts = [\"api.notion.com\"] }\n"))
	if err != nil {
		t.Fatal(err)
	}
	rs := m.Check(Env{}, nil)
	if r := has(rs, "cap http  hosts=api.notion.com methods=GET"); r == nil || !r.OK {
		t.Fatalf("rows = %v", rows(rs))
	}
}

func TestCapabilitiesValidationFails(t *testing.T) {
	cases := map[string]string{
		"empty hosts":   "[capabilities]\nhttp = { hosts = [] }\n",
		"bad method":    "[capabilities]\nhttp = { hosts = [\"a.b\"], methods = [\"FETCH\"] }\n",
		"absolute path": "[capabilities]\nwrite = { paths = [\"/etc/passwd\"] }\n",
		"dotdot path":   "[capabilities]\nwrite = { paths = [\"Library/../../x\"] }\n",
		"empty paths":   "[capabilities]\nwrite = { paths = [] }\n",
	}
	for name, body := range cases {
		m, err := Load(bundle(t, body))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		rs := m.Check(Env{}, nil)
		if !Failed(rs) {
			t.Errorf("%s: want a failing row, got %v", name, rows(rs))
		}
	}
}

func TestCapabilitiesAbsentStaysPure(t *testing.T) {
	m, err := Load(bundle(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	if m.Capabilities != nil {
		t.Fatalf("capabilities = %+v", m.Capabilities)
	}
	for _, r := range rows(m.Check(Env{}, map[string]any{"accounts": "x"})) {
		if strings.HasPrefix(r, "cap ") {
			t.Errorf("unexpected cap row %q", r)
		}
	}
}
