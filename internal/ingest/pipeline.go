// Package ingest provides pipeline-based document preprocessing.
// @spec: ./archive/2026-10-22-es-feature-gap-fill/02_spec.md#2.7
package ingest

import "fmt"

// Processor is the interface for pipeline processing steps.
type Processor interface {
	Process(doc map[string]any) error
	Type() string
}

// Pipeline is a named sequence of processors.
type Pipeline struct {
	ID          string
	Description string
	Processors  []Processor
}

// Execute runs all processors on the document in order.
func (p *Pipeline) Execute(doc map[string]any) error {
	for i, proc := range p.Processors {
		if err := proc.Process(doc); err != nil {
			return fmt.Errorf("processor[%d] %s: %w", i, proc.Type(), err)
		}
	}
	return nil
}

// SetProcessor sets a field to a constant or template value.
type SetProcessor struct {
	Field string
	Value any
}

func (p *SetProcessor) Type() string { return "set" }
func (p *SetProcessor) Process(doc map[string]any) error {
	doc[p.Field] = p.Value
	return nil
}

// RemoveProcessor removes a field from the document.
type RemoveProcessor struct {
	Field string
}

func (p *RemoveProcessor) Type() string { return "remove" }
func (p *RemoveProcessor) Process(doc map[string]any) error {
	delete(doc, p.Field)
	return nil
}

// RenameProcessor renames a field.
type RenameProcessor struct {
	From string
	To   string
}

func (p *RenameProcessor) Type() string { return "rename" }
func (p *RenameProcessor) Process(doc map[string]any) error {
	if v, ok := doc[p.From]; ok {
		doc[p.To] = v
		delete(doc, p.From)
	}
	return nil
}

// ConvertProcessor converts a field value to a different type.
type ConvertProcessor struct {
	Field      string
	TargetType string // "integer", "float", "string", "boolean"
}

func (p *ConvertProcessor) Type() string { return "convert" }
func (p *ConvertProcessor) Process(doc map[string]any) error {
	v, ok := doc[p.Field]
	if !ok {
		return nil
	}
	switch p.TargetType {
	case "string":
		doc[p.Field] = fmt.Sprintf("%v", v)
	case "integer":
		switch n := v.(type) {
		case float64:
			doc[p.Field] = int64(n)
		case int64:
			doc[p.Field] = n
		case int:
			doc[p.Field] = int64(n)
		default:
			return fmt.Errorf("cannot convert %T to integer", v)
		}
	case "float":
		switch n := v.(type) {
		case float64:
			doc[p.Field] = n
		case int64:
			doc[p.Field] = float64(n)
		case int:
			doc[p.Field] = float64(n)
		default:
			return fmt.Errorf("cannot convert %T to float", v)
		}
	case "boolean":
		switch b := v.(type) {
		case bool:
			doc[p.Field] = b
		case string:
			doc[p.Field] = b == "true"
		default:
			return fmt.Errorf("cannot convert %T to boolean", v)
		}
	}
	return nil
}

// NewProcessor creates a processor from a type name and config.
func NewProcessor(typeName string, config map[string]any) (Processor, error) {
	switch typeName {
	case "set":
		field, _ := config["field"].(string)
		return &SetProcessor{Field: field, Value: config["value"]}, nil
	case "remove":
		field, _ := config["field"].(string)
		return &RemoveProcessor{Field: field}, nil
	case "rename":
		from, _ := config["field"].(string)
		to, _ := config["target_field"].(string)
		return &RenameProcessor{From: from, To: to}, nil
	case "convert":
		field, _ := config["field"].(string)
		typ, _ := config["type"].(string)
		return &ConvertProcessor{Field: field, TargetType: typ}, nil
	case "grok":
		field, _ := config["field"].(string)
		var patterns []string
		if raw, ok := config["patterns"].([]any); ok {
			for _, p := range raw {
				if s, ok := p.(string); ok {
					patterns = append(patterns, s)
				}
			}
		}
		target, _ := config["target_field"].(string)
		return &GrokProcessor{Field: field, Patterns: patterns, TargetField: target}, nil
	case "dissect":
		field, _ := config["field"].(string)
		pattern, _ := config["pattern"].(string)
		return &DissectProcessor{Field: field, Pattern: pattern}, nil
	case "date":
		field, _ := config["field"].(string)
		target, _ := config["target_field"].(string)
		tz, _ := config["timezone"].(string)
		var formats []string
		if raw, ok := config["formats"].([]any); ok {
			for _, f := range raw {
				if s, ok := f.(string); ok {
					formats = append(formats, s)
				}
			}
		}
		return &DateProcessor{Field: field, Formats: formats, TargetField: target, TimeZone: tz}, nil
	case "script":
		source, _ := config["source"].(string)
		params, _ := config["params"].(map[string]any)
		return &ScriptProcessor{Source: source, Params: params}, nil
	case "sort":
		field, _ := config["field"].(string)
		order, _ := config["order"].(string)
		return &SortProcessor{Field: field, Order: order}, nil
	case "html_strip":
		field, _ := config["field"].(string)
		return &HtmlStripProcessor{Field: field}, nil
	case "urldecode":
		field, _ := config["field"].(string)
		return &URLDecodeProcessor{Field: field}, nil
	case "foreach":
		field, _ := config["field"].(string)
		// P3: ForEach with a single sub-processor from the "processor" config.
		var subProc Processor
		if procConf, ok := config["processor"].(map[string]any); ok {
			subType, _ := procConf["type"].(string)
			if subType != "" {
				subProc, _ = NewProcessor(subType, procConf)
			}
		}
		return &ForEachProcessor{Field: field, SubProcessor: subProc}, nil
	default:
		return nil, fmt.Errorf("unknown processor type: %s", typeName)
	}
}
