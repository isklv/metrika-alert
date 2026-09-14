package bot

import "strings"

// Telegram counts a message in UTF-16 code units, not runes or bytes: its
// limit is 4096. Cyrillic costs one unit per character but an emoji costs two,
// and this service's replies are full of both.
const telegramMaxUnits = 4000

// positionMarkerReserve is the room kept for the "_(n/m)_" suffix a split
// reply carries. Wide enough for three-digit part counts.
const positionMarkerReserve = 20

// utf16Len reports the length as the chat platforms measure it.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		// Anything outside the basic plane — emoji, mostly — takes two units.
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// splitMessage breaks text into parts that each fit within limit.
//
// A long reply is split rather than truncated: the goal list exists to be read,
// and cutting it off loses exactly the entries someone was looking for. Breaks
// land on line boundaries so no entry is cut in half.
func splitMessage(text string, limit int, measure func(string) int) []string {
	if measure == nil {
		measure = utf16Len
	}
	if limit <= 0 || measure(text) <= limit {
		return []string{text}
	}

	var parts []string
	var current []string

	flush := func() {
		if len(current) > 0 {
			parts = append(parts, strings.Join(current, "\n"))
			current = nil
		}
	}

	for _, line := range strings.Split(text, "\n") {
		// A single line past the limit has no boundary to break on, so it is
		// cut on rune boundaries instead of being dropped.
		if measure(line) > limit {
			flush()
			parts = append(parts, hardSplit(line, limit, measure)...)
			continue
		}

		// The candidate is measured whole rather than accumulated from parts:
		// a transport's measure need not treat a newline as one unit, and
		// assuming it did pushed the first part past the limit.
		candidate := append(append([]string(nil), current...), line)
		if len(current) > 0 && measure(strings.Join(candidate, "\n")) > limit {
			flush()
		}
		current = append(current, line)
	}
	flush()

	if len(parts) == 0 {
		return []string{""}
	}
	return parts
}

// hardSplit cuts an over-long line on rune boundaries. Cutting by bytes would
// split a multi-byte character and produce invalid UTF-8.
func hardSplit(line string, limit int, measure func(string) int) []string {
	var parts []string
	var current strings.Builder

	for _, r := range line {
		candidate := current.String() + string(r)
		if measure(candidate) > limit && current.Len() > 0 {
			parts = append(parts, current.String())
			current.Reset()
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}
