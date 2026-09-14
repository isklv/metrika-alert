package engine

import (
	"strings"
	"testing"
)

// The exact expression matters: it is what the API is asked, and a wrong
// operator or an unquoted value fails silently as "no data" rather than loudly.
func TestURLFilterBuildsTheDocumentedExpression(t *testing.T) {
	tests := []struct {
		name  string
		value string
		match string
		want  string
	}{
		{
			"substring is the default",
			"/checkout", "",
			`EXISTS(ym:pv:URL=@'/checkout')`,
		},
		{
			"substring stated explicitly",
			"/checkout", URLMatchContains,
			`EXISTS(ym:pv:URL=@'/checkout')`,
		},
		{
			"regexp uses its own operator",
			`^/catalog/\d+$`, URLMatchRegexp,
			`EXISTS(ym:pv:URL=~'^/catalog/\\d+$')`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := URLFilter(tc.value, tc.match)
			if err != nil {
				t.Fatalf("URLFilter: %v", err)
			}
			if got != tc.want {
				t.Errorf("URLFilter(%q, %q)\n got: %s\nwant: %s", tc.value, tc.match, got, tc.want)
			}
		})
	}
}

// EXISTS is the whole point: a plain ym:s:startURL filter would only catch
// sessions that began on the page, missing everyone who navigated to it.
func TestURLFilterMatchesSessionsThatTouchedThePage(t *testing.T) {
	got, err := URLFilter("/checkout", URLMatchContains)
	if err != nil {
		t.Fatalf("URLFilter: %v", err)
	}
	if !strings.HasPrefix(got, "EXISTS(") {
		t.Errorf("filter %q does not use EXISTS — it would only match landing pages", got)
	}
	if !strings.Contains(got, "ym:pv:URL") {
		t.Errorf("filter %q does not look at pageview URLs", got)
	}
}

// An unescaped quote would terminate the literal early and change the meaning
// of the expression the API receives.
func TestURLFilterEscapesReservedCharacters(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{`/it's`, `EXISTS(ym:pv:URL=@'/it\'s')`},
		{`/a\b`, `EXISTS(ym:pv:URL=@'/a\\b')`},
		{`/x'; DROP--`, `EXISTS(ym:pv:URL=@'/x\'; DROP--')`},
		// A backslash already before a quote must not be double-processed.
		{`/a\'b`, `EXISTS(ym:pv:URL=@'/a\\\'b')`},
	}
	for _, tc := range tests {
		got, err := URLFilter(tc.value, URLMatchContains)
		if err != nil {
			t.Fatalf("URLFilter(%q): %v", tc.value, err)
		}
		if got != tc.want {
			t.Errorf("URLFilter(%q)\n got: %s\nwant: %s", tc.value, got, tc.want)
		}
	}
}

func TestURLFilterEmptyMeansWholeCounter(t *testing.T) {
	for _, value := range []string{"", "   "} {
		got, err := URLFilter(value, "")
		if err != nil {
			t.Fatalf("URLFilter(%q): %v", value, err)
		}
		if got != "" {
			t.Errorf("URLFilter(%q) = %q, want no filter", value, got)
		}
	}
}

// A pattern that cannot compile should be refused where it is typed, not turn
// into an opaque API rejection at the first check.
func TestURLFilterRejectsBadRegexp(t *testing.T) {
	if _, err := URLFilter("^/catalog/[", URLMatchRegexp); err == nil {
		t.Fatal("accepted an uncompilable regular expression")
	}
}

func TestURLFilterRejectsUnknownMatch(t *testing.T) {
	if _, err := URLFilter("/checkout", "glob"); err == nil {
		t.Fatal("accepted an unknown comparison")
	}
}

// The API caps a filter expression at 2000 characters.
func TestURLFilterRejectsOversizedPattern(t *testing.T) {
	if _, err := URLFilter(strings.Repeat("a", 2100), URLMatchContains); err == nil {
		t.Fatal("accepted a pattern past the API's filter length limit")
	}
}

func TestValidURLMatch(t *testing.T) {
	for _, m := range []string{"", URLMatchContains, URLMatchRegexp} {
		if !ValidURLMatch(m) {
			t.Errorf("%q should be valid", m)
		}
	}
	for _, m := range []string{"glob", "prefix", "CONTAINS"} {
		if ValidURLMatch(m) {
			t.Errorf("%q should be rejected", m)
		}
	}
}

func TestURLFilterLabel(t *testing.T) {
	if got := URLFilterLabel("", ""); got != "весь счётчик" {
		t.Errorf("empty scope label = %q", got)
	}
	if got := URLFilterLabel("/checkout", URLMatchContains); !strings.Contains(got, "/checkout") {
		t.Errorf("label = %q", got)
	}
	if got := URLFilterLabel("^/a", URLMatchRegexp); !strings.Contains(got, "регулярке") {
		t.Errorf("label = %q", got)
	}
}
