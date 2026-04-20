package hub

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	tokenFileName = "hub.token"
	pskFileName   = "hub.psk"
)

// generateRandom creates a cryptographically random 32-byte hex string.
func generateRandom() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// GenerateToken creates a cryptographically random 32-byte hex token.
func GenerateToken() (string, error) {
	return generateRandom()
}

// TokenPath returns the path to the persisted Vigil token file.
func TokenPath(registryPath string) string {
	return filepath.Join(filepath.Dir(registryPath), tokenFileName)
}

// LoadPersistedToken reads a previously rotated Vigil token from disk.
// Returns empty string if the file doesn't exist.
func LoadPersistedToken(registryPath string) string {
	data, err := os.ReadFile(TokenPath(registryPath))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// PersistToken writes the Vigil token to disk with restricted permissions.
func PersistToken(registryPath, token string) error {
	path := TokenPath(registryPath)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating token directory: %w", err)
	}
	return os.WriteFile(path, []byte(token+"\n"), 0600)
}

// PSKPath returns the path to the persisted agent PSK file.
func PSKPath(registryPath string) string {
	return filepath.Join(filepath.Dir(registryPath), pskFileName)
}

// LoadOrGeneratePSK reads the PSK from disk. If the file does not exist, a new
// PSK is generated and persisted before being returned.
func LoadOrGeneratePSK(registryPath string) (string, error) {
	path := PSKPath(registryPath)

	data, err := os.ReadFile(path)
	if err == nil {
		if psk := strings.TrimSpace(string(data)); psk != "" {
			return psk, nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("reading PSK file: %w", err)
	}

	psk, err := generateRandom()
	if err != nil {
		return "", err
	}

	if err := PersistPSK(registryPath, psk); err != nil {
		return "", err
	}

	return psk, nil
}

// PersistPSK writes the PSK to disk with restricted permissions.
func PersistPSK(registryPath, psk string) error {
	path := PSKPath(registryPath)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating PSK directory: %w", err)
	}
	return os.WriteFile(path, []byte(psk+"\n"), 0600)
}
