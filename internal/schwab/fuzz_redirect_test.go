package schwab

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// redirectMessages are the text of CodeFromRedirect's errors: those that
// are fixed, and the start of the one that quotes Schwab's error. A code
// that is part of one is in an error by design.
var redirectMessages = []string{
	"nothing was pasted",
	"that doesn't look like a web address; paste the whole address from the browser's address bar",
	"no code= in that address; paste the address the browser ended up on after you approved",
	"that address is from a different login attempt; start the login over",
	"Schwab reported ",
}

// leaks reports whether msg, an error's text, holds code in a form that an
// error could quote it in: as it is, escaped as a query or a path escapes
// it, or as %q quotes it. Every real code ends in @, which a pasted address
// holds as %40, so a search for the code as it is alone would miss an error
// that quotes the paste. A code under 8 bytes is not looked for, since text
// holds such a string by chance, and nor is one that msg may hold by
// design: one that is part of redirectMessages, or of schwabs, the error and
// description that Schwab put in the address, as they are or as an error
// cuts and quotes them.
func leaks(msg, code string, schwabs ...string) bool {
	if len(code) < 8 {
		return false
	}
	q := strconv.Quote(code)
	forms := []string{code, url.QueryEscape(code), url.PathEscape(code), q[1 : len(q)-1]}
	allowed := slices.Clone(redirectMessages)
	for _, s := range schwabs {
		allowed = append(allowed, s, clip(s, 200), strconv.Quote(clip(s, 200)))
	}
	for _, text := range allowed {
		for _, form := range forms {
			if strings.Contains(text, form) {
				return false
			}
		}
	}
	for _, form := range forms {
		if strings.Contains(msg, form) {
			return true
		}
	}
	return false
}

// errText returns err's text, or "" for no error.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Each form a user may paste, built from a code and a state as Schwab
// sends them, gives the code back. Told to want another state, one that
// holds a state is refused, without quoting the code.
func FuzzCodeFromRedirectBuilt(f *testing.F) {
	// A code in Schwab's form, with ".", "-", "_", "=" and a final "@".
	const code = "C0.ZmFrZS1jb2Rl.code-1-_=@"
	for form := range uint8(7) {
		f.Add(code, "0123456789abcdef0123456789abcdef", form)
	}
	for _, s := range []struct {
		code, state string
		form        uint8
	}{
		{"C0.SECRETCODE", "s1", 0}, // nothing that a query escapes
		{"C0.abc@", "s1", 1},
		{"a+b/c=d&e#f%g?h", "s&t=u", 2},
		{" spaced ", "s", 3},
		{"ünï©ode", "s", 4},
		{"x", "", 0},
		{"code=C0.abc%40", "s1", 6},
		{"different login", "s1", 2}, // part of an error's text
	} {
		f.Add(s.code, s.state, s.form)
	}
	f.Fuzz(func(t *testing.T, code, state string, form uint8) {
		if code == "" {
			return // an address without a code is FuzzCodeFromRedirectAny's
		}
		q := "code=" + url.QueryEscape(code) + "&session=4f6a1c2e-0000-4000-8000-000000000000"
		hasState := state != "" && form%7 != 5
		if hasState {
			q += "&state=" + url.QueryEscape(state)
		}
		var pasted string
		switch form % 7 {
		case 0, 5: // the whole address, and 5 without the state
			pasted = "https://127.0.0.1/?" + q
		case 1: // without its scheme
			pasted = "127.0.0.1/?" + q
		case 2: // the query alone
			pasted = q
		case 3: // with a port and an empty fragment
			pasted = "https://127.0.0.1:8182/?" + q + "#"
		case 4: // in white space and line ends
			pasted = " \t\r\n https://127.0.0.1/?" + q + "\r\n"
		case 6: // the state first, and again after the code if there is one
			pasted = "https://127.0.0.1/?state=" + url.QueryEscape(state) + "&" + q
		}
		if got, err := CodeFromRedirect(pasted, state); err != nil || got != code {
			t.Fatalf("CodeFromRedirect(%q, %q) = %q, %v; want %q", pasted, state, got, err, code)
		}
		got, err := CodeFromRedirect(pasted, state+"x")
		switch {
		case hasState && err == nil:
			t.Fatalf("CodeFromRedirect(%q, %q) = %q, from another login attempt", pasted, state+"x", got)
		case !hasState && (err != nil || got != code):
			t.Fatalf("CodeFromRedirect(%q, %q) = %q, %v; want %q", pasted, state+"x", got, err, code)
		case err != nil && leaks(err.Error(), code):
			t.Fatalf("CodeFromRedirect(%q, %q) quotes the code: %v", pasted, state+"x", err)
		}
	})
}

// Whatever is pasted, CodeFromRedirect gives a code or an error, and white
// space around the paste changes neither. Where it reads an address, the
// code is the one net/url reads there, and a state other than the one
// wanted is refused. No error quotes the code, as net/url reads it or as it
// was pasted.
func FuzzCodeFromRedirectAny(f *testing.F) {
	for _, s := range []string{
		// TestCodeFromRedirect's.
		"https://127.0.0.1/?code=C0.abc%40&session=x&state=s1\n", "https://127.0.0.1/?code=C0.abc%40",
		"https://127.0.0.1/?code=C0.abc%40&state=other", "C0.abc%40", "  C0.abc@  ", "C0.abc%4",
		"127.0.0.1/?code=C0.abc%40&session=x&state=s1", "code=C0.abc%40&state=s1", "code=C0.abc%40&state=other",
		"https://127.0.0.1/?error=access_denied&error_description=nope", "https://127.0.0.1/", "\n",
		// As Schwab sends it.
		"https://127.0.0.1:8182/?code=C0.ZmFrZS1jb2Rl.code-1-_%3D%40&session=4f6a1c2e-0000-4000-8000-000000000000&state=s1",
		// Codes that escaping and form decoding change.
		"https://127.0.0.1/?code=C0.a+b%2Bc%25d%26e%23f&state=s1", "https://127.0.0.1/?code=C0.%C3%BCn%C3%AF+%20spaced%20",
		"code=C0.a&b#c", "%20", "code=C0.A%40%", "https://127.0.0.1/?code=C0.A%ZZ&state=s1",
		// Addresses that net/url refuses, whose errors quote them.
		"https://127.0.0.1 /?code=C0.SECRETCODE%40", "https://127.0.0.1\x7f/?code=C0.SECRETCODE%40",
		"https://[::1/?code=C0.SECRETCODE%40", "https://127.0.0.1:port/?code=C0.SECRETCODE%40", "A://:code=00000000",
		// A code of 8 bytes or more on each path that can refuse it.
		"https://127.0.0.1/?code=C0.SECRETCODE%40&state=other", "code=C0.SECRETCODE%40&state=other",
		"https://127.0.0.1/?error=access_denied&error_description=nope&code=C0.SECRETCODE%40",
		"https://127.0.0.1/?state=s1&code=C0.SECRETCODE%40&state=evil", "https://127.0.0.1/?state=evil&state=s1&code=C0.SECRETCODE%40",
		"https://127.0.0.1/?error=" + strings.Repeat("e", 300) + "&error_description=" + strings.Repeat("d", 300) + "&code=C0.SECRETCODE%40",
		// Schwab's own text holding the code, which an error quotes by design.
		"https://127.0.0.1/?code=C0.abcdefgh&error=C0.abcdefgh", "https://127.0.0.1/?code=different%20login&state=other",
	} {
		f.Add(s, "s1")
	}
	f.Fuzz(func(t *testing.T, pasted, state string) {
		got, err := CodeFromRedirect(pasted, state)
		if err == nil && got == "" {
			t.Fatalf("CodeFromRedirect(%q, %q) gave neither a code nor an error", pasted, state)
		}
		if got2, err2 := CodeFromRedirect(" \t"+pasted+"\r\n", state); got2 != got || errText(err2) != errText(err) {
			t.Fatalf("white space around %q changed %q, %v to %q, %v", pasted, got, err, got2, err2)
		}
		p := strings.TrimSpace(pasted)
		// The address as CodeFromRedirect reads it.
		address := p
		if !strings.Contains(p, "://") {
			if !strings.Contains(p, "code=") {
				return // a bare code, which is taken as it is
			}
			address = "https://pasted/?" + p[strings.Index(p, "?")+1:]
		}
		var schwabs []string
		if u, perr := url.Parse(address); perr != nil {
			if err == nil {
				t.Fatalf("CodeFromRedirect(%q, %q) = %q, from an address net/url refuses: %v", pasted, state, got, perr)
			}
		} else {
			q := u.Query()
			code := q.Get("code")
			if s := q.Get("state"); err == nil && (got != code || s != "" && s != state) {
				t.Fatalf("CodeFromRedirect(%q, %q) = %q, where net/url reads the code %q and the state %q", pasted, state, got, code, s)
			}
			schwabs = []string{q.Get("error"), q.Get("error_description")}
			if err != nil && leaks(err.Error(), code, schwabs...) {
				t.Fatalf("CodeFromRedirect(%q, %q) quotes the code %q: %v", pasted, state, code, err)
			}
		}
		// The code as it was pasted, up to the next parameter.
		if _, after, ok := strings.Cut(p, "code="); ok && err != nil {
			raw, _, _ := strings.Cut(after, "&")
			if leaks(err.Error(), raw, schwabs...) {
				t.Fatalf("CodeFromRedirect(%q, %q) quotes the code as pasted, %q: %v", pasted, state, raw, err)
			}
		}
	})
}
