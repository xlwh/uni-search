// Package grpc — type conversion between proto messages and esapi types.
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.4
package grpc

import (
	gen "github.com/unisearch-db/unisearch/api/proto/gen"
	"github.com/unisearch-db/unisearch/internal/jsonx"
	"github.com/unisearch-db/unisearch/internal/server/esapi"
)

// ConvertWriteBatch converts proto WriteBatch to []esapi.IndexDoc.
func ConvertWriteBatch(req *gen.WriteBatch) []esapi.IndexDoc {
	docs := make([]esapi.IndexDoc, len(req.Ops))
	for i, op := range req.Ops {
		docs[i] = esapi.IndexDoc{
			Op:     esapi.DocOp(op.Kind),
			Index:  req.Index,
			ID:     op.Id,
			Source: op.Doc,
		}
	}
	return docs
}

// ConvertWriteAck converts []esapi.WriteResult to proto WriteAck.
func ConvertWriteAck(results []esapi.WriteResult) *gen.WriteAck {
	ack := &gen.WriteAck{Success: true}
	ack.Results = make([]*gen.WriteResult, len(results))
	for i, r := range results {
		ack.Results[i] = &gen.WriteResult{
			Id:     r.ID,
			Result: r.Result,
			SeqNo:  r.SeqNo,
		}
		if r.Err != nil {
			ack.Results[i].Result = "error"
		}
	}
	return ack
}

// ConvertSearchResult converts esapi.SearchResult to proto SearchResponse.
func ConvertSearchResult(r *esapi.SearchResult) *gen.SearchResponse {
	hits := make([]*gen.SearchHit, len(r.Hits))
	for i, h := range r.Hits {
		hits[i] = &gen.SearchHit{
			Index:  h.Index,
			Id:     h.ID,
			Score:  h.Score,
			Source: h.Source,
			SeqNo:  h.SeqNo,
		}
	}

	var maxScore float32
	if r.MaxScore != nil {
		maxScore = *r.MaxScore
	}

	var aggs []byte
	if r.Aggregations != nil {
		aggs, _ = jsonx.Marshal(r.Aggregations)
	}

	return &gen.SearchResponse{
		Total:        int32(r.Total),
		Relation:     r.Relation,
		MaxScore:     maxScore,
		Hits:         hits,
		Aggregations: aggs,
	}
}
