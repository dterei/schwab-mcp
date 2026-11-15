package mcpserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
)

// A cursor (design 8.5) is opaque to the model: base64url, without
// padding, of {"v":1,"t":tool,"f":digest,"k":key}, at most 512 bytes. The
// digest is the first 16 hex digits of the SHA-256 of the call's canonical
// arguments: its arguments with the defaults applied, as JSON with the keys
// sorted, without cursor and limit, and with account replaced by the picked
// account's key (dirAccount.key), its hash or store ref, never its number:
// a digest over a number would let anyone who holds the transcript and the
// other arguments find the number among a few candidates. The key is where
// the page ended, a total order of the tool's rows, so that no row is
// skipped or repeated. Nothing is kept in the server: a restarted server,
// or another process, continues a page. A cursor is not signed; a forged
// one can only move where a page of the person's own data starts.

// cursorVersion is the version of the cursors this build writes and reads.
const cursorVersion = 1

// cursorBody is what a cursor holds.
type cursorBody struct {
	V int             `json:"v"`
	T string          `json:"t"`
	F string          `json:"f"`
	K json.RawMessage `json:"k"`
}

// newCursor returns the cursor of the call's next page: key is the sort key
// of the last row sent, and args the call's canonical arguments.
func (c *call) newCursor(args, key any) (*Cursor, error) {
	f, err := argsDigest(args)
	if err != nil {
		return nil, err
	}
	k, err := encode(key)
	if err != nil {
		return nil, err
	}
	b, err := encode(cursorBody{V: cursorVersion, T: c.tool, F: f, K: k})
	if err != nil {
		return nil, err
	}
	cur := Cursor(base64.RawURLEncoding.EncodeToString(b))
	if len(cur) > cursorMax {
		return nil, c.failf(codeInternal, "The server made a page cursor longer than %d bytes, a bug.", cursorMax)
	}
	return &cur, nil
}

// readCursor decodes cur, a cursor the model passed back, into key. A
// cursor of another tool or of other arguments, of another version, or one
// that does not decode, is bad_cursor; a changed limit is not checked.
func (c *call) readCursor(cur string, args, key any) error {
	bad := c.badCursor()
	b, err := base64.RawURLEncoding.DecodeString(cur)
	if err != nil {
		return bad
	}
	var body cursorBody
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(&body) != nil || body.V != cursorVersion || body.T != c.tool {
		return bad
	}
	f, err := argsDigest(args)
	if err != nil {
		return err
	}
	if body.F != f || json.Unmarshal(body.K, key) != nil {
		return bad
	}
	return nil
}

// badCursor is the error for a cursor that does not belong to the call, or
// whose key is none that the tool writes.
func (c *call) badCursor() error {
	return c.failf(codeBadCursor, "The cursor does not belong to these arguments; call again without it.")
}

// argsDigest returns the first 16 hex digits of the SHA-256 of args as
// canonical JSON: args encoded, then decoded and encoded again as a map,
// which sorts the keys of every object.
func argsDigest(args any) (string, error) {
	b, err := encode(args)
	if err != nil {
		return "", err
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	canonical, err := encode(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:8]), nil
}
