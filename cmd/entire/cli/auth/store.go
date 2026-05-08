package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/zalando/go-keyring"
)

const (
	keyringService       = "entire-cli"
	testAuthStoreFileEnv = "ENTIRE_TEST_AUTH_STORE_FILE"
)

// Store manages CLI authentication tokens. Production uses the OS keyring;
// subprocess tests can opt into a file-backed store with testAuthStoreFileEnv.
type Store struct {
	service       string
	testStoreFile string
}

// NewStore returns a Store backed by the system keyring.
func NewStore() *Store {
	return &Store{
		service:       keyringService,
		testStoreFile: os.Getenv(testAuthStoreFileEnv),
	}
}

// NewStoreWithService returns a Store with a custom keyring service name (for testing).
func NewStoreWithService(service string) *Store {
	return &Store{service: service}
}

// SaveToken persists an access token for the given base URL.
func (s *Store) SaveToken(baseURL, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("refusing to save empty token")
	}

	if s.testStoreFile != "" {
		return s.saveTokenToFile(baseURL, token)
	}

	if err := keyring.Set(s.service, baseURL, token); err != nil {
		return fmt.Errorf("save token to keyring: %w", err)
	}

	return nil
}

// GetToken retrieves a stored token for the given base URL.
// Returns an empty string (and no error) if no token is stored.
func (s *Store) GetToken(baseURL string) (string, error) {
	if s.testStoreFile != "" {
		return s.getTokenFromFile(baseURL)
	}

	token, err := keyring.Get(s.service, baseURL)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get token from keyring: %w", err)
	}

	return token, nil
}

// DeleteToken removes a stored token for the given base URL.
func (s *Store) DeleteToken(baseURL string) error {
	if s.testStoreFile != "" {
		return s.deleteTokenFromFile(baseURL)
	}

	err := keyring.Delete(s.service, baseURL)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete token from keyring: %w", err)
	}

	return nil
}

// LookupCurrentToken retrieves the token for the current base URL.
func LookupCurrentToken() (string, error) {
	store := NewStore()
	return store.GetToken(api.BaseURL())
}

func (s *Store) saveTokenToFile(baseURL, token string) error {
	tokens, err := s.readTokenFile()
	if err != nil {
		return err
	}

	serviceTokens := tokens[s.service]
	if serviceTokens == nil {
		serviceTokens = make(map[string]string)
		tokens[s.service] = serviceTokens
	}
	serviceTokens[baseURL] = token

	if err := s.writeTokenFile(tokens); err != nil {
		return err
	}
	return nil
}

func (s *Store) getTokenFromFile(baseURL string) (string, error) {
	tokens, err := s.readTokenFile()
	if err != nil {
		return "", err
	}
	return tokens[s.service][baseURL], nil
}

func (s *Store) deleteTokenFromFile(baseURL string) error {
	tokens, err := s.readTokenFile()
	if err != nil {
		return err
	}
	if serviceTokens := tokens[s.service]; serviceTokens != nil {
		delete(serviceTokens, baseURL)
		if len(serviceTokens) == 0 {
			delete(tokens, s.service)
		}
	}
	return s.writeTokenFile(tokens)
}

func (s *Store) readTokenFile() (map[string]map[string]string, error) {
	data, err := os.ReadFile(s.testStoreFile)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]map[string]string), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read test auth store: %w", err)
	}
	if len(data) == 0 {
		return make(map[string]map[string]string), nil
	}

	var tokens map[string]map[string]string
	if err := json.Unmarshal(data, &tokens); err != nil {
		return nil, fmt.Errorf("parse test auth store: %w", err)
	}
	if tokens == nil {
		tokens = make(map[string]map[string]string)
	}
	return tokens, nil
}

func (s *Store) writeTokenFile(tokens map[string]map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(s.testStoreFile), 0o700); err != nil {
		return fmt.Errorf("create test auth store directory: %w", err)
	}

	data, err := json.Marshal(tokens)
	if err != nil {
		return fmt.Errorf("marshal test auth store: %w", err)
	}
	if err := os.WriteFile(s.testStoreFile, data, 0o600); err != nil {
		return fmt.Errorf("write test auth store: %w", err)
	}
	return nil
}
