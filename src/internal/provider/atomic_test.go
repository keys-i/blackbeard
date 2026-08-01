package provider

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReplaceFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.xml")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(dir, "catalog.xml", []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("content = %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions = %o", info.Mode().Perm())
	}
}

func TestReplaceFilePreservesPriorStateOnFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "state.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "state.json", "prior")
	if err := os.WriteFile(marker, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(dir, "state.json", []byte("new"), 0o600); err == nil {
		t.Fatal("expected replacement failure")
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "old" {
		t.Fatalf("prior state changed: content=%q error=%v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".blackbeard-") {
			t.Errorf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestReplaceFileRejectsNonPortableNames(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"", ".hidden", "../state", `sub\\state`, "C:state", "state.", "CON", "nul.txt", "COM1.log", strings.Repeat("x", 129)} {
		if err := ReplaceFile(dir, name, []byte("x"), 0o600); err == nil {
			t.Errorf("ReplaceFile accepted %q", name)
		}
	}
}

func FuzzValidCacheFilename(f *testing.F) {
	f.Add("catalog.xml")
	f.Add("../state")
	f.Fuzz(func(t *testing.T, name string) {
		valid := validCacheFilename(name)
		if valid && (name == "" || len(name) > 128 || strings.ContainsAny(name, `/\\:`)) {
			t.Fatalf("unsafe filename accepted: %q", name)
		}
	})
}
