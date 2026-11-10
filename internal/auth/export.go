package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ExportRaw returns the complete on-disk credential document while holding
// the same lock used by token refresh and SaveAtomic.
func (a *Auth) ExportRaw() (json.RawMessage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.FilePath == "" {
		return nil, fmt.Errorf("account has no credential file")
	}
	// filepath.Clean deliberately does not turn the trusted FilePath into a
	// user-provided path: callers only ever use the path parsed at startup.
	raw, err := os.ReadFile(filepath.Clean(a.FilePath))
	if err != nil {
		return nil, fmt.Errorf("read credential file: %w", err)
	}
	var document json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("credential file is not valid JSON: %w", err)
	}
	return document, nil
}
