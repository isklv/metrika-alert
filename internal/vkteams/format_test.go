package vkteams

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFromMarkdown(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bold", "*Триггер:* Ошибки", "<b>Триггер:</b> Ошибки"},
		{"double bold", "**Магазин**", "<b>Магазин</b>"},
		{"code", "`status_code == 500`", "<code>status_code == 500</code>"},
		{"italic", "_нет данных_", "<i>нет данных</i>"},
		{"plain", "Посещения 120", "Посещения 120"},
		{"unmatched marker kept literal", "3 * 4 = 12", "3 * 4 = 12"},
		{
			"metrika text cannot inject markup",
			"*Страница:* <script>alert(1)</script>",
			"<b>Страница:</b> &lt;script&gt;alert(1)&lt;/script&gt;",
		},
		{
			"ampersand in url is escaped",
			"`/checkout?a=1&b=2`",
			"<code>/checkout?a=1&amp;b=2</code>",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FromMarkdown(tc.in); got != tc.want {
				t.Errorf("FromMarkdown(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTruncateMessageKeepsValidUTF8(t *testing.T) {
	// Cyrillic is two bytes per rune: a byte-wise cut would split a character.
	long := strings.Repeat("привет ", MaxMessageRunes)

	got := TruncateMessage(long)

	if !utf8.ValidString(got) {
		t.Fatal("truncated message is not valid UTF-8")
	}
	if n := utf8.RuneCountInString(got); n > MaxMessageRunes {
		t.Errorf("truncated to %d runes, limit is %d", n, MaxMessageRunes)
	}
	if !strings.HasSuffix(got, truncationNotice) {
		t.Error("truncated message does not carry the truncation notice")
	}
}

func TestTruncateMessageLeavesShortTextAlone(t *testing.T) {
	const msg = "🔴 Alert: Магазин — Ошибки чекаута"
	if got := TruncateMessage(msg); got != msg {
		t.Errorf("short message was modified: %q", got)
	}
}
