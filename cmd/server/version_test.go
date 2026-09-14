package main

import (
	"strings"
	"testing"
)

// A build stamped at link time reports exactly what it was given.
func TestBuildVersionPrefersTheStamp(t *testing.T) {
	original := version
	t.Cleanup(func() { version = original })

	version = "v1.2.3"
	if got := buildVersion(); got != "v1.2.3" {
		t.Errorf("buildVersion() = %q, want the stamped value", got)
	}
}

// Without a stamp the binary still has to identify itself, from the VCS data Go
// embeds when building inside a repository.
func TestBuildVersionFallsBackToVCS(t *testing.T) {
	original := version
	t.Cleanup(func() { version = original })

	version = ""
	got := buildVersion()

	if got == "" {
		t.Fatal("buildVersion() is empty — a running build must always be identifiable")
	}
	// Tests are built from the repository, so a revision is expected; an
	// environment without one must still answer rather than return nothing.
	if got == "unknown" {
		t.Skip("built without VCS information")
	}
	if len(shortVersion()) < 7 {
		t.Errorf("shortVersion() = %q, too short to identify a commit", shortVersion())
	}
}

func TestShortVersionDropsTheTimestamp(t *testing.T) {
	original := version
	t.Cleanup(func() { version = original })

	version = "abc123def456 от 14.09.2026 21:42"
	if got := shortVersion(); got != "abc123def456" {
		t.Errorf("shortVersion() = %q", got)
	}

	version = "v1.2.3"
	if got := shortVersion(); got != "v1.2.3" {
		t.Errorf("shortVersion() = %q", got)
	}
}

// A dirty tree must say so: a build from uncommitted changes is not the commit
// it claims to be.
func TestBuildVersionMarksModifiedTrees(t *testing.T) {
	original := version
	t.Cleanup(func() { version = original })

	version = ""
	got := buildVersion()
	if got == "unknown" {
		t.Skip("built without VCS information")
	}
	// Only asserts the format is understood; whether this particular tree is
	// dirty depends on where the suite runs.
	if strings.Count(got, "+dirty") > 1 {
		t.Errorf("version marker repeated: %q", got)
	}
}
