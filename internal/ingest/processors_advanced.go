package ingest

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.7.3

// GrokProcessor parses unstructured text using grok patterns.
type GrokProcessor struct {
	Field       string
	Patterns    []string
	TargetField string
}

func (p *GrokProcessor) Type() string { return "grok" }
func (p *GrokProcessor) Process(doc map[string]any) error {
	text, ok := doc[p.Field].(string)
	if !ok {
		return nil
	}
	// Build regex from grok pattern: %{NAME:capture} → (?P<capture>...)
	grokRe := regexp.MustCompile(`%\{(\w+):(\w+)\}`)
	for _, pattern := range p.Patterns {
		regexPattern := grokRe.ReplaceAllString(pattern, `(?P<$2>.+?)`)
		re, err := regexp.Compile(regexPattern)
		if err != nil {
			continue
		}
		matches := re.FindStringSubmatch(text)
		if matches == nil {
			continue
		}
		for i, name := range re.SubexpNames() {
			if i > 0 && name != "" && i < len(matches) {
				doc[name] = matches[i]
			}
		}
		return nil
	}
	return nil
}

// DissectProcessor parses structured text using fixed delimiter patterns.
type DissectProcessor struct {
	Field   string
	Pattern string
}

func (p *DissectProcessor) Type() string { return "dissect" }
func (p *DissectProcessor) Process(doc map[string]any) error {
	text, ok := doc[p.Field].(string)
	if !ok {
		return nil
	}
	// Dissect pattern: "part1 %{field1} part2 %{field2}"
	// Convert to regex: part1 (.+?) part2 (.+)
	parts := strings.Split(p.Pattern, "%{")
	if len(parts) < 2 {
		return nil
	}
	var regexParts []string
	regexParts = append(regexParts, regexp.QuoteMeta(parts[0]))
	var fieldNames []string
	for _, part := range parts[1:] {
		idx := strings.Index(part, "}")
		if idx < 0 {
			break
		}
		fieldName := part[:idx]
		rest := part[idx+1:]
		fieldNames = append(fieldNames, fieldName)
		if rest != "" {
			regexParts = append(regexParts, `(.+?)`, regexp.QuoteMeta(rest))
		} else {
			regexParts = append(regexParts, `(.+)`)
		}
	}
	re, err := regexp.Compile(strings.Join(regexParts, ""))
	if err != nil {
		return nil
	}
	matches := re.FindStringSubmatch(text)
	if matches == nil {
		return nil
	}
	for i, name := range fieldNames {
		if i+1 < len(matches) {
			doc[name] = strings.TrimSpace(matches[i+1])
		}
	}
	return nil
}

// DateProcessor parses date strings and normalizes to ISO8601.
type DateProcessor struct {
	Field       string
	Formats     []string
	TargetField string
	TimeZone    string
}

func (p *DateProcessor) Type() string { return "date" }
func (p *DateProcessor) Process(doc map[string]any) error {
	text, ok := doc[p.Field].(string)
	if !ok {
		return nil
	}
	target := p.TargetField
	if target == "" {
		target = "@timestamp"
	}
	// Try common date formats.
	formats := p.Formats
	if len(formats) == 0 {
		formats = []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"}
	}
	for _, layout := range formats {
		if t, err := time.Parse(layout, text); err == nil {
			doc[target] = t.UTC().Format(time.RFC3339)
			return nil
		}
	}
	// Try native Go parsing.
	if t, err := time.Parse(time.RFC3339, text); err == nil {
		doc[target] = t.UTC().Format(time.RFC3339)
	}
	return nil
}

// ScriptProcessor executes a Painless-like script (P2 simplified: only
// assignment expressions like "doc.field = value").
type ScriptProcessor struct {
	Source string
	Params map[string]any
}

func (p *ScriptProcessor) Type() string { return "script" }
func (p *ScriptProcessor) Process(doc map[string]any) error {
	// P2 simplified: parse simple assignments like "doc.field = expr"
	// where expr can be a literal or doc.other_field reference.
	lines := strings.Split(p.Source, ";")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		lhs := strings.TrimSpace(parts[0])
		rhs := strings.TrimSpace(parts[1])
		// Extract field name from "doc.field" pattern.
		if strings.HasPrefix(lhs, "doc.") {
			fieldName := strings.TrimPrefix(lhs, "doc.")
			fieldName = strings.Trim(fieldName, "[]'\"")
			// Evaluate RHS: literal or doc.other_field reference.
			if strings.HasPrefix(rhs, "doc.") {
				refField := strings.TrimPrefix(rhs, "doc.")
				refField = strings.Trim(refField, "[]'\"")
				if v, ok := doc[refField]; ok {
					doc[fieldName] = v
				}
			} else {
				// Try to parse as literal.
				doc[fieldName] = parseLiteral(rhs)
			}
		}
	}
	return nil
}

func parseLiteral(s string) any {
	s = strings.Trim(s, "\"'")
	if s == "true" {
		return true
	}
	if s == "false" {
		return false
	}
	if f, _ := fmt.Sscanf(s, "%f", new(float64)); f == 1 {
		var fval float64
		fmt.Sscanf(s, "%f", &fval)
		return fval
	}
	return s
}

// ForEachProcessor applies a sub-processor to each element of an array field.
type ForEachProcessor struct {
	Field        string
	SubProcessor Processor
}

func (p *ForEachProcessor) Type() string { return "foreach" }
func (p *ForEachProcessor) Process(doc map[string]any) error {
	arr, ok := doc[p.Field].([]any)
	if !ok {
		return nil
	}
	for i, elem := range arr {
		if elemMap, ok := elem.(map[string]any); ok {
			if p.SubProcessor != nil {
				_ = p.SubProcessor.Process(elemMap)
			}
			arr[i] = elemMap
		}
	}
	doc[p.Field] = arr
	return nil
}

// SortProcessor sorts an array field.
type SortProcessor struct {
	Field string
	Order string // "asc" | "desc"
}

func (p *SortProcessor) Type() string { return "sort" }
func (p *SortProcessor) Process(doc map[string]any) error {
	arr, ok := doc[p.Field].([]any)
	if !ok || len(arr) < 2 {
		return nil
	}
	sort.Slice(arr, func(i, j int) bool {
		a := fmt.Sprintf("%v", arr[i])
		b := fmt.Sprintf("%v", arr[j])
		if p.Order == "desc" {
			return a > b
		}
		return a < b
	})
	doc[p.Field] = arr
	return nil
}

// HtmlStripProcessor removes HTML tags from a string field.
type HtmlStripProcessor struct {
	Field string
}

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

func (p *HtmlStripProcessor) Type() string { return "html_strip" }
func (p *HtmlStripProcessor) Process(doc map[string]any) error {
	text, ok := doc[p.Field].(string)
	if !ok {
		return nil
	}
	doc[p.Field] = htmlTagRe.ReplaceAllString(text, "")
	return nil
}

// URLDecodeProcessor URL-decodes a string field.
type URLDecodeProcessor struct {
	Field string
}

func (p *URLDecodeProcessor) Type() string { return "urldecode" }
func (p *URLDecodeProcessor) Process(doc map[string]any) error {
	text, ok := doc[p.Field].(string)
	if !ok {
		return nil
	}
	if decoded, err := url.QueryUnescape(text); err == nil {
		doc[p.Field] = decoded
	}
	return nil
}
