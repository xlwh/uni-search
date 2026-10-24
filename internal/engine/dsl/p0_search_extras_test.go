package dsl

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestP0_SearchExtras_RescoreParse verifies that rescore JSON is captured
// in SearchExtras.Rescore.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.3.1
func TestP0_SearchExtras_RescoreParse(t *testing.T) {
	body := `{"query": {"match": {"title": "hello"}}, "rescore": [{"query": {"match": {"title": "uni"}}, "window_size": 10, "query_weight": 0.3, "rescore_query_weight": 0.7}]}`
	_, _, _, extras, err := ParseSearchBodyFull([]byte(body))
	require.NoError(t, err)
	assert.NotNil(t, extras.Rescore)
	assert.Nil(t, extras.Collapse)
	assert.Nil(t, extras.PostFilter)
}

// TestP0_SearchExtras_CollapseParse verifies that collapse JSON is captured
// in SearchExtras.Collapse.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.3.2
func TestP0_SearchExtras_CollapseParse(t *testing.T) {
	body := `{"query": {"match": {"title": "hello"}}, "collapse": {"field": "category"}}`
	_, _, _, extras, err := ParseSearchBodyFull([]byte(body))
	require.NoError(t, err)
	assert.NotNil(t, extras.Collapse)
	assert.Nil(t, extras.Rescore)
	assert.Nil(t, extras.PostFilter)
}

// TestP0_SearchExtras_CollapseWithInnerHits verifies collapse with inner_hits.
func TestP0_SearchExtras_CollapseWithInnerHits(t *testing.T) {
	body := `{"query": {"match": {"title": "hello"}}, "collapse": {"field": "category", "inner_hits": {"name": "group", "size": 3}}}`
	_, _, _, extras, err := ParseSearchBodyFull([]byte(body))
	require.NoError(t, err)
	assert.NotNil(t, extras.Collapse)
}

// TestP0_SearchExtras_PostFilterParse verifies that post_filter JSON is
// captured in SearchExtras.PostFilter.
// @spec: ./active_specs/2026-10-22-es-feature-gap-fill/02_spec.md#2.3.3
func TestP0_SearchExtras_PostFilterParse(t *testing.T) {
	body := `{"query": {"match": {"title": "hello"}}, "post_filter": {"term": {"status": "active"}}}`
	_, _, _, extras, err := ParseSearchBodyFull([]byte(body))
	require.NoError(t, err)
	assert.NotNil(t, extras.PostFilter)
	assert.Nil(t, extras.Rescore)
	assert.Nil(t, extras.Collapse)
}

// TestP0_SearchExtras_AllCombined verifies that rescore, collapse, and
// post_filter can all be present in the same search body.
func TestP0_SearchExtras_AllCombined(t *testing.T) {
	body := `{"query": {"match": {"title": "hello"}}, "rescore": [{"query": {"match": {"title": "uni"}}, "window_size": 10}], "collapse": {"field": "category"}, "post_filter": {"term": {"status": "active"}}}`
	_, _, _, extras, err := ParseSearchBodyFull([]byte(body))
	require.NoError(t, err)
	assert.NotNil(t, extras.Rescore)
	assert.NotNil(t, extras.Collapse)
	assert.NotNil(t, extras.PostFilter)
}
