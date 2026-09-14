package main

import (
	"runtime/debug"
	"strings"
	"time"
)

// version is stamped at build time with -ldflags "-X main.version=...".
// Container images set it from a build argument because the image build has no
// git history to read.
var version string

// buildVersion reports which build is running.
//
// Without this the only way to tell a stale deployment from a broken feature is
// to guess — and a container that silently kept an old image looks exactly like
// a command that was never added.
func buildVersion() string {
	if version != "" {
		return version
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}

	// Go stamps VCS data into binaries built inside a repository.
	var revision, stamped string
	var dirty bool
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.time":
			stamped = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if revision == "" {
		return "unknown"
	}

	if len(revision) > 12 {
		revision = revision[:12]
	}
	if dirty {
		revision += "+dirty"
	}
	if t, err := time.Parse(time.RFC3339, stamped); err == nil {
		revision += " от " + t.Local().Format("02.01.2006 15:04")
	}
	return revision
}

// shortVersion trims the build stamp down to the identifier alone.
func shortVersion() string {
	v := buildVersion()
	if i := strings.Index(v, " от "); i > 0 {
		return v[:i]
	}
	return v
}
