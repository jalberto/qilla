package candidates

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestAddListCountDrop(t *testing.T) {
	s := New(t.TempDir())
	now := at("2026-09-12T14:30:05+02:00")
	id, dup, err := s.Add("time-promises", Candidate{Source: "meeting", Text: "call Ana next week", People: []string{"Ana"}, DateHint: "next week"}, now)
	if err != nil || dup {
		t.Fatalf("add: %v dup=%v", err, dup)
	}
	if !strings.HasPrefix(id, "c_20260912_143005_") || len(id) != len("c_20260912_143005_")+8 {
		t.Fatalf("id %q is not c_YYYYMMDD_HHMMSS_<hex>", id)
	}

	items, err := s.List("time-promises")
	if err != nil || len(items) != 1 {
		t.Fatalf("list: %v %d items", err, len(items))
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(items[0].Raw), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"id": id, "ts": "2026-09-12T14:30:05+02:00", "source": "meeting",
		"text": "call Ana next week", "people": []any{"Ana"}, "date_hint": "next week"}
	for k, v := range want {
		if s := toJSON(got[k]); s != toJSON(v) {
			t.Errorf("%s = %s, want %s", k, s, toJSON(v))
		}
	}
	if len(got) != len(want) {
		t.Errorf("fields %v, want %v", keys(got), keys(want))
	}

	// exact-text duplicate: no second line, the existing id comes back
	if id2, dup, err := s.Add("time-promises", Candidate{Text: "call Ana next week"}, now); err != nil || !dup || id2 != id {
		t.Fatalf("duplicate: id=%q dup=%v err=%v", id2, dup, err)
	}
	if n, _ := s.Count("time-promises"); n != 1 {
		t.Fatalf("count after duplicate = %d, want 1", n)
	}

	id2, _, err := s.Add("facts", Candidate{Text: "Alice moved to Lisbon"}, now)
	if err != nil {
		t.Fatal(err)
	}
	counts, err := s.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 2 || counts[0] != (Count{"facts", 1}) || counts[1] != (Count{"time-promises", 1}) {
		t.Fatalf("counts = %v", counts)
	}

	n, err := s.DropAll([]string{id, id2, "c_nope"})
	if err != nil || n != 2 {
		t.Fatalf("drop = %d, %v; want 2", n, err)
	}
	if counts, _ := s.Counts(); len(counts) != 0 {
		t.Fatalf("counts after drop = %v", counts)
	}
	if b, err := os.ReadFile(s.Path("facts")); err != nil || len(b) != 0 {
		t.Fatalf("drained file = %q, %v", b, err)
	}
}

func TestDropKeepsTheRest(t *testing.T) {
	s := New(t.TempDir())
	now := at("2026-09-12T14:30:05+02:00")
	var ids []string
	for _, txt := range []string{"one", "two", "three"} {
		id, _, err := s.Add("facts", Candidate{Text: txt}, now)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if n, err := s.Drop("facts", []string{ids[1]}); err != nil || n != 1 {
		t.Fatalf("drop = %d, %v", n, err)
	}
	items, _ := s.List("facts")
	if len(items) != 2 || items[0].Text != "one" || items[1].Text != "three" {
		t.Fatalf("kept %v", items)
	}
	b, _ := os.ReadFile(s.Path("facts"))
	if !strings.HasSuffix(string(b), "\n") || strings.Contains(string(b), "\n\n") {
		t.Fatalf("file not clean JSONL: %q", b)
	}
}

func TestEmptyAndBadInput(t *testing.T) {
	s := New(t.TempDir())
	if items, err := s.List("nothing"); err != nil || items != nil {
		t.Fatalf("missing family: %v %v", items, err)
	}
	if n, err := s.Drop("nothing", []string{"x"}); err != nil || n != 0 {
		t.Fatalf("drop on missing family: %d %v", n, err)
	}
	if _, _, err := s.Add("Bad Family", Candidate{Text: "x"}, time.Now()); err == nil {
		t.Fatal("bad family accepted")
	}
	if _, _, err := s.Add("facts", Candidate{Text: "  "}, time.Now()); err == nil {
		t.Fatal("empty text accepted")
	}
	for _, f := range []string{"facts", "time-promises", "notion-tasks", "a"} {
		if !ValidFamily(f) {
			t.Errorf("%q rejected", f)
		}
	}
	for _, f := range []string{"", "1facts", "-facts", "Facts", "fa/cts", "fa.cts"} {
		if ValidFamily(f) {
			t.Errorf("%q accepted", f)
		}
	}
}

func TestPeopleIsAlwaysAList(t *testing.T) {
	s := New(t.TempDir())
	if _, _, err := s.Add("facts", Candidate{Text: "no people"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	items, _ := s.List("facts")
	if !strings.Contains(items[0].Raw, `"people":[]`) {
		t.Fatalf("people not an empty list: %s", items[0].Raw)
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func keys(m map[string]any) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	return k
}
