// Package jsonx provides a unified JSON interface that uses goccy/go-json
// for high-performance serialization on hot paths (design 00 §2.1).
// To fall back to the standard library, replace the import in this file
// from "github.com/goccy/go-json" to "encoding/json".
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.9
package jsonx

import (
	"io"

	"github.com/goccy/go-json"
)

// Marshal is a drop-in replacement for encoding/json.Marshal.
func Marshal(v any) ([]byte, error) { return json.Marshal(v) }

// Unmarshal is a drop-in replacement for encoding/json.Unmarshal.
func Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// NewEncoder mirrors encoding/json.NewEncoder.
func NewEncoder(w io.Writer) *json.Encoder { return json.NewEncoder(w) }

// NewDecoder mirrors encoding/json.NewDecoder.
func NewDecoder(r io.Reader) *json.Decoder { return json.NewDecoder(r) }

// RawMessage is a type alias to json.RawMessage for convenience.
type RawMessage = json.RawMessage

// Number is a type alias to json.Number for convenience.
type Number = json.Number

// Encoder is a type alias to json.Encoder.
type Encoder = json.Encoder

// Decoder is a type alias to json.Decoder.
type Decoder = json.Decoder
