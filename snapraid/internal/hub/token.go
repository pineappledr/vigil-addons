package hub

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const tokenFileName = "hub.token"

// GenerateToken creates a cryptographically random 32-byte hex token.
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// TokenPath returns the path to the persisted token file, stored alongside
// the registry file in the data directory.
func TokenPath(registryPath string) string {
	return filepath.Join(filepath.Dir(registryPath), tokenFileName)
}

// LoadPersistedToken reads a previously rotated token from disk.
// Returns empty string if the file doesn't exist.
func LoadPersistedToken(registryPath string) string {
	data, err := os.ReadFile(TokenPath(registryPath))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// PersistToken writes the token to disk with restricted permissions.
func PersistToken(registryPath, token string) error {
	path := TokenPath(registryPath)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating token directory: %w", err)
	}
	return os.WriteFile(path, []byte(token+"\n"), 0600)
}
