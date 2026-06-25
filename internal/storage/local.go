package storage

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// tmpPrefix is the root directory for transient attachment files.
// Files under this tree are deleted as soon as the email is delivered.
const tmpPrefix = "./tmp/attachments"

// SaveAttachment decodes a base64 string, writes the raw bytes to a
// temporary file under tmpPrefix, and returns the absolute local path.
//
// The file name is prefixed with a UUID to avoid collisions when the
// client sends multiple attachments with the same base name.
func SaveAttachment(base64Str string, fileName string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(base64Str)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}

	// Ensure the tmp directory exists (created once per process lifetime).
	dir := filepath.Clean(tmpPrefix)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir attachments: %w", err)
	}

	// Qualify the file name with a UUID so concurrent saves never collide.
	localName := fmt.Sprintf("%s_%s", uuid.New().String(), filepath.Base(fileName))
	localPath := filepath.Join(dir, localName)

	if err := os.WriteFile(localPath, raw, 0o644); err != nil {
		return "", fmt.Errorf("write attachment: %w", err)
	}

	return localPath, nil
}

// PruneFile removes a single file from disk. It is safe to call with an
// empty string or a path that no longer exists (the error is swallowed in
// those cases so callers can blanket-defer without checking).
func PruneFile(localPath string) error {
	if localPath == "" {
		return nil
	}
	if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("prune file %s: %w", localPath, err)
	}
	return nil
}

// ReadFile reads a local file into memory and returns the bytes.
// The caller is responsible for closing the file descriptor.
func ReadFile(localPath string) ([]byte, error) {
	f, err := os.Open(filepath.Clean(localPath))
	if err != nil {
		return nil, fmt.Errorf("open attachment %s: %w", localPath, err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read attachment %s: %w", localPath, err)
	}
	return data, nil
}
