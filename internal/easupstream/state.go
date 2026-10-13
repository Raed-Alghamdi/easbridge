package easupstream

import (
	"context"
	"sync"

	"github.com/hstern/go-activesync/eas"
)

// stagedState is the go-activesync StateStore. It never touches the disk.
//
// The library writes a new SyncKey as soon as it parses a response, before the
// caller has stored the items that came with it, so a crash in between would
// lose them for good (the server does not resend changes covered by an accepted
// key). Here every write only stages the key in memory. The caller primes the
// key it has committed before a call and takes the staged key afterwards; the
// engine commits it in the same database transaction as the items.
//
// It also records when the library reset a key to "0" (its silent answer to
// InvalidSyncKey), which the engine must know to reconcile deletions.
type stagedState struct {
	mu     sync.Mutex
	policy string
	keys   map[string]string
	resets map[string]bool
}

func newStagedState(policyKey string) *stagedState {
	return &stagedState{policy: policyKey, keys: map[string]string{}, resets: map[string]bool{}}
}

// prime sets the committed key a call starts from and clears the reset mark.
func (s *stagedState) prime(folder, key string) {
	if key == "" {
		key = "0"
	}
	s.mu.Lock()
	s.keys[folder] = key
	delete(s.resets, folder)
	s.mu.Unlock()
}

// take returns the key the library left staged and whether it reset the key to
// "0" during the call.
func (s *stagedState) take(folder string) (key string, reset bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key = s.keys[folder]
	if key == "" {
		key = "0"
	}
	return key, s.resets[folder]
}

func (s *stagedState) policyKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policy
}

// PolicyKey implements eas.StateStore.
func (s *stagedState) PolicyKey(context.Context) (string, error) { return s.policyKey(), nil }

// SetPolicyKey implements eas.StateStore.
func (s *stagedState) SetPolicyKey(_ context.Context, key string) error {
	s.mu.Lock()
	s.policy = key
	s.mu.Unlock()
	return nil
}

// SyncKey implements eas.StateStore.
func (s *stagedState) SyncKey(_ context.Context, folderID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k, ok := s.keys[folderID]; ok && k != "" {
		return k, nil
	}
	return "0", nil
}

// SetSyncKey implements eas.StateStore. Writing "0" is the library resetting a
// folder after InvalidSyncKey.
func (s *stagedState) SetSyncKey(_ context.Context, folderID, key string) error {
	s.mu.Lock()
	s.keys[folderID] = key
	if key == "0" {
		s.resets[folderID] = true
	}
	s.mu.Unlock()
	return nil
}

var _ eas.StateStore = (*stagedState)(nil)
