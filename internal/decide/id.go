package decide

import (
	"encoding/json"
	"strconv"
)

// ID is a row identifier. Callers disagree about its type — msgvault ids are
// integers, the shadow log and some gathers use strings — so it is kept as
// the raw JSON token and echoed back exactly as it arrived: a caller that
// sent 12345 gets 12345 back, not "12345".
type ID struct{ raw json.RawMessage }

// StringID and IntID build an ID from a Go value (the Starlark builtin's path).
func StringID(s string) ID {
	b, _ := json.Marshal(s)
	return ID{raw: b}
}

func IntID(n int64) ID { return ID{raw: json.RawMessage(strconv.FormatInt(n, 10))} }

func (i *ID) UnmarshalJSON(b []byte) error {
	i.raw = append(i.raw[:0], b...)
	return nil
}

func (i ID) MarshalJSON() ([]byte, error) {
	if len(i.raw) == 0 {
		return []byte("null"), nil
	}
	return i.raw, nil
}

// String is the id for logs and map keys, without the JSON quoting.
func (i ID) String() string {
	if len(i.raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(i.raw, &s); err == nil {
		return s
	}
	return string(i.raw)
}

// Value is the id as a plain Go value — string for a JSON string, int64 or
// float64 for a number — so a consumer can hand it back in its own types.
func (i ID) Value() any {
	if len(i.raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(i.raw, &s); err == nil {
		return s
	}
	if n, err := strconv.ParseInt(string(i.raw), 10, 64); err == nil {
		return n
	}
	var f float64
	if err := json.Unmarshal(i.raw, &f); err == nil {
		return f
	}
	return string(i.raw)
}
