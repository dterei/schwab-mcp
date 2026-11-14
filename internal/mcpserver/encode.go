package mcpserver

import (
	"bytes"
	"encoding/json"
)

// encode returns v as compact JSON, its fields in the order they are
// declared, with HTML's characters as they are, so that S&P 500 reads
// "S&P 500" rather than "S&P 500" in the text a model reads. It is the
// one encoder of what the server returns: json.Marshal escapes them.
func encode(v any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
