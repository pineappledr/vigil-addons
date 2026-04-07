package manager

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	pskFileName   = "hub.psk"
	tokenFileName = "hub.token"
)

func generateRandom() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// PSKPath returns the path to the PSK file.
func PSKPath(registryPath string) string {
	return filepath.Join(filepath.Dir(registryPath), pskFileName)
}

// LoadOrGeneratePSK reads the PSK from disk, generating one if absent.
func LoadOrGeneratePSK(registryPath string) (string, error) {
	path := PSKPath(registryPath)
	data, err := os.ReadFile(path)
	if err == nil {
		if psk := strings.TrimSpace(string(data)); psk != "" {
			return psk, nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("reading PSK: %w", err)
	}

	psk, err := generateRandom()
	if err != nil {
		return "", err
	}
	return psk, PersistPSK(registryPath, psk)
}

// PersistPSK writes the PSK to disk with restricted permissions.
func PersistPSK(registryPath, psk string) error {
	path := PSKPath(registryPath)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating PSK directory: %w", err)
	}
	return os.WriteFile(path, []byte(psk+"\n"), 0600)
}

// TokenPath returns the path to the persisted Vigil token file.
func TokenPath(registryPath string) string {
	return filepath.Join(filepath.Dir(registryPath), tokenFileName)
}

// LoadPersistedToken reads a previously rotated Vigil token from disk.
func LoadPersistedToken(registryPath string) string {
	data, err := os.ReadFile(TokenPath(registryPath))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// PersistToken writes the Vigil token to disk.
func PersistToken(registryPath, token string) error {
	path := TokenPath(registryPath)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating token directory: %w", err)
	}
	return os.WriteFile(path, []byte(token+"\n"), 0600)
}
