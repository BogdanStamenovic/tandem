// Package pair creates, stores and exchanges the pre-shared key that binds a
// client to its relay.
package pair

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const keyBytes = 32

// Generate returns a new hex-encoded key.
func Generate() (string, error) {
	b := make([]byte, keyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Parse validates a hex key as typed or fetched.
func Parse(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != keyBytes {
		return nil, fmt.Errorf("pair: key must be %d hex characters", keyBytes*2)
	}
	return b, nil
}

// Load reads a key file.
func Load(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(string(b))
}

// Store writes key to path with mode 0600, atomically.
func Store(path, key string) error {
	if _, err := Parse(key); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.TrimSpace(key)+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Export is what a relay hands a client when pairing.
type Export struct {
	Listen string `json:"listen"`
	Key    string `json:"key"`
}

// RemoteBinary is where setup installs the root-owned binary.
const RemoteBinary = "/usr/local/lib/tandem/tandem"

// FetchSSH asks the relay at sshTarget for its address and key by running
// `sudo -n tandem pair --export` there. It uses the invoking user's ssh
// identity: when running as root under sudo it drops to $SUDO_USER for the
// ssh call, because root has no keys for the relay. BatchMode makes it fail
// rather than prompt.
func FetchSSH(sshTarget string) (Export, error) {
	var ex Export
	if sshTarget == "" {
		return ex, errors.New("pair: no ssh target")
	}
	args := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", sshTarget,
		"sudo -n " + shellQuote(RemoteBinary) + " pair --export"}
	if u := os.Getenv("SUDO_USER"); os.Geteuid() == 0 && u != "" && u != "root" {
		args = append([]string{"runuser", "-u", u, "--"}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return ex, fmt.Errorf("pair: ssh %s: %v: %s", sshTarget, err, strings.TrimSpace(errb.String()))
	}
	if err := json.Unmarshal(out.Bytes(), &ex); err != nil {
		return ex, fmt.Errorf("pair: %s did not return a pairing export: %v", sshTarget, err)
	}
	if _, err := Parse(ex.Key); err != nil {
		return ex, fmt.Errorf("pair: %s returned a bad key: %v", sshTarget, err)
	}
	return ex, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
