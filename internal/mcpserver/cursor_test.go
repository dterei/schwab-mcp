package mcpserver

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// cursorArgs are a call's canonical arguments, as a tool makes them.
type cursorArgs struct {
	Account string   `json:"account"`
	From    string   `json:"from"`
	Types   []string `json:"types"`
}

// cursorKey is a position, as a tool makes it.
type cursorKey struct {
	Day, Time, Ref, Account string
}

// A cursor carries its tool, a digest of the canonical arguments and the
// position, and reads back only for the same tool and arguments, whatever
// order the arguments' keys come in; one of another tool, other
// arguments, another version or none that decodes is bad_cursor; and a
// cursor never holds more than 512 bytes, nor an account's number.
func TestCursor(t *testing.T) {
	t.Parallel()
	c := &call{tool: "get_transactions", mask: testMasker("")}
	args := cursorArgs{Account: "742C51B544655B85F41224019A82CD7AB3C6BABDB67BF80531E82B306CB5F691", From: "2025-01-01", Types: []string{"TRADE"}}
	key := cursorKey{Day: "2025-06-02", Time: "2025-06-02T14:30:27.000Z", Ref: "T73508261943", Account: args.Account}
	cur, err := c.newCursor(args, key)
	if err != nil {
		t.Fatal(err)
	}
	var back cursorKey
	if err := c.readCursor(string(*cur), args, &back); err != nil || back != key {
		t.Fatalf("readCursor = %+v, %v; want %+v", back, err, key)
	}
	// The same arguments as a map, its keys in another order.
	same := map[string]any{"types": []string{"TRADE"}, "from": "2025-01-01", "account": args.Account}
	if err := c.readCursor(string(*cur), same, &back); err != nil {
		t.Errorf("the same arguments as a map: %v", err)
	}
	b, _ := base64.RawURLEncoding.DecodeString(string(*cur))
	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil || body["v"] != 1.0 || body["t"] != "get_transactions" || len(body["f"].(string)) != 16 {
		t.Errorf("the cursor holds %s", b)
	}
	other := args
	other.From = "2025-01-02"
	versioned := strings.Replace(string(b), `"v":1`, `"v":2`, 1)
	for name, tc := range map[string]struct {
		cur  string
		args any
		tool string
	}{
		"other arguments":        {string(*cur), other, "get_transactions"},
		"another tool":           {string(*cur), args, "get_orders"},
		"another version":        {base64.RawURLEncoding.EncodeToString([]byte(versioned)), args, "get_transactions"},
		"not base64":             {"!!", args, "get_transactions"},
		"not JSON":               {base64.RawURLEncoding.EncodeToString([]byte("{")), args, "get_transactions"},
		"an unknown key":         {base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(b), "{", `{"x":1,`, 1))), args, "get_transactions"},
		"a key of another shape": {base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"t":"get_transactions","f":"` + body["f"].(string) + `","k":[1]}`)), args, "get_transactions"},
	} {
		c := &call{tool: tc.tool, mask: testMasker("")}
		err := c.readCursor(tc.cur, tc.args, &back)
		if e, ok := errors.AsType[*toolErr](err); !ok || e.code != codeBadCursor || !strings.Contains(string(e.msg), "call again without it") {
			t.Errorf("%s: %v, want bad_cursor", name, err)
		}
	}
	if _, err := c.newCursor(args, strings.Repeat("k", cursorMax)); err == nil {
		t.Error("a cursor over 512 bytes was made")
	}
}
