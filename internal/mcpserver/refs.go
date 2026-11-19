package mcpserver

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// The store's ref of an API row without an activity ID (docs/store.md,
// Refs) is T~, the row's time and 12 hex digits of the SHA-256 of the
// row's cut form, which holds the row's full account number. With the
// row's other fields, as include_raw returns them, anyone could find the
// number among the 10^5 that end in its last three digits, by testing
// each against the digest: the attack that design 8.5 keeps cursors'
// digests from. So the server never shows that digest. It shows each such
// ref with the digest permuted under a key of its own, in rows, pairs,
// matches, the store's flag details and cursors, and maps a ref given back
// to the store's before it reads the store or Schwab: get_transaction's
// id, and a cursor's position. The permutation keeps each ref's time and
// its .2, .3 and so on, and gives distinct refs distinct digests; without
// the key, no one can compute it or undo it.

// A tildeKey is the key that the digests of T~ refs are permuted under.
type tildeKey [32]byte

// tildeRounds is how many rounds the permutation has.
const tildeRounds = 8

// newTildeKey returns the key that the app's secret gives, so that a ref,
// and a cursor that holds one, mean the same to every process of the app
// and after a restart; without a secret, a random key of this process.
// The key is a MAC of a fixed text under the secret, which no answer holds.
func newTildeKey(appSecret string) tildeKey {
	var k tildeKey
	if appSecret == "" {
		_, _ = rand.Read(k[:]) // crypto/rand.Read never fails
		return k
	}
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write([]byte("schwab-mcp: the digests of T~ refs"))
	copy(k[:], mac.Sum(nil))
	return k
}

// permute returns d, the 6 bytes of a T~ ref's digest, permuted under k
// for the ref's time at, or with back the permutation undone: a Feistel
// network over two halves of 3 bytes, whose round function is the first 3
// bytes of HMAC-SHA256 under k of the time, the round and the half.
func (k *tildeKey) permute(d [6]byte, at string, back bool) [6]byte {
	round := func(i byte, half [3]byte) [3]byte {
		mac := hmac.New(sha256.New, k[:])
		mac.Write([]byte(at))
		mac.Write([]byte{0, i})
		mac.Write(half[:])
		return [3]byte(mac.Sum(nil))
	}
	xor := func(a, b [3]byte) [3]byte { return [3]byte{a[0] ^ b[0], a[1] ^ b[1], a[2] ^ b[2]} }
	l, r := [3]byte(d[:3]), [3]byte(d[3:])
	if back {
		for i := byte(tildeRounds); i > 0; i-- {
			l, r = xor(r, round(i-1, l)), l
		}
	} else {
		for i := range byte(tildeRounds) {
			l, r = r, xor(l, round(i, r))
		}
	}
	return [6]byte(append(l[:], r[:]...))
}

// tildeParts is a T~ ref of the store's shape in any case: its time, or
// none for a row without one; its 12 hex digits of digest; and its .2, .3
// and so on.
var tildeParts = regexp.MustCompile(`^[Tt]~([0-9]{8}[Tt][0-9]{6}|[Nn][Oo][Nn][Ee])\.([0-9A-Fa-f]{12})((?:\.[0-9]{1,9})?)$`)

// tildeInText is a T~ ref of the store's shape in text that the store
// composed, as a flag's detail names a row.
var tildeInText = regexp.MustCompile(`T~(?:[0-9]{8}T[0-9]{6}|none)\.[0-9a-f]{12}`)

// tilde returns ref, a T~ ref of the store's shape in any case, as the
// store writes it with its digest permuted, or with back the permutation
// undone; any other ref as it is.
func (m *masker) tilde(ref string, back bool) string {
	p := tildeParts.FindStringSubmatch(ref)
	if p == nil {
		return ref
	}
	at := strings.ToUpper(p[1])
	if at == "NONE" {
		at = "none"
	}
	var d [6]byte
	if _, err := hex.Decode(d[:], []byte(p[2])); err != nil {
		return ref // the pattern takes hex digits only
	}
	d = m.tildes.permute(d, at, back)
	return "T~" + at + "." + hex.EncodeToString(d[:]) + p[3]
}

// ref returns ref, one of the store's refs, as an answer shows it: a T~
// ref with its digest permuted; any other as it is.
func (m *masker) ref(ref string) Ident { return Ident(m.tilde(ref, false)) }

// refPtr returns ref as an answer shows it, or nil for "".
func (m *masker) refPtr(ref string) *Ident {
	if ref == "" {
		return nil
	}
	return new(m.ref(ref))
}

// refList returns refs as an answer shows them, [] for none.
func (m *masker) refList(refs []string) []Ident {
	out := make([]Ident, len(refs))
	for i, r := range refs {
		out[i] = m.ref(r)
	}
	return out
}

// storeRef returns the store's ref of ref, a ref as answers show it, in
// any case: a T~ ref with its digest permuted back, written as the store
// writes it; any other as it is.
func (m *masker) storeRef(ref string) string { return m.tilde(ref, true) }

// tildeText returns s, text that the store composed, with each T~ ref in
// it as an answer shows it.
func (m *masker) tildeText(s string) string {
	if !strings.Contains(s, "T~") {
		return s
	}
	var b strings.Builder
	done := 0
	for _, loc := range tildeInText.FindAllStringIndex(s, -1) {
		if loc[1] < len(s) && isHexDigit(s[loc[1]]) {
			continue // a longer run of hex digits: no ref of the store's
		}
		b.WriteString(s[done:loc[0]])
		b.WriteString(m.tilde(s[loc[0]:loc[1]], false))
		done = loc[1]
	}
	b.WriteString(s[done:])
	return b.String()
}

// isHexDigit reports whether c is a hex digit, in either case.
func isHexDigit(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}
