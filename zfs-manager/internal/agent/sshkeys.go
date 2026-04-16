package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// sshKeyNameValid restricts key names to a filesystem-safe charset to prevent
// path traversal. Only letters, digits, dashes and underscores are permitted.
func sshKeyNameValid(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
			continue
		default:
			return false
		}
	}
	return true
}

// PrivateKeyPath returns the absolute path of the Ed25519 private key for the
// given name inside dir. It does not create anything.
func PrivateKeyPath(dir, name string) string {
	return filepath.Join(dir, name)
}

// PublicKeyPath returns the absolute path of the OpenSSH-format public key.
func PublicKeyPath(dir, name string) string {
	return filepath.Join(dir, name+".pub")
}

// KnownHostsPath returns the absolute path of the known_hosts file shared
// across all replication tasks on this agent.
func KnownHostsPath(dir string) string {
	return filepath.Join(dir, "known_hosts")
}

// EnsureSSHKey generates an Ed25519 keypair at <dir>/<name> if it does not
// already exist. Returns the OpenSSH authorized_keys line (the single-line
// contents of <name>.pub) so the caller can display it to the user.
//
// The keypair is shelled out to ssh-keygen for OpenSSH-native format; the
// agent must have ssh-keygen installed. This matches the host requirements for
// actually running ssh later.
func (e *Engine) EnsureSSHKey(ctx context.Context, name string) (string, error) {
	if !sshKeyNameValid(name) {
		return "", fmt.Errorf("invalid ssh key name %q: use letters, digits, '-', '_' only", name)
	}
	if e.sshKeyDir == "" {
		return "", fmt.Errorf("ssh key directory not configured")
	}
	if e.sshKeygenPath == "" {
		return "", fmt.Errorf("%w: ssh-keygen not found on host — install openssh-client", ErrCapabilityUnavailable)
	}
	if err := os.MkdirAll(e.sshKeyDir, 0700); err != nil {
		return "", fmt.Errorf("create ssh key dir: %w", err)
	}

	privPath := PrivateKeyPath(e.sshKeyDir, name)
	pubPath := PublicKeyPath(e.sshKeyDir, name)

	if _, err := os.Stat(privPath); os.IsNotExist(err) {
		// #nosec G204,G702 -- name is validated by sshKeyNameValid above
		// (letters/digits/-/_ only, max 64 chars) so the -C comment and -f
		// path cannot be used for argument or command injection.
		cmd := exec.CommandContext(ctx, e.sshKeygenPath,
			"-t", "ed25519",
			"-N", "",
			"-C", "vigil-replication-"+name,
			"-f", privPath,
		)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("ssh-keygen: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		// ssh-keygen already writes 0600 on the private key, but be explicit.
		_ = os.Chmod(privPath, 0600)
		_ = os.Chmod(pubPath, 0644)
	} else if err != nil {
		return "", fmt.Errorf("stat key: %w", err)
	}

	pub, err := os.ReadFile(pubPath) // #nosec G304 -- pubPath built from validated name
	if err != nil {
		return "", fmt.Errorf("read public key: %w", err)
	}
	return strings.TrimSpace(string(pub)), nil
}

// PublicKey returns the OpenSSH authorized_keys line for an already-generated
// keypair, or an error if it does not yet exist.
func (e *Engine) PublicKey(name string) (string, error) {
	if !sshKeyNameValid(name) {
		return "", fmt.Errorf("invalid ssh key name %q", name)
	}
	if e.sshKeyDir == "" {
		return "", fmt.Errorf("ssh key directory not configured")
	}
	pub, err := os.ReadFile(PublicKeyPath(e.sshKeyDir, name)) // #nosec G304 -- validated
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("ssh key %q does not exist — create it first", name)
		}
		return "", fmt.Errorf("read public key: %w", err)
	}
	return strings.TrimSpace(string(pub)), nil
}
