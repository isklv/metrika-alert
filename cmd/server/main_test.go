package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/isklv/metrika-alert/internal/config"
)

func TestFindConfig_ExplicitArg(t *testing.T) {
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()

	os.Args = []string{"metrika-alert", "custom.yaml"}
	if got := findConfig(); got != "custom.yaml" {
		t.Errorf("findConfig = %q, want custom.yaml", got)
	}

	// A flag-like first arg is not treated as a config path.
	os.Args = []string{"metrika-alert", "-verbose"}
	if got := findConfig(); got == "-verbose" {
		t.Errorf("findConfig should ignore flag args, got %q", got)
	}
}

func TestFindConfig_HomeDir(t *testing.T) {
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	os.Args = []string{"metrika-alert"}

	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".metrika-alert"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(home, ".metrika-alert", "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("database: x.db\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := findConfig(); got != cfgPath {
		t.Errorf("findConfig = %q, want %q", got, cfgPath)
	}
}

func TestFindConfig_CurrentDir(t *testing.T) {
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	os.Args = []string{"metrika-alert"}

	// No config in HOME.
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("config.yaml", []byte("database: x.db\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := findConfig(); got != "config.yaml" {
		t.Errorf("findConfig = %q, want config.yaml", got)
	}
}

func TestFindConfig_NotFound(t *testing.T) {
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	os.Args = []string{"metrika-alert"}

	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	if got := findConfig(); got != "" {
		t.Errorf("findConfig = %q, want empty", got)
	}
}

func TestHomeDir(t *testing.T) {
	t.Setenv("HOME", "/some/home")
	if got := homeDir(); got != "/some/home" {
		t.Errorf("homeDir = %q, want /some/home", got)
	}

	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "C:\\Users\\u")
	if got := homeDir(); got != "C:\\Users\\u" {
		t.Errorf("homeDir = %q, want USERPROFILE value", got)
	}

	t.Setenv("USERPROFILE", "")
	if got := homeDir(); got != "" {
		t.Errorf("homeDir = %q, want empty", got)
	}
}

func TestOpenDB_AbsolutePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "test.db")
	db, err := openDB(&config.Config{Database: path})
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("db file should exist at %s: %v", path, err)
	}
}

func TestOpenDB_RelativePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	db, err := openDB(&config.Config{Database: "rel.db"})
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()
	want := filepath.Join(home, ".metrika-alert", "rel.db")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("db file should exist at %s: %v", want, err)
	}
}

func TestOpenDB_DirCreationFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A regular file where the db directory should be -> MkdirAll fails.
	blocker := filepath.Join(home, ".metrika-alert")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := openDB(&config.Config{Database: "x.db"}); err == nil {
		t.Error("openDB should fail when the db directory cannot be created")
	}
}
