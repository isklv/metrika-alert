package engine

import (
	"encoding/json"
	"testing"
)

// The published schema shows is_favorite as `true`; the live API sends `1`.
// Both have to decode, or one field mismatch fails the whole request.
func TestFlexBoolAcceptsWhatTheAPIActuallySends(t *testing.T) {
	tests := []struct {
		json string
		want bool
	}{
		{`true`, true},
		{`false`, false},
		{`1`, true},
		{`0`, false},
		{`2`, true},
		{`"true"`, true},
		{`"false"`, false},
		{`"1"`, true},
		{`"0"`, false},
		{`null`, false},
	}
	for _, tc := range tests {
		var b flexBool
		if err := json.Unmarshal([]byte(tc.json), &b); err != nil {
			t.Errorf("unmarshal %s: %v", tc.json, err)
			continue
		}
		if bool(b) != tc.want {
			t.Errorf("unmarshal %s = %v, want %v", tc.json, bool(b), tc.want)
		}
	}
}

func TestFlexBoolRejectsNonsense(t *testing.T) {
	for _, in := range []string{`"нет"`, `{}`, `[]`} {
		var b flexBool
		if err := json.Unmarshal([]byte(in), &b); err == nil {
			t.Errorf("unmarshal %s was accepted", in)
		}
	}
}

func TestFlexBoolMarshalsAsBoolean(t *testing.T) {
	data, err := json.Marshal(flexBool(true))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Clients of our own API get a real boolean, whatever Metrika sent.
	if string(data) != "true" {
		t.Errorf("marshal = %s, want true", data)
	}
}
