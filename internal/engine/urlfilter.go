package engine

import (
	"fmt"
	"regexp"
	"strings"
)

// How a rule's URL filter is compared against a page address.
const (
	// URLMatchContains matches any page whose address contains the value.
	URLMatchContains = "contains"
	// URLMatchRegexp matches the address against a regular expression.
	URLMatchRegexp = "regexp"
)

// Metrika's operators for these comparisons.
const (
	opContains = "=@"
	opRegexp   = "=~"
)

// maxFilterLength is the API's ceiling on a filter expression.
const maxFilterLength = 2000

// ValidURLMatch reports whether m names a supported comparison. An empty value
// means "contains", which is what a bare substring in a rule implies.
func ValidURLMatch(m string) bool {
	switch m {
	case "", URLMatchContains, URLMatchRegexp:
		return true
	}
	return false
}

// URLFilter builds the Reporting API filter that narrows a report to sessions
// which touched a matching page.
//
// EXISTS is what makes this mean what a person expects. A plain
// ym:s:startURL filter would only catch sessions that *began* on the page,
// missing everyone who navigated to it — for a checkout page that is almost
// everyone. EXISTS(ym:pv:URL …) instead keeps every session containing at
// least one view of a matching page.
//
// The consequence worth knowing: the metric is still measured over the whole
// session. "visits with a view of /checkout" is exactly right, while
// "pageviews" counts every page those sessions saw, not only /checkout.
func URLFilter(value, match string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}

	var op string
	switch match {
	case "", URLMatchContains:
		op = opContains
	case URLMatchRegexp:
		// Compiling here is not the same engine the API uses, but it catches
		// the everyday mistakes before they become an opaque API rejection.
		if _, err := regexp.Compile(value); err != nil {
			return "", fmt.Errorf("некорректное регулярное выражение %q: %w", value, err)
		}
		op = opRegexp
	default:
		return "", fmt.Errorf("неизвестный способ сравнения URL %q: доступны %s и %s",
			match, URLMatchContains, URLMatchRegexp)
	}

	filter := "EXISTS(ym:pv:URL" + op + quoteFilterValue(value) + ")"
	if len(filter) > maxFilterLength {
		return "", fmt.Errorf("фильтр по URL длиннее %d символов, которые принимает API", maxFilterLength)
	}
	return filter, nil
}

// filterValueEscaper escapes the two characters the API reserves inside a
// quoted value. The backslash is listed first so the replacer does not double
// back over escapes it just produced.
var filterValueEscaper = strings.NewReplacer(`\`, `\\`, `'`, `\'`)

// quoteFilterValue renders a value as a quoted API string literal.
func quoteFilterValue(v string) string {
	return "'" + filterValueEscaper.Replace(v) + "'"
}

// URLFilterLabel renders a rule's scope for a human.
func URLFilterLabel(value, match string) string {
	if strings.TrimSpace(value) == "" {
		return "весь счётчик"
	}
	if match == URLMatchRegexp {
		return "URL по регулярке " + value
	}
	return "URL содержит " + value
}
