package supervisor

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"longarm/internal/jobs"
)

// bindStdin runs with submit held. The key stays outside job directories:
// neither a job record nor the hub can be used to guess a low-entropy prompt.
func (m *Manager) bindStdin(id string, payload []byte) (string, error) {
	if m.stdinKey == nil {
		dir := filepath.Join(m.opts.JobsDir, ".stdin")
		if err := secureStdinDir(dir); err != nil {
			return "", fmt.Errorf("stdin binding directory: %w", err)
		}
		key, err := os.ReadFile(filepath.Join(dir, "key"))
		if errors.Is(err, os.ErrNotExist) {
			m.mu.Lock()
			bound := false
			for _, e := range m.jobs {
				bound = bound || e.spec.StdinMAC != ""
			}
			m.mu.Unlock()
			if bound {
				return "", fmt.Errorf("%w: stdin binding key is missing; restore it before retrying jobs", ErrInvalid)
			}
			key = make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return "", err
			}
			if err := jobs.Dir(dir).WriteAtomic("key", key); err != nil {
				return "", err
			}
		} else if err != nil {
			return "", err
		}
		if len(key) != 32 {
			return "", fmt.Errorf("%w: invalid stdin binding key", ErrInvalid)
		}
		m.stdinKey = key
	}
	mac := hmac.New(sha256.New, m.stdinKey)
	mac.Write([]byte("longarm-stdin-v1\x00" + id + "\x00"))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil)), nil
}
