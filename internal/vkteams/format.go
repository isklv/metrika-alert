package vkteams

import (
	"html"
	"strings"
	"unicode/utf8"
)

// MaxMessageRunes is the largest message the bot will send. The API rejects
// oversized payloads, and alert cards can grow unbounded with goal lists.
const MaxMessageRunes = 9000

// truncationNotice is appended when a message had to be cut.
const truncationNotice = "\n\n<i>сообщение обрезано</i>"

// FromMarkdown converts the Telegram-flavoured Markdown the alert and report
// builders emit into the HTML subset VK Teams renders.
//
// Escaping happens first so that text coming from Metrika (page titles, URLs,
// trigger conditions) can never inject markup; only the markers this package
// recognises afterwards become tags.
func FromMarkdown(s string) string {
	s = html.EscapeString(s)
	s = convertPaired(s, "**", "b")
	s = convertPaired(s, "*", "b")
	s = convertPaired(s, "`", "code")
	s = convertPaired(s, "_", "i")
	return s
}

// convertPaired replaces balanced marker pairs with an HTML tag. An unmatched
// trailing marker is left as literal text rather than swallowing the rest of
// the message.
func convertPaired(s, marker, tag string) string {
	if !strings.Contains(s, marker) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))

	openTag := "<" + tag + ">"
	closeTag := "</" + tag + ">"

	rest := s
	for {
		start := strings.Index(rest, marker)
		if start < 0 {
			b.WriteString(rest)
			return b.String()
		}
		after := rest[start+len(marker):]
		end := strings.Index(after, marker)
		// No closing marker, or an empty pair like "**" — emit literally.
		if end < 0 {
			b.WriteString(rest)
			return b.String()
		}
		if end == 0 {
			b.WriteString(rest[:start+len(marker)*2])
			rest = after[len(marker):]
			continue
		}

		b.WriteString(rest[:start])
		b.WriteString(openTag)
		b.WriteString(after[:end])
		b.WriteString(closeTag)
		rest = after[end+len(marker):]
	}
}

// TruncateMessage cuts a message to MaxMessageRunes on a rune boundary. Cutting
// by bytes would split a multi-byte character and produce invalid UTF-8, which
// the API rejects outright — and every alert here is written in Russian.
func TruncateMessage(s string) string {
	if utf8.RuneCountInString(s) <= MaxMessageRunes {
		return s
	}

	limit := MaxMessageRunes - utf8.RuneCountInString(truncationNotice)
	count := 0
	for i := range s {
		if count == limit {
			return s[:i] + truncationNotice
		}
		count++
	}
	return s
}
