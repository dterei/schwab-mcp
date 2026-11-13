package schwabfile

import (
	"strings"
	"unicode/utf8"
)

// cp1252 maps the bytes 0x80 to 0x9F of Windows-1252 to the characters
// they stand for. The five bytes that Windows-1252 leaves undefined (0x81,
// 0x8D, 0x8F, 0x90, 0x9D) stand for the control characters of the same
// number, as the WHATWG's encoding standard reads them, so that every byte
// decodes to one character. Every other byte is the Latin-1 character of
// the same number.
var cp1252 = [32]rune{
	'€', '\u0081', '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', '\u008d', 'Ž', '\u008f',
	'\u0090', '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', '\u009d', 'ž', 'Ÿ',
}

// decodeCP1252 returns b, text in Windows-1252, as UTF-8.
func decodeCP1252(b []byte) string {
	var s strings.Builder
	s.Grow(len(b) + len(b)/8)
	for _, c := range b {
		switch {
		case c < 0x80:
			s.WriteByte(c)
		case c < 0xA0:
			s.WriteRune(cp1252[c-0x80])
		default:
			s.WriteRune(rune(c))
		}
	}
	return s.String()
}

// decode returns content as text: as UTF-8 when it is valid UTF-8, else
// as Windows-1252, which a 2022 History export was written in. It says
// which, and whether a UTF-8 byte order mark, which it takes off, came
// first.
func decode(content []byte) (text, encoding string, bom bool) {
	if rest, ok := strings.CutPrefix(string(content), "\xef\xbb\xbf"); ok {
		content, bom = []byte(rest), true
	}
	if utf8.Valid(content) {
		return string(content), encodingUTF8, bom
	}
	return decodeCP1252(content), encodingCP1252, bom
}

// The encodings that decode reads.
const (
	encodingUTF8   = "utf-8"
	encodingCP1252 = "windows-1252"
)
