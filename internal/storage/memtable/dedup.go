package memtable

// DedupWindow implements an LRU-based idempotent write deduplication
// window (design 01 §2.3). When a _bulk item carries client_id + sequence,
// the MemTable checks this window: if the (client_id, sequence) pair has
// already been seen (sequence <= stored max), the write is a duplicate
// and is silently skipped, returning the previous result.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#1.2.1
type DedupWindow struct {
	entries map[string]uint64 // client_id → max_seq
	results map[string][]byte // client_id → last result (serialized)
	order   []string          // LRU order (oldest first)
	maxLen  int               // capacity (default 1_000_000)
}

// NewDedupWindow creates a dedup window with the given capacity.
func NewDedupWindow(capacity int) *DedupWindow {
	if capacity <= 0 {
		capacity = 1_000_000
	}
	return &DedupWindow{
		entries: make(map[string]uint64),
		results: make(map[string][]byte),
		maxLen:  capacity,
	}
}

// IsDuplicate returns true if (clientID, sequence) is a replay of a
// previously seen write (sequence <= stored max for this clientID).
// A non-duplicate write updates the stored max and refreshes the LRU
// position. Empty clientID always returns false (dedup disabled).
func (w *DedupWindow) IsDuplicate(clientID string, sequence uint64) bool {
	if clientID == "" {
		return false
	}
	if prev, ok := w.entries[clientID]; ok && sequence <= prev {
		return true
	}
	// New or higher sequence: update entry.
	if _, exists := w.entries[clientID]; !exists {
		if len(w.order) >= w.maxLen {
			oldest := w.order[0]
			w.order = w.order[1:]
			delete(w.entries, oldest)
			delete(w.results, oldest)
		}
		w.order = append(w.order, clientID)
	} else {
		// Existing client: refresh LRU position.
		for i, id := range w.order {
			if id == clientID {
				w.order = append(w.order[:i], w.order[i+1:]...)
				break
			}
		}
		w.order = append(w.order, clientID)
	}
	w.entries[clientID] = sequence
	return false
}

// StoreResult saves the result for a client_id so duplicate requests
// can return the original response (design 01 §2.3).
func (w *DedupWindow) StoreResult(clientID string, result []byte) {
	if clientID == "" {
		return
	}
	w.results[clientID] = result
}

// GetResult returns the stored result for a client_id, or nil if
// no result has been stored. Used when IsDuplicate returns true.
func (w *DedupWindow) GetResult(clientID string) []byte {
	if clientID == "" {
		return nil
	}
	return w.results[clientID]
}
