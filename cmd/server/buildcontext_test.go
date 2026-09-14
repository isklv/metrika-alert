package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The image stamps its revision from the repository that comes in with the
// build context. Excluding .git again would silently return every container to
// reporting nothing useful — which is what made a stale deployment and a
// missing feature look identical.
func TestDockerContextKeepsGitHistory(t *testing.T) {
	for _, line := range strings.Split(readDockerignore(t), "\n") {
		if strings.TrimSpace(line) == ".git" {
			t.Error(".git is excluded from the build context; the image can no longer report its revision")
		}
	}
}

// Secrets and local state must stay out of the image.
func TestDockerContextExcludesLocalState(t *testing.T) {
	ignored := readDockerignore(t)

	for _, pattern := range []string{"/config.yaml", "/.env", "/*.db"} {
		if !strings.Contains(ignored, pattern) {
			t.Errorf("%s is not excluded from the build context", pattern)
		}
	}
}

// Excluding a tracked file makes the checkout inside the image look modified,
// so every build stamps itself +dirty and the marker stops meaning anything.
func TestDockerContextExcludesNothingTracked(t *testing.T) {
	for _, line := range strings.Split(readDockerignore(t), "\n") {
		pattern := strings.TrimSpace(line)
		if pattern == "" || strings.HasPrefix(pattern, "#") {
			continue
		}

		// A leading slash anchors the pattern to the context root, so a match
		// deeper in the tree does not count — /*.db must not be judged by the
		// vendored fixtures it never touches.
		anchored := strings.HasPrefix(pattern, "/")

		cmd := exec.Command("git", "ls-files", "--", strings.TrimPrefix(pattern, "/"))
		cmd.Dir = filepath.Join("..", "..")
		out, err := cmd.Output()
		if err != nil {
			continue
		}

		var hits []string
		for _, file := range strings.Fields(string(out)) {
			if anchored && strings.Contains(file, "/") {
				continue
			}
			hits = append(hits, file)
		}
		if len(hits) > 0 {
			t.Errorf("%q excludes tracked file(s) from the build context, making every image +dirty:\n  %s",
				pattern, strings.Join(hits, "\n  "))
		}
	}
}

func readDockerignore(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", ".dockerignore"))
	if err != nil {
		t.Skipf("no .dockerignore: %v", err)
	}
	return string(data)
}
