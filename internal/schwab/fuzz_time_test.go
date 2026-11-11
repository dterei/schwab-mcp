package schwab

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// refInstant and refDate are the forms of a Schwab timestamp, for refTime:
// an instant, with an optional fraction and an offset of Z, ±hhmm or
// ±hh:mm, whose hour may have one digit and whose fraction may follow a
// comma; or a date alone.
var (
	refInstant = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})T(\d{1,2}):(\d{2}):(\d{2})(?:[.,](\d+))?(Z|([+-])(\d{2}):?(\d{2}))$`)
	refDate    = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)
)

// refTime reads s as Schwab writes times, by itself rather than through
// time.Parse as readTime does: an instant whose month, day, hour, minute
// and second are in range, whose offset has hours 00 to 23 and minutes 00
// to 59, and whose year in UTC is 0 to 9999; or a date alone, as midnight
// in New York. It takes the three forms that time.Parse lets through and
// readTime keeps: a one-digit hour, a comma before the fraction, and more
// than nine digits of fraction, cut to nine.
func refTime(s string) (time.Time, bool) {
	num := func(digits string) int {
		n, _ := strconv.Atoi(digits) // the expression matched digits
		return n
	}
	inMonth := func(y, m, d int) bool {
		return m >= 1 && m <= 12 && d >= 1 && d <= time.Date(y, time.Month(m)+1, 0, 0, 0, 0, 0, time.UTC).Day()
	}
	if m := refDate.FindStringSubmatch(s); m != nil {
		y, mo, d := num(m[1]), num(m[2]), num(m[3])
		if !inMonth(y, mo, d) {
			return time.Time{}, false
		}
		return time.Date(y, time.Month(mo), d, 0, 0, 0, 0, newYork()).UTC(), true
	}
	m := refInstant.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, false
	}
	y, mo, d, h, mi, sec := num(m[1]), num(m[2]), num(m[3]), num(m[4]), num(m[5]), num(m[6])
	if !inMonth(y, mo, d) || h > 23 || mi > 59 || sec > 59 {
		return time.Time{}, false
	}
	ns := num((m[7] + "000000000")[:9])
	offset := 0
	if m[8] != "Z" {
		oh, om := num(m[10]), num(m[11])
		if oh > 23 || om > 59 {
			return time.Time{}, false
		}
		offset = (oh*60 + om) * 60
		if m[9] == "-" {
			offset = -offset
		}
	}
	at := time.Date(y, time.Month(mo), d, h, mi, sec, ns, time.FixedZone("", offset)).UTC()
	if at.Year() < 0 || at.Year() > 9999 {
		return time.Time{}, false
	}
	return at, true
}

// checkTime checks what readTime makes of raw, a JSON string that holds s,
// against refTime: the key is present; a time refTime takes is the same
// instant, in UTC, without a problem, and encodes; anything else is the zero
// time and exactly one problem, at the path given.
func checkTime(t *testing.T, raw []byte, s string) {
	t.Helper()
	var ps []Problem
	got, present := readTime(raw, "time", &ps)
	want, ok := refTime(s)
	switch {
	case !present:
		t.Fatalf("readTime(%s) says the key is absent", raw)
	case len(ps) > 1:
		t.Fatalf("readTime(%s) gave %d problems: %q", raw, len(ps), ps)
	case ok && len(ps) > 0:
		t.Fatalf("readTime(%s) refused %v: %q", raw, want, ps)
	case ok && (!got.Equal(want) || got.Location() != time.UTC):
		t.Fatalf("readTime(%s) = %v in %q, want %v in UTC", raw, got, got.Location(), want)
	case !ok && len(ps) == 0:
		t.Fatalf("readTime(%s) = %v, but it is not a time", raw, got)
	case !ok && (!got.IsZero() || ps[0].Path != "time"):
		t.Fatalf("readTime(%s) = %v, %q; want the zero time and a problem at time", raw, got, ps)
	}
	if ok {
		if _, err := json.Marshal(got); err != nil {
			t.Fatalf("readTime(%s) = %v, which does not encode: %v", raw, got, err)
		}
	}
}

// readTime agrees with refTime on any string. The seeds reach each of its
// decisions, so that CI, which runs only the seeds, sees a change in what it
// takes or refuses.
func FuzzReadTime(f *testing.F) {
	for _, s := range []string{
		// The forms Schwab writes.
		"2024-04-04T15:56:07+0000", "2024-04-04T15:56:07.000+0000", "2024-04-04T15:56:07Z",
		"2024-04-04T15:56:07.000Z", "2024-04-04T15:56:07.000+00:00", "2024-04-04T11:56:07-04:00",
		"2024-04-04T11:56:07-0400",
		// Dates alone, on the days the clocks change in New York, and the
		// 29th of February.
		"2024-03-10", "2024-11-03", "2024-02-29", "2023-02-29",
		// Offsets out of range, and the last in range.
		"2024-04-04T15:56:07+2400", "2024-04-04T15:56:07-2460", "2024-04-04T15:56:07+0060",
		"2024-04-04T15:56:07+24:00", "2024-04-04T15:56:07+00:60", "2024-04-04T15:56:07+2359",
		// Times out of range.
		"2024-04-04T24:00:00+0000", "2024-04-04T23:59:60+0000", "2023-02-29T00:00:00+0000",
		// The zero instant, and the edges of the years 0 to 9999 in UTC.
		"0001-01-01T00:00:00Z", "0000-01-01T00:00:00Z", "9999-12-31T23:59:59Z",
		"0000-01-01T00:00:00+0100", "9999-12-31T23:59:59-2359", "0000-01-01", "9999-12-31",
		// The forms time.Parse lets through: a one-digit hour, a comma before
		// the fraction, and more than nine digits of fraction.
		"2024-04-04T5:56:07+0000", "2024-04-04T5:56:07Z", "2024-04-04T15:56:07,5+0000", "2024-04-04T15:56:07,5Z",
		"2024-04-04T15:56:07.1234567891+0000", "2024-04-04T15:56:07.1234567891Z", "2024-04-04T15:56:07.000000000000Z",
		// Near misses, each refused, without which a change that took them
		// would pass: a one-digit minute, second, month or day, in an instant
		// or a date alone; a dot with no digits; an offset of hours alone; a
		// lower-case z or t; a space in place of the T, with an offset or
		// without one, as time.DateTime reads it; a one-digit offset's minute
		// or hour; a space before the offset; a space before or after the
		// time; no seconds, with each form of offset; a fraction with no
		// offset, of one digit or of three as in Schwab's forms; an offset
		// with seconds.
		"2024-04-04T15:5:07+0000", "2024-04-04T15:56:7+0000", "2024-4-04T15:56:07+0000", "2024-04-4T15:56:07+0000",
		"2024-4-04", "2024-04-4", "2024-4-4",
		"2024-04-04T15:56:07.+0000", "2024-04-04T15:56:07+00", "2024-04-04T15:56:07z", "2024-04-04t15:56:07Z",
		"2024-04-04 15:56:07+0000", "2024-04-04 15:56:07Z", "2024-04-04 15:56:07",
		"2024-04-04T15:56:07+00:0", "2024-04-04T15:56:07+5:30", "2024-04-04T15:56:07 +0000",
		" 2024-04-04T15:56:07Z", "2024-04-04T15:56:07Z ",
		"2024-04-04T15:56Z", "2024-04-04T15:56+0000", "2024-04-04T15:56+00:00",
		"2024-04-04T15:56:07.5", "2024-04-04T15:56:07.000", "2024-04-04T15:56:07+05:30:00", "2024-04-04T15:56:07.Z",
		// Not times at all.
		"", "garbage", "2024-04-04T15:56:07", "2024-13-45",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		// A string that is not valid UTF-8 is encoded with U+FFFD in its
		// place, which is no time to readTime or to refTime.
		checkTime(t, raw, s)
	})
}

// readTime takes any JSON value as encoding/json hands it a field's, without
// the space around it: null is absent, without a problem; a string is read
// as FuzzReadTime checks; anything else is present, the zero time and one
// problem.
func FuzzReadTimeAnyJSON(f *testing.F) {
	for _, s := range []string{
		`null`, ` null`, `null `, ` "2024-04-04T15:56:07Z"`, `"2024-04-04T15:56:07+0000"`, `"2024-04-04"`,
		`"2024-04-04T15:56:07+2400"`, `""`, `5`, `-1.5e3`, `true`, `false`, `{}`, `[]`, `{"time":"2024-04-04"}`, `["2024-04-04"]`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if !json.Valid(b) {
			return
		}
		raw := bytes.TrimSpace(b)
		switch {
		case string(raw) == "null":
			var ps []Problem
			if got, present := readTime(raw, "time", &ps); !got.IsZero() || present || ps != nil {
				t.Fatalf("readTime(null) = %v, %v, %q; want nothing", got, present, ps)
			}
		case raw[0] == '"':
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatal(err)
			}
			checkTime(t, raw, s)
		default:
			var ps []Problem
			got, present := readTime(raw, "time", &ps)
			if !got.IsZero() || !present || len(ps) != 1 || ps[0].Path != "time" || !strings.HasSuffix(ps[0].What, " where a time was expected") {
				t.Fatalf("readTime(%s) = %v, %v, %q; want the zero time and one problem", raw, got, present, ps)
			}
		}
	})
}
