// Package escompat implements the M3 uniadm es-compat tool (design
// 04-es-compat-layer.md §9). It performs static analysis of ES mappings
// and query logs to assess compatibility with UniSearch, producing a
// report with issues, coverage scores, and migration suggestions.
// @spec: ./archive/2026-10-20-m3-ecosystem-advanced/02_spec.md#2.5
package escompat

import (
	"encoding/json"
	"fmt"
	"sort"
)

// CompatReport is the output of an es-compat scan.
type CompatReport struct {
	IndexName     string
	MappingScore  float64   // 0-1 compatibility score
	QueryCoverage float64   // 0-1 query compatibility
	Issues        []CompatIssue
	Suggestions   []string
}

// CompatIssue is one compatibility problem found during scanning.
type CompatIssue struct {
	Field    string
	Type     string   // "unsupported_type" | "unsupported_query" | "mapping_conflict"
	Detail   string
	Severity string   // "error" | "warning" | "info"
}

// ScanMapping analyzes an ES mapping JSON and returns a compatibility report.
func ScanMapping(indexName string, mappingJSON []byte) (*CompatReport, error) {
	var mapping struct {
		Mappings struct {
			Properties map[string]map[string]any `json:"properties"`
		} `json:"mappings"`
	}
	if err := json.Unmarshal(mappingJSON, &mapping); err != nil {
		return nil, fmt.Errorf("escompat: parse mapping: %w", err)
	}

	report := &CompatReport{IndexName: indexName}
	props := mapping.Mappings.Properties
	if len(props) == 0 {
		report.MappingScore = 0
		report.Issues = append(report.Issues, CompatIssue{
			Type: "mapping_conflict", Detail: "no properties found", Severity: "error",
		})
		return report, nil
	}

	supportedTypes := map[string]bool{
		"text": true, "keyword": true, "long": true, "double": true,
		"boolean": true, "date": true, "dense_vector": true,
		"ip": true, "geo_point": true, "object": true,
	}
	unsupportedTypes := map[string]string{
		"nested":      "use object type or flatten manually",
		"geo_shape":   "supported in M3 (W7.3)",
		"sparse_vector": "supported in M1",
		"search_as_you_type": "use text + completion suggester",
	}

	totalFields := 0
	supportedFields := 0
	for field, def := range props {
		totalFields++
		fieldType, _ := def["type"].(string)
		if supportedTypes[fieldType] {
			supportedFields++
			continue
		}
		if suggestion, ok := unsupportedTypes[fieldType]; ok {
			severity := "warning"
			if fieldType == "nested" {
				severity = "error"
			}
			report.Issues = append(report.Issues, CompatIssue{
				Field: field, Type: "unsupported_type",
				Detail: fmt.Sprintf("type %q: %s", fieldType, suggestion),
				Severity: severity,
			})
		} else if fieldType != "" {
			report.Issues = append(report.Issues, CompatIssue{
				Field: field, Type: "unsupported_type",
				Detail: fmt.Sprintf("unknown type %q", fieldType),
				Severity: "warning",
			})
		}
	}

	if totalFields > 0 {
		report.MappingScore = float64(supportedFields) / float64(totalFields)
	}

	if report.MappingScore < 0.8 {
		report.Suggestions = append(report.Suggestions,
			"Consider modifying mapping to use supported types before migration")
	}

	sort.Slice(report.Issues, func(i, j int) bool {
		return report.Issues[i].Severity < report.Issues[j].Severity
	})

	return report, nil
}

// ScanQueries analyzes query log entries and returns query compatibility.
func ScanQueries(queryLogs []string) (float64, []CompatIssue) {
	supportedQueries := map[string]bool{
		"match": true, "match_phrase": true, "term": true, "terms": true,
		"range": true, "exists": true, "bool": true, "knn": true,
		"hybrid": true, "match_all": true,
	}
	total := len(queryLogs)
	if total == 0 {
		return 1.0, nil
	}
	supported := 0
	var issues []CompatIssue
	for _, log := range queryLogs {
		var top map[string]json.RawMessage
		if err := json.Unmarshal([]byte(log), &top); err != nil {
			continue
		}
		if q, ok := top["query"]; ok {
			var queryMap map[string]any
			if json.Unmarshal(q, &queryMap) == nil {
				for qType := range queryMap {
					if supportedQueries[qType] {
						supported++
					} else {
						issues = append(issues, CompatIssue{
							Type: "unsupported_query",
							Detail: fmt.Sprintf("query type %q not supported", qType),
							Severity: "warning",
						})
					}
				}
			}
		}
	}
	return float64(supported) / float64(total), issues
}

// Run performs a full es-compat scan (mapping + queries).
func Run(indexName string, mappingJSON []byte, queryLogs []string) (*CompatReport, error) {
	report, err := ScanMapping(indexName, mappingJSON)
	if err != nil {
		return nil, err
	}
	coverage, queryIssues := ScanQueries(queryLogs)
	report.QueryCoverage = coverage
	report.Issues = append(report.Issues, queryIssues...)
	return report, nil
}
