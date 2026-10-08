package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"unicode/utf8"
)

func validJSON(raw []byte) error {
	bad := errors.New("invalid_json")
	if len(raw) == 0 || len(raw) > maxResponse || !utf8.Valid(raw) {
		return bad
	}
	// Check UTF-16 escape pairs before encoding/json can replace invalid ones.
	for i := 0; i < len(raw); i++ {
		if raw[i] != '"' {
			continue
		}
		i++
		for i < len(raw) && raw[i] != '"' {
			if raw[i] != '\\' {
				i++
				continue
			}
			i++
			if i >= len(raw) {
				return bad
			}
			if raw[i] != 'u' {
				i++
				continue
			}
			if i+4 >= len(raw) {
				return bad
			}
			n, e := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
			if e != nil {
				return bad
			}
			i += 5
			if n >= 0xdc00 && n <= 0xdfff {
				return bad
			}
			if n >= 0xd800 && n <= 0xdbff {
				if i+5 >= len(raw) || raw[i] != '\\' || raw[i+1] != 'u' {
					return bad
				}
				low, e := strconv.ParseUint(string(raw[i+2:i+6]), 16, 16)
				if e != nil || low < 0xdc00 || low > 0xdfff {
					return bad
				}
				i += 6
			}
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	tokens := 0
	var walk func(int) error
	walk = func(depth int) error {
		tokens++
		if depth > 24 || tokens > 100000 {
			return bad
		}
		t, e := d.Token()
		if e != nil {
			return bad
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				s, ok := k.(string)
				if e != nil || !ok || seen[s] {
					return bad
				}
				seen[s] = true
				if e = walk(depth + 1); e != nil {
					return e
				}
			}
			t, e = d.Token()
			if e != nil || t != json.Delim('}') {
				return bad
			}
		case '[':
			for d.More() {
				if e = walk(depth + 1); e != nil {
					return e
				}
			}
			t, e = d.Token()
			if e != nil || t != json.Delim(']') {
				return bad
			}
		default:
			return bad
		}
		return nil
	}
	if walk(0) != nil {
		return bad
	}
	if _, e := d.Token(); e != io.EOF {
		return bad
	}
	return nil
}
