package bot

import (
	"fmt"
	"strings"
	"testing"
)

// Telegram counts UTF-16 code units, so an emoji costs two. Measuring in runes
// let a reply pass a local check and still be rejected as too long.
func TestUTF16Len(t *testing.T) {
	tests := map[string]int{
		"":        0,
		"abc":     3,
		"привет":  6,
		"⭐":       1, // basic plane despite looking exotic
		"📉":       2, // outside it
		"📉 упали": 8,
	}
	for in, want := range tests {
		if got := utf16Len(in); got != want {
			t.Errorf("utf16Len(%q) = %d, want %d", in, got, want)
		}
	}
}

// A long reply is split, not truncated: the goal list exists to be read, and
// cutting it loses exactly the entries someone was looking for.
func TestSplitMessageKeepsEveryLine(t *testing.T) {
	var b strings.Builder
	for i := range 300 {
		fmt.Fprintf(&b, "• Цель номер %d — `goal:%d`\n", i, 200000000+i)
	}
	text := b.String()

	parts := splitMessage(text, telegramMaxUnits, utf16Len)

	if len(parts) < 2 {
		t.Fatalf("got %d parts, expected the text to be split", len(parts))
	}
	for i, part := range parts {
		if n := utf16Len(part); n > telegramMaxUnits {
			t.Errorf("part %d is %d units, over the %d limit", i, n, telegramMaxUnits)
		}
	}

	// Nothing may be lost in the split.
	rejoined := strings.Join(parts, "\n")
	for i := range 300 {
		want := fmt.Sprintf("goal:%d", 200000000+i)
		if !strings.Contains(rejoined, want) {
			t.Fatalf("%s was lost in the split", want)
		}
	}
}

// Breaks land between lines so no entry is cut in half.
func TestSplitMessageBreaksOnLineBoundaries(t *testing.T) {
	var lines []string
	for i := range 200 {
		lines = append(lines, fmt.Sprintf("• Цель %d — goal:%d", i, i))
	}
	parts := splitMessage(strings.Join(lines, "\n"), 200, utf16Len)

	for _, part := range parts {
		for _, line := range strings.Split(part, "\n") {
			if line == "" {
				continue
			}
			if !strings.HasPrefix(line, "• Цель ") || !strings.Contains(line, "goal:") {
				t.Errorf("a line was cut in half: %q", line)
			}
		}
	}
}

func TestSplitMessageLeavesShortTextAlone(t *testing.T) {
	const text = "Цели — Магазин:\n\n• Покупка — goal:42"
	parts := splitMessage(text, telegramMaxUnits, utf16Len)

	if len(parts) != 1 || parts[0] != text {
		t.Errorf("a short reply was altered: %q", parts)
	}
}

// A single line past the limit has no boundary to break on; it must still be
// delivered rather than dropped, and must stay valid UTF-8.
func TestSplitMessageHandlesOneLongLine(t *testing.T) {
	line := strings.Repeat("длинный", 2000)

	parts := splitMessage(line, telegramMaxUnits, utf16Len)

	if len(parts) < 2 {
		t.Fatalf("got %d parts", len(parts))
	}
	for i, part := range parts {
		if n := utf16Len(part); n > telegramMaxUnits {
			t.Errorf("part %d is %d units, over the limit", i, n)
		}
	}
	if strings.Join(parts, "") != line {
		t.Error("content changed while splitting an over-long line")
	}
}

// Emoji must be counted as the platform counts them, or a reply full of them
// passes the check and is still rejected.
func TestSplitMessageCountsEmojiAsTwo(t *testing.T) {
	// 100 lines of an emoji plus text; each emoji costs two units.
	var lines []string
	for range 100 {
		lines = append(lines, "📉 "+strings.Repeat("a", 30))
	}
	parts := splitMessage(strings.Join(lines, "\n"), 500, utf16Len)

	for i, part := range parts {
		if n := utf16Len(part); n > 500 {
			t.Errorf("part %d is %d units, over the 500 limit", i, n)
		}
	}
}

func TestSplitMessageWithNoLimit(t *testing.T) {
	const text = "что угодно"
	if parts := splitMessage(text, 0, utf16Len); len(parts) != 1 || parts[0] != text {
		t.Errorf("a zero limit should leave the text alone, got %q", parts)
	}
}

// The failure this caught in practice: the bot split the Markdown, the VK Teams
// transport then expanded it to HTML, and the tags pushed the result back over
// the limit where it was silently truncated. Splitting must measure the text as
// the transport will actually send it.
func TestSplitMessageUsesTheTransportMeasure(t *testing.T) {
	// A measure that doubles the length, standing in for markup expansion.
	doubling := func(s string) int { return utf16Len(s) * 2 }

	var lines []string
	for i := range 100 {
		lines = append(lines, fmt.Sprintf("• Цель %d — goal:%d", i, 200000000+i))
	}
	text := strings.Join(lines, "\n")

	parts := splitMessage(text, 1000, doubling)

	for i, part := range parts {
		if n := doubling(part); n > 1000 {
			t.Errorf("part %d measures %d once rendered, over the 1000 limit", i, n)
		}
	}
	// Nothing lost.
	rejoined := strings.Join(parts, "\n")
	for i := range 100 {
		if !strings.Contains(rejoined, fmt.Sprintf("goal:%d", 200000000+i)) {
			t.Fatalf("goal %d was lost", i)
		}
	}
}
