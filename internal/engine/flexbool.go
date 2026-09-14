package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// flexBool decodes a boolean that the API may send as a number or a string.
//
// The published schema shows is_favorite as `true`, but Metrika sends `1`.
// A strict bool field turns that single mismatch into a failure of the whole
// request, so every boolean read from the API goes through this.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)

	switch {
	case bytes.Equal(data, []byte("null")):
		*b = false
		return nil
	case bytes.Equal(data, []byte("true")):
		*b = true
		return nil
	case bytes.Equal(data, []byte("false")):
		*b = false
		return nil
	}

	// A quoted value ("1", "true") is unwrapped before it is interpreted.
	if len(data) >= 2 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("flexBool: %w", err)
		}
		parsed, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("flexBool: %q is not a boolean", s)
		}
		*b = flexBool(parsed)
		return nil
	}

	var n float64
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("flexBool: cannot read %s as a boolean", data)
	}
	*b = n != 0
	return nil
}

func (b flexBool) MarshalJSON() ([]byte, error) {
	return json.Marshal(bool(b))
}
