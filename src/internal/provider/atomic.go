package provider

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// ReplaceFile syncs a cache snapshot and replaces its previous version with a
// same-directory rename. Crash durability still depends on the filesystem.
// dir must already be a trusted application directory.
func ReplaceFile(dir, name string, data []byte, perm fs.FileMode) error {
	if !validCacheFilename(name) {
		return errors.New("provider cache filename must be a portable base filename")
	}
	if perm&^fs.FileMode(0o777) != 0 {
		return errors.New("provider cache permissions contain non-permission bits")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open provider cache directory: %w", err)
	}
	defer func() { _ = root.Close() }()

	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Errorf("create provider cache temporary name: %w", err)
	}
	temporary := ".blackbeard-" + hex.EncodeToString(random[:])
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("create provider cache temporary file: %w", err)
	}
	keepTemporary := true
	defer func() {
		if keepTemporary {
			_ = root.Remove(temporary)
		}
	}()

	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write provider cache temporary file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync provider cache temporary file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close provider cache temporary file: %w", err)
	}
	if err := root.Rename(temporary, name); err != nil {
		return fmt.Errorf("replace provider cache file: %w", err)
	}
	keepTemporary = false
	return nil
}

func validCacheFilename(name string) bool {
	if len(name) == 0 || len(name) > 128 || !asciiAlnum(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		if !asciiAlnum(name[i]) && name[i] != '.' && name[i] != '-' && name[i] != '_' {
			return false
		}
	}
	if name[len(name)-1] == '.' {
		return false
	}
	base := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
		len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
		return false
	}
	return true
}

func asciiAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
