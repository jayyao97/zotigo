package channels

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type SecretStore struct {
	mu   sync.Mutex
	path string
}

func NewSecretStore(root string) (*SecretStore, error) {
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home: %w", err)
		}
		root = filepath.Join(home, ".zotigo")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, fmt.Errorf("create zotigo directory: %w", err)
	}
	return &SecretStore{path: filepath.Join(root, "channel-secrets.json")}, nil
}

func (s *SecretStore) Get(id string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values, err := s.read()
	if err != nil {
		return "", false, err
	}
	value, ok := values[id]
	return value, ok && value != "", nil
}

func (s *SecretStore) Set(id, secret string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	values, err := s.read()
	if err != nil {
		return err
	}
	if secret == "" {
		delete(values, id)
	} else {
		values[id] = secret
	}
	return s.write(values)
}

func (s *SecretStore) Delete(id string) error { return s.Set(id, "") }

func (s *SecretStore) read() (map[string]string, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read channel secrets: %w", err)
	}
	values := map[string]string{}
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("decode channel secrets: %w", err)
	}
	return values, nil
}

func (s *SecretStore) write(values map[string]string) error {
	data, err := json.Marshal(values)
	if err != nil {
		return err
	}
	temporary := s.path + ".tmp"
	if err := os.WriteFile(temporary, data, 0600); err != nil {
		return fmt.Errorf("write channel secrets: %w", err)
	}
	if err := os.Rename(temporary, s.path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("replace channel secrets: %w", err)
	}
	return os.Chmod(s.path, 0600)
}
