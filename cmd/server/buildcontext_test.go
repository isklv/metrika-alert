package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The image stamps its revision from the repository that comes in with the
// build context. Excluding .git again would silently return every container to
// reporting nothing useful — which is what made a stale deployment and a
// missing feature look identical.
func TestDockerContextKeepsGitHistory(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".dockerignore"))
	if err != nil {
		t.Skipf("no .dockerignore: %v", err)
	}

	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == ".git" {
			t.Error(".git is excluded from the build context; the image can no longer report its revision")
		}
	}
}

// Secrets and local state must stay out of the image.
func TestDockerContextExcludesLocalState(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".dockerignore"))
	if err != nil {
		t.Skipf("no .dockerignore: %v", err)
	}
	ignored := string(data)

	for _, pattern := range []string{"config.yaml", "*.db"} {
		if !strings.Contains(ignored, pattern) {
			t.Errorf("%s is not excluded from the build context", pattern)
		}
	}
}
