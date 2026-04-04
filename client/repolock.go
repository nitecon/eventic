package client

import "sync"

// RepoLocks provides per-repository mutual exclusion so that concurrent
// events targeting the same repo are serialized (queued), while events
// for different repos run in parallel.
type RepoLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func NewRepoLocks() *RepoLocks {
	return &RepoLocks{locks: make(map[string]*sync.Mutex)}
}

// Lock acquires the mutex for the given repository. If another goroutine
// holds the lock for the same repo, the caller blocks until it is released.
func (r *RepoLocks) Lock(repo string) {
	r.mu.Lock()
	l, ok := r.locks[repo]
	if !ok {
		l = &sync.Mutex{}
		r.locks[repo] = l
	}
	r.mu.Unlock()
	l.Lock()
}

// Unlock releases the mutex for the given repository.
func (r *RepoLocks) Unlock(repo string) {
	r.mu.Lock()
	l, ok := r.locks[repo]
	r.mu.Unlock()
	if ok {
		l.Unlock()
	}
}
