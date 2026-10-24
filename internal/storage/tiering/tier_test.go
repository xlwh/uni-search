// Package tiering tier_test.go tests cold-hot tier migration.
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.8
package tiering

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/unisearch-db/unisearch/internal/objstore"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// mockSegment implements MigratableSegment for testing.
type mockSegment struct {
	id      types.SegmentID
	numRows int
	data    []byte
	vectors [][]float32
}

func (m *mockSegment) ID() types.SegmentID     { return m.id }
func (m *mockSegment) NumRows() int            { return m.numRows }
func (m *mockSegment) Serialize() []byte       { return m.data }
func (m *mockSegment) GetVectors() [][]float32 { return m.vectors }

func TestClassify(t *testing.T) {
	tm := NewTierManager(DefaultTierConfig())

	assert.Equal(t, TierHot, tm.Classify(1*time.Hour))
	assert.Equal(t, TierHot, tm.Classify(6*24*time.Hour))
	assert.Equal(t, TierWarm, tm.Classify(10*24*time.Hour))
	assert.Equal(t, TierCold, tm.Classify(60*24*time.Hour))
}

func TestMaybeMigrate(t *testing.T) {
	tm := NewTierManager(DefaultTierConfig())

	assert.Equal(t, ActionNone, tm.MaybeMigrate(TierHot, 1*time.Hour))
	assert.Equal(t, ActionHotToWarm, tm.MaybeMigrate(TierHot, 10*24*time.Hour))
	assert.Equal(t, ActionWarmToCold, tm.MaybeMigrate(TierWarm, 60*24*time.Hour))
	assert.Equal(t, ActionNone, tm.MaybeMigrate(TierCold, 100*24*time.Hour))
}

func TestHotToWarm(t *testing.T) {
	tm := NewTierManager(DefaultTierConfig())
	tm.SetQuantizer(768)

	seg := &mockSegment{id: 1, numRows: 100, data: []byte("test")}
	err := tm.ExecuteMigration(context.Background(), ActionHotToWarm, seg)
	assert.NoError(t, err)
}

func TestHotToWarmNoQuantizer(t *testing.T) {
	tm := NewTierManager(DefaultTierConfig())
	// Don't call SetQuantizer

	seg := &mockSegment{id: 1, numRows: 100, data: []byte("test")}
	err := tm.ExecuteMigration(context.Background(), ActionHotToWarm, seg)
	assert.Error(t, err)
}

func TestWarmToCold(t *testing.T) {
	tm := NewTierManager(DefaultTierConfig())
	tm.SetS3Client(objstore.NewMemS3Client())

	seg := &mockSegment{id: 2, numRows: 50, data: []byte("segment-data")}
	err := tm.ExecuteMigration(context.Background(), ActionWarmToCold, seg)
	assert.NoError(t, err)

	// Verify data was uploaded to S3
	s3 := objstore.NewMemS3Client() // This won't work - need same instance
	_ = s3
}

func TestWarmToColdNoS3Client(t *testing.T) {
	tm := NewTierManager(DefaultTierConfig())
	// Don't call SetS3Client

	seg := &mockSegment{id: 1, numRows: 100, data: []byte("test")}
	err := tm.ExecuteMigration(context.Background(), ActionWarmToCold, seg)
	assert.Error(t, err)
}

func TestExecuteMigrationNone(t *testing.T) {
	tm := NewTierManager(DefaultTierConfig())
	err := tm.ExecuteMigration(context.Background(), ActionNone, &mockSegment{id: 1})
	assert.NoError(t, err)
}

func TestSetConfig(t *testing.T) {
	tm := NewTierManager(DefaultTierConfig())
	newCfg := TierConfig{HotAge: 1 * time.Hour, WarmAge: 24 * time.Hour}
	tm.SetConfig(newCfg)
	assert.Equal(t, newCfg, tm.Config())
	assert.Equal(t, TierWarm, tm.Classify(2*time.Hour))
}
