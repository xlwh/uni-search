// CollationFilter converts tokens to locale-aware collation sort keys.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.4
package analyzer

import (
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

// CollationFilter implements TokenFilter. Replaces each token's Term
// with the hex-encoded collation sort key for the configured locale,
// so that byte-order comparison of tokens equals locale-aware sorting.
type CollationFilter struct {
	collator *collate.Collator
	buf      collate.Buffer
}

// NewCollationFilter creates a collation filter for the given locale
// and comparison strength.
func NewCollationFilter(locale string, strength string) (*CollationFilter, error) {
	tag, err := language.Parse(locale)
	if err != nil {
		// Fall back to und (undefined) rather than failing.
		tag = language.Und
	}
	c := collate.New(tag)

	// Map strength string to collate options.
	switch strings.ToLower(strength) {
	case "primary":
		c = collate.New(tag, collate.IgnoreDiacritics, collate.IgnoreCase, collate.IgnoreWidth)
	case "secondary":
		c = collate.New(tag, collate.IgnoreCase, collate.IgnoreWidth)
	case "tertiary":
		c = collate.New(tag, collate.IgnoreWidth)
	case "quaternary", "identical":
		c = collate.New(tag)
	default:
		// No strength option — use default.
	}

	return &CollationFilter{collator: c}, nil
}

// Filter implements TokenFilter. Replaces each token's Term with the
// hex-encoded collation sort key. Start/End/Pos are preserved.
func (f *CollationFilter) Filter(tokens []Token) []Token {
	out := make([]Token, len(tokens))
	for i, tok := range tokens {
		key := f.collator.KeyFromString(&f.buf, tok.Term)
		out[i] = Token{
			Term:  hex.EncodeToString(key),
			Start: tok.Start,
			End:   tok.End,
			Pos:   tok.Pos,
			Type:  tok.Type,
		}
	}
	return out
}

// KeyString returns the collation sort key for a string (exposed
// for use by the sort path in the translator).
func (f *CollationFilter) KeyString(s string) string {
	key := f.collator.KeyFromString(&f.buf, s)
	return hex.EncodeToString(key)
}

// SupportedLocales returns a list of commonly supported locale tags.
func SupportedLocales() []string {
	return []string{"en", "zh-Hans", "zh-Hant", "ja", "ko", "de", "fr", "es", "it", "ru", "ar"}
}

// validateStrength checks that the strength string is one of the
// supported values. Returns an error for unknown values (used by
// the factory).
func validateStrength(strength string) error {
	switch strings.ToLower(strength) {
	case "primary", "secondary", "tertiary", "quaternary", "identical", "":
		return nil
	default:
		return fmt.Errorf("collation: unknown strength %q", strength)
	}
}
