package esapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/unisearch-db/unisearch/pkg/types"
)

// ScrollManager manages scroll contexts by translating ES scroll
// semantics to PIT + search_after internally (design 04 §6.4).
// ES scroll is NOT maintained as a native context — scroll_id encodes
// the PIT id + sort cursor. POST /_search/scroll advances the
// search_after cursor within the PIT.
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.4
type ScrollManager struct {
	pitMgr *PITManager
}

// NewScrollManager creates a scroll manager backed by the PIT manager.
func NewScrollManager(pitMgr *PITManager) *ScrollManager {
	return &ScrollManager{pitMgr: pitMgr}
}

// scrollCursor is the internal state encoded in scroll_id.
type scrollCursor struct {
	PITID      string `json:"pit_id"`
	SortValues []any  `json:"sort_values"`
	From       int    `json:"from"`
	Body       []byte `json:"body,omitempty"` // original search body for replay
	Index      string `json:"index,omitempty"`
	Size       int    `json:"size,omitempty"`
}

// CreateScroll creates a PIT for the scroll session and returns a
// scroll_id encoding the PIT id + initial cursor + search body.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#2.2.2
func (sm *ScrollManager) CreateScroll(index string, snap types.Snapshot, keepAlive time.Duration, body []byte, size int) (string, error) {
	pitID, err := sm.pitMgr.Create(index, snap)
	if err != nil {
		return "", fmt.Errorf("scroll: create pit: %w", err)
	}
	cursor := scrollCursor{
		PITID:      pitID,
		SortValues: nil,
		From:       0,
		Body:       body,
		Index:      index,
		Size:       size,
	}
	return encodeScrollID(&cursor), nil
}

// AdvanceScroll decodes the scroll_id, retrieves the PIT snapshot,
// and returns the cursor for the next page.
func (sm *ScrollManager) AdvanceScroll(scrollID string) (*scrollCursor, types.Snapshot, string, error) {
	cursor, err := decodeScrollID(scrollID)
	if err != nil {
		return nil, types.Snapshot{}, "", fmt.Errorf("scroll: decode scroll_id: %w", err)
	}
	snap, index, err := sm.pitMgr.Snapshot(cursor.PITID)
	if err != nil {
		return nil, types.Snapshot{}, "", fmt.Errorf("scroll: pit expired: %w", err)
	}
	return cursor, snap, index, nil
}

// DeleteScroll destroys the underlying PIT.
func (sm *ScrollManager) DeleteScroll(scrollID string) error {
	cursor, err := decodeScrollID(scrollID)
	if err != nil {
		return fmt.Errorf("scroll: decode scroll_id: %w", err)
	}
	return sm.pitMgr.Delete(cursor.PITID)
}

// encodeScrollID serializes the cursor as base64 JSON.
func encodeScrollID(c *scrollCursor) string {
	data, _ := json.Marshal(c)
	return base64.URLEncoding.EncodeToString(data)
}

// decodeScrollID deserializes the base64 JSON cursor.
func decodeScrollID(scrollID string) (*scrollCursor, error) {
	data, err := base64.URLEncoding.DecodeString(scrollID)
	if err != nil {
		return nil, err
	}
	var c scrollCursor
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}
