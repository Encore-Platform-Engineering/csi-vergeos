package block

import "sync"

// keyedLock serializes work per string key. The zero value is ready to use.
// Entries are removed once no goroutine holds or waits for the key.
type keyedLock struct {
	mu      sync.Mutex
	entries map[string]*keyedLockEntry
}

type keyedLockEntry struct {
	mu   sync.Mutex
	refs int
}

// lock blocks until the caller owns key and returns the matching unlock func.
func (k *keyedLock) lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.entries == nil {
		k.entries = make(map[string]*keyedLockEntry)
	}
	e := k.entries[key]
	if e == nil {
		e = &keyedLockEntry{}
		k.entries[key] = e
	}
	e.refs++
	k.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.entries, key)
		}
		k.mu.Unlock()
	}
}
