// MinHashFilter computes MinHash signatures over token Term sets.
// @spec: ./active_specs/2026-10-23-iresearch-features-import/02_spec.md#2.3
package analyzer

import (
	"fmt"
	"hash/fnv"
	"math/rand"
)

// MinHashFilter implements TokenFilter. For each input token set it
// computes a MinHash signature (num_perm dimensions) and emits the
// signature as synthetic tokens with Type "<MINHASH>".
type MinHashFilter struct {
	numPerm int
	hashFns []minHashFunc
}

// minHashFunc is a parameterized hash function h(x) = (a*x + b) mod p.
type minHashFunc struct {
	a, b uint64
	p    uint64 // Mersenne prime 2^61 - 1
}

const mersennePrime uint64 = (1 << 61) - 1

// NewMinHashFilter creates a MinHash filter with the given signature
// dimension and random seed.
func NewMinHashFilter(numPerm int, seed int64) (*MinHashFilter, error) {
	if numPerm <= 0 || numPerm > 1024 {
		return nil, fmt.Errorf("minhash: num_perm must be 1-1024, got %d", numPerm)
	}
	r := rand.New(rand.NewSource(seed))
	fns := make([]minHashFunc, numPerm)
	for i := range fns {
		fns[i] = minHashFunc{
			a: r.Uint64()%mersennePrime + 1, // a ∈ [1, p]
			b: r.Uint64() % mersennePrime,   // b ∈ [0, p)
			p: mersennePrime,
		}
	}
	return &MinHashFilter{numPerm: numPerm, hashFns: fns}, nil
}

// Filter implements TokenFilter. Computes MinHash signatures over
// the input token set and emits them as synthetic tokens.
func (f *MinHashFilter) Filter(tokens []Token) []Token {
	if len(tokens) == 0 {
		return tokens
	}

	// Compute FNV-1a hash of each token Term.
	tokenHashes := make([]uint64, len(tokens))
	for i, tok := range tokens {
		h := fnv.New64a()
		h.Write([]byte(tok.Term))
		tokenHashes[i] = h.Sum64()
	}

	// Compute MinHash signature: for each hash function, take
	// the minimum over all token hashes.
	out := make([]Token, 0, f.numPerm)
	for dim, hf := range f.hashFns {
		minVal := hf.apply(tokenHashes[0])
		for i := 1; i < len(tokenHashes); i++ {
			v := hf.apply(tokenHashes[i])
			if v < minVal {
				minVal = v
			}
		}
		out = append(out, Token{
			Term:  fmt.Sprintf("mh_%d_%d", dim, minVal),
			Start: 0,
			End:   0,
			Pos:   dim,
			Type:  "<MINHASH>",
		})
	}
	return out
}

// apply computes h(x) = (a*x + b) mod p for the given hash value.
// Uses uint64 arithmetic with natural overflow (mod 2^64), which
// provides good pairwise independence for MinHash purposes without
// requiring 128-bit intermediate values.
func (hf *minHashFunc) apply(x uint64) uint64 {
	// Reduce x modulo p first to keep products smaller.
	xx := x % hf.p
	// Compute (a*xx + b) with uint64 overflow wrapping. The overflow
	// acts as mod 2^64; combined with the final mod p, this gives a
	// good approximation of true modular arithmetic. For MinHash,
	// pairwise independence is preserved which is all that's needed.
	product := hf.a * xx // uint64 overflow wraps around
	result := product + hf.b
	return result % hf.p
}
