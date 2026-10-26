package nspawn

import (
	"sync"
)

type taskStore struct {
	store map[string]*taskHandle
	lock  sync.RWMutex
}

func newTaskStore() *taskStore {
	return &taskStore{store: map[string]*taskHandle{}}
}

func (ts *taskStore) Set(id string, handle *taskHandle) {
	ts.lock.Lock()
	defer ts.lock.Unlock()
	ts.store[id] = handle
}

func (ts *taskStore) Get(id string) (*taskHandle, bool) {
	ts.lock.RLock()
	defer ts.lock.RUnlock()
	t, ok := ts.store[id]
	return t, ok
}

func (ts *taskStore) Delete(id string) {
	ts.lock.Lock()
	defer ts.lock.Unlock()
	delete(ts.store, id)
}

// OwnsScratchPrefix reports whether a task in the store owns the scratch
// image prefix.
func (ts *taskStore) OwnsScratchPrefix(prefix string) bool {
	ts.lock.RLock()
	defer ts.lock.RUnlock()
	for _, h := range ts.store {
		if h.taskConfig != nil && fleetScratchPrefix(h.taskConfig.AllocID, h.taskConfig.Name) == prefix {
			return true
		}
	}
	return false
}
