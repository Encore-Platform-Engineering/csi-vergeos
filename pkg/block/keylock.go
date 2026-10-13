package block

import (
	"context"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// keyedLock serializes work per string key. The zero value is ready to use.
// Entries are removed once no goroutine holds or waits for the key.
type keyedLock struct {
	mu      sync.Mutex
	entries map[string]*keyedLockEntry
}

type keyedLockEntry struct {
	sem  chan struct{} // capacity 1; a token in sem means "held"
	refs int
}

// lock blocks until the caller owns key or ctx ends (Aborted: the caller
// should retry) and returns the matching unlock func.
func (k *keyedLock) lock(ctx context.Context, key string) (unlock func(), err error) {
	k.mu.Lock()
	if k.entries == nil {
		k.entries = make(map[string]*keyedLockEntry)
	}
	e := k.entries[key]
	if e == nil {
		e = &keyedLockEntry{sem: make(chan struct{}, 1)}
		k.entries[key] = e
	}
	e.refs++
	k.mu.Unlock()

	release := func() {
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.entries, key)
		}
		k.mu.Unlock()
	}

	select {
	case e.sem <- struct{}{}:
		return func() {
			<-e.sem
			release()
		}, nil
	case <-ctx.Done():
		release()
		return nil, status.Errorf(codes.Aborted, "waiting for another operation on %s: %v", key, ctx.Err())
	}
}
