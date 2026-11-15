package mcpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// masker is the server's one place that turns text and numbers into what a
// tool may return or a log line may hold. It is the only maker of
// FreeText, MaskedNumber and Path. It is safe for concurrent use: Known is,
// and the rest is set before the server serves.
type masker struct {
	known *mask.Known
	// secrets are the app key and secret, when set, each with what it
	// becomes, the longer first; scrub is (*schwab.Auth).Scrub, which knows
	// them and the tokens too, or nil before there is an Auth.
	secrets [][2]string
	scrub   func(string) string
	// homes are the home directory as given, with links resolved, and on
	// Windows in its short form, each cleaned, the longest first.
	homes []string
	// rateName is the name that schwab.DefaultRateFile gives the app key's
	// rate-limit file, and schwabtx setup saves: rate- and 16 hex digits of
	// the key's SHA-256, the server's own digest, which no account number
	// can be; "" without a key.
	rateName string
	// off turns the backstop off, for a test.
	off bool
}

// newMasker returns a masker that knows home, the user's home directory
// ("" for none), and the app's key and secret ("" when not set). A home
// that is the root of a volume, as / is for some service accounts, is none:
// every path would be under it, and every separator of a log line would be
// taken for it.
func newMasker(home, appKey, appSecret string) *masker {
	m := &masker{known: &mask.Known{}}
	for _, s := range [][2]string{{appKey, "[app key]"}, {appSecret, "[app secret]"}} {
		if s[0] != "" {
			m.secrets = append(m.secrets, s)
		}
	}
	slices.SortStableFunc(m.secrets, func(x, y [2]string) int { return len(y[0]) - len(x[0]) })
	if appKey != "" {
		m.rateName = filepath.Base(schwab.DefaultRateFile(appKey, ""))
	}
	if home != "" {
		home = filepath.Clean(home)
		forms := []string{home}
		if resolved, err := filepath.EvalSymlinks(home); err == nil {
			forms = append(forms, filepath.Clean(resolved))
		}
		if short := shortPath(home); short != "" {
			forms = append(forms, filepath.Clean(short))
		}
		for _, h := range forms {
			if filepath.Dir(h) != h && !slices.Contains(m.homes, h) {
				m.homes = append(m.homes, h)
			}
		}
		slices.SortStableFunc(m.homes, func(x, y string) int { return len(y) - len(x) })
	}
	return m
}

// hide returns s with the app key and secret, and the tokens that the Auth
// last read, replaced. It runs before any masking of digits, which could
// change a secret's text so that it is no longer found whole.
func (m *masker) hide(s string) string {
	for _, sec := range m.secrets {
		s = strings.ReplaceAll(s, sec[0], sec[1])
	}
	if m.scrub != nil {
		s = m.scrub(s)
	}
	return s
}

// free returns text that Schwab, a file or the user wrote as FreeText: its
// secrets replaced, then every run of 8 or more digits masked (mask.Text),
// then every known number.
func (m *masker) free(s string) FreeText {
	return FreeText(m.known.Text(mask.Text(m.hide(s))))
}

// stored returns text that the store keeps as FreeText: its secrets and its
// known numbers replaced, with no pattern, since the store masked it when
// it stored it, and composed its details from masked text and identifiers
// that a pattern would mangle.
func (m *masker) stored(s string) FreeText {
	return FreeText(m.known.Text(m.hide(s)))
}

// masked learns number, a full account number, so that it is masked
// wherever it appears from now on, and returns it as Schwab masks it.
func (m *masker) masked(number string) MaskedNumber {
	m.known.Add(number)
	return MaskedNumber(mask.Number(number))
}

// path returns p as a client may see it: ~/ and the rest under the home
// directory, else only its base name, each component masked as
// mask.FileName masks a name, with the known numbers and the secrets
// replaced; but the name of the app key's rate-limit file is kept whole,
// since mask.FileName would cut its digest where 8 of its hex digits in a
// row are decimal ones, as in about 1 digest in 11. Components are joined
// with "/" on every system.
func (m *masker) path(p string) Path {
	if p == "" {
		return ""
	}
	clean := filepath.Clean(p)
	for _, home := range m.homes {
		rest, ok := under(clean, home)
		if !ok {
			continue
		}
		if rest == "" {
			return "~"
		}
		parts := strings.Split(rest, string(filepath.Separator))
		for i, c := range parts {
			parts[i] = m.component(c)
		}
		return Path("~/" + strings.Join(parts, "/"))
	}
	return Path(m.component(filepath.Base(clean)))
}

// component masks one component of a path.
func (m *masker) component(c string) string {
	if m.rateName != "" && c == m.rateName {
		return m.known.Text(c)
	}
	return m.known.Text(mask.FileName(m.hide(c)))
}

// foldPaths says whether paths name the same file whatever their case, as
// they do on Windows and macOS by default.
var foldPaths = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

// under returns the part of p, a clean path, after home, a clean directory,
// and whether p is home or under it.
func under(p, home string) (string, bool) {
	same := func(a, b string) bool { return a == b || foldPaths && strings.EqualFold(a, b) }
	if same(p, home) {
		return "", true
	}
	prefix := home
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	if len(p) > len(prefix) && same(p[:len(prefix)], prefix) {
		return p[len(prefix):], true
	}
	return "", false
}

// compose returns the text that format and args make, for an error, a
// warning or a note: format is the server's own words, and each arg is
// already safe, one of the kinds or a number, a bool or a duration; any
// other arg, a string included, is masked as free text first. It never
// masks the text whole, which would mangle the dates, IDs and hashes that
// the safe pieces hold.
func (m *masker) compose(format string, args ...any) FreeText {
	safe := make([]any, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case FreeText, MaskedNumber, Path, Ident, Date, Instant, Cursor, errorCode,
			int, int64, uint, uint64, float64, bool, time.Duration:
			safe[i] = v
		case string:
			safe[i] = m.free(v)
		case error:
			safe[i] = m.free(v.Error())
		case fmt.Stringer:
			safe[i] = m.free(v.String())
		default:
			safe[i] = m.free(fmt.Sprint(v))
		}
	}
	return FreeText(fmt.Sprintf(format, safe...))
}

// cleanErr returns e with the secrets and the known numbers replaced in
// its text, or e itself when it holds none: the backstop of an error.
func (m *masker) cleanErr(e *toolErr) *toolErr {
	if m.off {
		return e
	}
	msg, detail := m.known.Text(m.hide(string(e.msg))), m.known.Text(m.hide(string(e.detail)))
	if msg == string(e.msg) && detail == string(e.detail) {
		return e
	}
	return &toolErr{code: e.code, msg: FreeText(msg), detail: FreeText(detail)}
}

// words returns text, the server's own words, such as a constant
// sentence of an error, as FreeText. It never takes text from outside,
// which free masks.
func (m *masker) words(text string) FreeText { return FreeText(text) }

// join returns parts, each already safe, joined by sep, which is the
// server's own words.
func (m *masker) join(parts []FreeText, sep string) FreeText {
	s := make([]string, len(parts))
	for i, p := range parts {
		s[i] = string(p)
	}
	return FreeText(strings.Join(s, sep))
}

// maxDetail is the most characters of a Schwab error's detail that a tool
// error shows.
const maxDetail = 300

// schwabDetail returns what a tool error shows of e, in the form of
// e.Error(), from its fields: Schwab's own words masked as free text, and
// its status, tries, correlation ID and reference as sent, so that the
// user can quote them to Schwab. Its code and the code nested in an OAuth
// error's description are masked as free text too: they come from the
// body's "error" or the gateway's "errorcode", which Schwab may fill with
// a sentence that quotes an account, and mask.Text leaves a code such as
// invalid_grant or protocol.http.TooBigBody as it is, since it changes only
// runs of 8 or more digits. It is cut at 300 characters.
func (m *masker) schwabDetail(e *schwab.Error) FreeText {
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP %d from Schwab", e.Status)
	if e.Tries > 1 {
		fmt.Fprintf(&b, " after %d tries", e.Tries)
	}
	said, code, detail := m.free(e.Said()), m.free(e.Code), m.free(e.Detail)
	switch {
	case said != "" && code != "":
		fmt.Fprintf(&b, ": %s (%s)", said, code)
	case said != "":
		b.WriteString(": " + string(said))
	case detail != "":
		fmt.Fprintf(&b, ": %s (%s)", code, detail)
	case code != "":
		b.WriteString(": " + string(code))
	}
	if e.CorrelID != "" {
		fmt.Fprintf(&b, " (correlation ID %s)", m.stored(e.CorrelID))
	}
	if e.Reference != "" {
		fmt.Fprintf(&b, " (reference #%s)", m.stored(e.Reference))
	}
	s := b.String()
	if utf8.RuneCountInString(s) > maxDetail {
		s = string([]rune(s)[:maxDetail]) + "..."
	}
	return FreeText(s)
}

// json rewrites raw, Schwab's JSON for a record, with mask.JSON and the
// classes of classOf, and the known numbers; see mask.JSON. The secrets are
// replaced in raw first, as free does before it masks digits: mask.JSON
// masks free text by pattern, which would cut a secret that holds a run of
// 8 or more digits so that it is no longer found whole. The key, the secret
// and Schwab's tokens hold no character that JSON escapes, so each is in
// raw as it is; they are replaced again in what mask.JSON wrote, in case
// Schwab escaped one that it needs not.
func (m *masker) json(raw []byte, classOf func([]string) mask.Class) (json.RawMessage, mask.Report, error) {
	out, report, err := mask.JSON([]byte(m.hide(string(raw))), classOf, m.known)
	if err != nil {
		return nil, report, err
	}
	return json.RawMessage(m.hide(string(out))), report, nil
}

// backstop returns b, one JSON value as encode wrote it, with every string,
// keys included, cleared of the secrets and the known numbers, and every
// number whose value is a known number made null, and how many values it
// changed. Everything else stays byte for byte. It should never find
// anything: a find is a bug in the layers above, which the caller logs.
// It fails only on b that is not JSON.
func (m *masker) backstop(b []byte) ([]byte, int, error) {
	if !json.Valid(b) {
		return nil, 0, errors.New("the backstop was given what is not JSON")
	}
	if m.off {
		return b, 0, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out []byte
	done, changed := 0, 0 // b[:done] is in out, or needs no change
	end := 0              // where the last token ended
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		start := end
		end = int(dec.InputOffset())
		var replacement []byte
		switch v := tok.(type) {
		case string:
			if hidden := m.known.Text(m.hide(v)); hidden != v {
				if replacement, err = encode(hidden); err != nil {
					return nil, 0, err
				}
			}
		case json.Number:
			if m.known.HasNumber(v) {
				replacement = []byte("null")
			}
		}
		if replacement == nil {
			continue
		}
		// The token is the last of b[start:end] that is not a separator or
		// space: a string from its quote, a number from its first digit or
		// sign.
		from := start
		if _, ok := tok.(string); ok {
			from += bytes.IndexByte(b[start:end], '"')
		} else {
			from += bytes.IndexAny(b[start:end], "-0123456789")
		}
		out = append(out, b[done:from]...)
		out = append(out, replacement...)
		done = end
		changed++
	}
	if changed == 0 {
		return b, 0, nil
	}
	return append(out, b[done:]...), changed, nil
}

// logValue returns what a log line may show of v, a value of an attribute
// or the message: as text, with the secrets and the home directory
// replaced, an error's text masked as free text, and the known numbers
// replaced.
func (m *masker) logValue(v slog.Value) slog.Value {
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.StringValue(m.logString(v.String(), false))
	case slog.KindInt64, slog.KindUint64, slog.KindFloat64:
		if s := v.String(); m.known.Contains(s) {
			return slog.StringValue(m.known.Text(s))
		}
		return v
	case slog.KindBool, slog.KindDuration, slog.KindTime:
		return v
	case slog.KindGroup:
		as := v.Group()
		masked := make([]slog.Attr, len(as))
		for i, a := range as {
			masked[i] = slog.Attr{Key: m.logString(a.Key, false), Value: m.logValue(a.Value)}
		}
		return slog.GroupValue(masked...)
	}
	switch a := v.Any().(type) {
	case error:
		return slog.StringValue(m.logString(a.Error(), true))
	case fmt.Stringer:
		return slog.StringValue(m.logString(a.String(), false))
	default:
		return slog.StringValue(m.logString(fmt.Sprint(a), false))
	}
}

// logString returns s as a log line may show it: its secrets replaced, the
// home directory written ~, then, when it is outside text such as an
// error's, masked by pattern (mask.Text), and its known numbers replaced.
func (m *masker) logString(s string, outside bool) string {
	s = m.hide(s)
	for _, home := range m.homes {
		s = replaceHome(s, home, "~")
	}
	if outside {
		s = mask.Text(s)
	}
	return m.known.Text(s)
}

// replaceHome returns s with every instance of home, matched without regard
// to case, replaced by repl where it starts a path: where neither a
// separator nor a character of a file's name comes before it, and no
// character of a file's name but a separator comes after it. So a folder
// whose name begins with the home's, /home/alice beside /home/al, and a
// folder of the same name inside another, /mnt/home/al, are left whole.
func replaceHome(s, home, repl string) string {
	if home == "" {
		return s
	}
	hay, needle := strings.ToLower(s), strings.ToLower(home)
	if len(hay) != len(s) || len(needle) != len(home) {
		// A change of case that changes a length cannot be mapped back to
		// s; match exactly instead.
		hay, needle = s, home
	}
	var b strings.Builder
	done, from := 0, 0
	for {
		i := strings.Index(hay[from:], needle)
		if i < 0 {
			break
		}
		start, end := from+i, from+i+len(home)
		from = start + 1
		if start > 0 && inName(s[start-1]) || end < len(s) && inName(s[end]) && !os.IsPathSeparator(s[end]) {
			continue
		}
		b.WriteString(s[done:start])
		b.WriteString(repl)
		done, from = end, end
	}
	if done == 0 {
		return s
	}
	b.WriteString(s[done:])
	return b.String()
}

// inName reports whether c can be part of a path before a home folder: a
// separator, or a letter, digit, dot, dash or underscore of a file's name.
func inName(c byte) bool {
	return os.IsPathSeparator(c) || c == '.' || c == '-' || c == '_' || c >= utf8.RuneSelf ||
		'0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

// logText is the log handler's rewriting of an attribute (log.go): its
// key and its value as logValue makes it.
func (m *masker) logText(a slog.Attr) slog.Attr {
	return slog.Attr{Key: m.logString(a.Key, false), Value: m.logValue(a.Value)}
}
