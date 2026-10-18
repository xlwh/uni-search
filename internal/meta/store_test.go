package meta

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unisearch-db/unisearch/internal/errors"
	"github.com/unisearch-db/unisearch/pkg/types"
)

// buildTestIndexMeta assembles a representative index metadata record
// exercising every FieldDef attribute that persistence must round-trip.
func buildTestIndexMeta() *IndexMeta {
	m := NewMapping()
	m.Dynamic = DynamicStrict

	title := newFieldDef("title", TypeText)
	title.Analyzer = "standard"
	title.CopyTo = []string{"_all"}
	kw := newFieldDef("title.keyword", TypeKeyword)
	kw.IgnoreAbove = 256
	title.Fields = map[string]*FieldDef{"keyword": kw}

	vec := newFieldDef("embedding", TypeDenseVector)
	vec.Dimensions = 4
	vec.Element = ElementFloat32
	vec.Similarity = "cosine"

	created := newFieldDef("created", TypeDate)
	created.Format = "strict_date_optional_time||epoch_millis"
	created.NullValue = "1970-01-01"

	user := newFieldDef("user", TypeObject)
	user.Properties["name"] = newFieldDef("user.name", TypeText)

	notIndexed := newFieldDef("hint", TypeKeyword)
	notIndexed.Index = false

	m.Properties["title"] = title
	m.Properties["embedding"] = vec
	m.Properties["created"] = created
	m.Properties["user"] = user
	m.Properties["hint"] = notIndexed

	return &IndexMeta{
		ID:        types.TenantID(7),
		Name:      "logs-2026.10",
		Mapping:   m,
		Settings:  IndexSettings{NumberOfShards: 2, NumberOfReplicas: 0, RefreshInterval: "1s"},
		Shards:    []types.ShardID{1, 2},
		Aliases:   []string{"logs"},
		CreatedAt: time.Date(2026, 10, 17, 10, 0, 0, 123456789, time.UTC),
	}
}

// dirFileNames lists visible file names in dir (hidden temp files included,
// so atomic-write leftovers cannot hide).
func dirFileNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestR3_IndexMetaPersistence covers the R3 single-node persistence
// contract: save → reload → equal round trip through the atomic
// (temp + fsync + rename + dir fsync) write path, overwrite, list, 404 on
// missing indices, and no leftover temp files.
func TestR3_IndexMetaPersistence(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	require.NoError(t, err)

	want := buildTestIndexMeta()
	require.NoError(t, st.Save(want))

	// atomic write leaves exactly one file: no temp, no partial state
	assert.Equal(t, []string{"logs-2026.10.json"}, dirFileNames(t, dir))

	got, err := st.Load("logs-2026.10")
	require.NoError(t, err)

	// structural equality: re-encoding both records must be byte-identical
	wantJSON, err := json.Marshal(want)
	require.NoError(t, err)
	gotJSON, err := json.Marshal(got)
	require.NoError(t, err)
	assert.JSONEq(t, string(wantJSON), string(gotJSON))

	assert.Equal(t, want.ID, got.ID)
	assert.Equal(t, want.Name, got.Name)
	assert.True(t, want.CreatedAt.Equal(got.CreatedAt), "created_at must survive the round trip")
	assert.Equal(t, want.Settings, got.Settings)
	assert.Equal(t, want.Shards, got.Shards)
	assert.Equal(t, want.Aliases, got.Aliases)

	// mapping tree survives in full: dynamic mode, multi-field, vectors,
	// date format, object recursion and index=false flag
	assert.Equal(t, DynamicStrict, got.Mapping.Dynamic)
	title := got.Mapping.Field("title")
	require.NotNil(t, title)
	assert.Equal(t, TypeText, title.Type)
	assert.Equal(t, "standard", title.Analyzer)
	require.NotNil(t, title.Fields["keyword"])
	assert.Equal(t, TypeKeyword, title.Fields["keyword"].Type)
	assert.Equal(t, 256, title.Fields["keyword"].IgnoreAbove)

	vec := got.Mapping.Field("embedding")
	require.NotNil(t, vec)
	assert.Equal(t, TypeDenseVector, vec.Type)
	assert.Equal(t, 4, vec.Dimensions)
	assert.Equal(t, ElementFloat32, vec.Element)
	assert.Equal(t, "cosine", vec.Similarity)

	date := got.Mapping.Field("created")
	require.NotNil(t, date)
	assert.Equal(t, "strict_date_optional_time||epoch_millis", date.Format)

	assert.NotNil(t, got.Mapping.Field("user.name"))
	hint := got.Mapping.Field("hint")
	require.NotNil(t, hint)
	assert.False(t, hint.Index, "index=false must survive the round trip")

	// overwrite: Save atomically replaces the previous file
	want.Settings.NumberOfShards = 3
	require.NoError(t, st.Save(want))
	got2, err := st.Load("logs-2026.10")
	require.NoError(t, err)
	assert.Equal(t, 3, got2.Settings.NumberOfShards)
	assert.Equal(t, []string{"logs-2026.10.json"}, dirFileNames(t, dir), "overwrite must not leave temp files")

	// a second index shows up in List in name order
	other := buildTestIndexMeta()
	other.Name = "a-second"
	other.Mapping = NewMapping()
	other.Shards = []types.ShardID{9}
	require.NoError(t, st.Save(other))
	metas, err := st.List()
	require.NoError(t, err)
	require.Len(t, metas, 2)
	assert.Equal(t, "a-second", metas[0].Name)
	assert.Equal(t, "logs-2026.10", metas[1].Name)

	// loading a missing index surfaces the 404 index_not_found APIError
	_, err = st.Load("no-such-index")
	apiErr := requireAPIError(t, err)
	assert.Equal(t, 404, apiErr.Status)
	assert.Equal(t, errors.TypeIndexNotFound, apiErr.Type)

	// delete removes the file and the index is gone
	require.NoError(t, st.Delete("a-second"))
	_, err = st.Load("a-second")
	requireAPIError(t, err)
	metas, err = st.List()
	require.NoError(t, err)
	assert.Len(t, metas, 1)
}

// TestR3_StoreSaveFailureLeavesNoFile ensures a failed Save never leaves
// a temp or partial metadata file behind.
func TestR3_StoreSaveFailureLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	require.NoError(t, err)

	m := buildTestIndexMeta()
	m.Mapping.Properties["bad"] = &FieldDef{
		Name: "bad", Type: TypeKeyword, Index: true, DocValues: true,
		NullValue: make(chan int), // not JSON-encodable
	}
	require.Error(t, st.Save(m))
	assert.Empty(t, dirFileNames(t, dir), "failed save must leave no files")
}

// TestR3_StoreRejectsUnsafeIndexNames guards the file-name mapping against
// path traversal and invalid ES index names.
func TestR3_StoreRejectsUnsafeIndexNames(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	require.NoError(t, err)

	for _, name := range []string{"", "../evil", "a/b", "..", "UPPER", "-leading", "trailing-", "a..b"} {
		err := st.Save(&IndexMeta{Name: name})
		require.Errorf(t, err, "index name %q must be rejected", name)
		apiErr, ok := err.(*errors.APIError)
		require.True(t, ok)
		assert.Equal(t, 400, apiErr.Status)
	}
	_, err = st.Load("../evil")
	require.Error(t, err)
	err = st.Delete("a/b")
	require.Error(t, err)
}

// TestR3_MappingJSONDynamicModes pins the ES wire forms of the dynamic
// setting: JSON booleans true/false, the "strict" string, and the
// default-true behavior when omitted or null.
func TestR3_MappingJSONDynamicModes(t *testing.T) {
	decode := func(body string) DynamicMode {
		var m Mapping
		require.NoError(t, json.Unmarshal([]byte(body), &m))
		return m.Dynamic
	}

	assert.Equal(t, DynamicTrue, decode(`{"dynamic": true, "properties": {}}`))
	assert.Equal(t, DynamicFalse, decode(`{"dynamic": false, "properties": {}}`))
	assert.Equal(t, DynamicStrict, decode(`{"dynamic": "strict", "properties": {}}`))
	assert.Equal(t, DynamicTrue, decode(`{"properties": {}}`), "omitted dynamic defaults to true")
	assert.Equal(t, DynamicTrue, decode(`{"dynamic": null, "properties": {}}`))
	require.Error(t, json.Unmarshal([]byte(`{"dynamic": "bogus"}`), &Mapping{}))

	b, err := json.Marshal(&Mapping{Dynamic: DynamicTrue, Properties: map[string]*FieldDef{}})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"dynamic":true`)
	b, err = json.Marshal(&Mapping{Dynamic: DynamicFalse, Properties: map[string]*FieldDef{}})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"dynamic":false`)
	b, err = json.Marshal(&Mapping{Dynamic: DynamicStrict, Properties: map[string]*FieldDef{}})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"dynamic":"strict"`)

	// FieldDef decoding applies the design defaults: index/doc_values=true
	var m Mapping
	require.NoError(t, json.Unmarshal([]byte(`{"properties": {"f": {"type": "keyword"}}}`), &m))
	fd := m.Field("f")
	require.NotNil(t, fd)
	assert.True(t, fd.Index)
	assert.True(t, fd.DocValues)
}
