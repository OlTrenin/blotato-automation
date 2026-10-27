package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

// strictJSON preserves the input's meaning before the legacy JSON decoder can
// repair malformed Unicode or overwrite duplicate (including case-alias) keys.
func strictJSON(body []byte) bool {
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			continue
		}
		i++
		if i >= len(body) {
			return false
		}
		if body[i] != 'u' {
			continue
		}
		if i+4 >= len(body) {
			return false
		}
		n, err := strconv.ParseUint(string(body[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(body) || body[i+1] != '\\' || body[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(body[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	d := json.NewDecoder(bytes.NewReader(body))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return false
	}
	if !jsonObject(d, 0) {
		return false
	}
	_, err = d.Token()
	return err == io.EOF
}

func jsonObject(d *json.Decoder, depth int) bool {
	if depth > 32 {
		return false
	}
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		key, ok := token.(string)
		// All API fields use canonical lower-case names.
		if err != nil || !ok || key != strings.ToLower(key) || seen[key] {
			return false
		}
		seen[key] = true
		if !jsonValue(d, depth+1) {
			return false
		}
	}
	token, err := d.Token()
	return err == nil && token == json.Delim('}')
}

func jsonValue(d *json.Decoder, depth int) bool {
	if depth > 32 {
		return false
	}
	token, err := d.Token()
	if err != nil {
		return false
	}
	switch token {
	case json.Delim('{'):
		return jsonObject(d, depth)
	case json.Delim('['):
		for d.More() {
			if !jsonValue(d, depth+1) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim(']')
	}
	return true
}
