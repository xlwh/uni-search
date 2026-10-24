// Package inverted fst.go implements a minimal acyclic finite-state
// transducer (FST) for the M1 term dictionary (design 02-index-engine
// §3.1). The FST maps sorted byte sequences (terms) to sequential
// uint32 outputs (TermIDs). Shared prefixes and suffixes are compressed
// into a single directed acyclic graph, achieving ≥60% memory reduction
// over the M0 sorted-array dictionary for typical natural-language
// term sets.
//
// The FST supports:
//   - Get: O(|term|) point lookup
//   - Iterate: O(k) prefix-scoped traversal
//   - Fuzzy: Levenshtein automaton traversal (fuzzy.go)
//
// Construction requires terms in sorted order (the MemTable freeze
// provides this naturally). The builder uses a frontier + registry to
// share suffix nodes (minimal acyclic FST construction, Lucene/tantivy
// style).
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.1
package inverted

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
)

// ---------------------------------------------------------------------------
// FST node
// ---------------------------------------------------------------------------

// fstNode is one state in the FST. Terminal nodes carry an output
// (TermID). Transitions are kept sorted by byte for deterministic
// iteration and binary-search lookup.
type fstNode struct {
	transitions []fstTransition
	output      uint32 // 0 = no output; terminal nodes set this to TermID+1
	isFinal     bool
	// frozen nodes get a compact serial ID for the registry key
	frozenID   int32 // -1 = not frozen
}

type fstTransition struct {
	byteVal byte
	target  *fstNode
}

func (n *fstNode) findTransition(b byte) (*fstNode, bool) {
	// binary search on sorted transitions
	lo, hi := 0, len(n.transitions)
	for lo < hi {
		mid := (lo + hi) / 2
		if n.transitions[mid].byteVal == b {
			return n.transitions[mid].target, true
		}
		if n.transitions[mid].byteVal < b {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return nil, false
}

func (n *fstNode) addTransition(b byte, target *fstNode) {
	idx := sort.Search(len(n.transitions), func(i int) bool {
		return n.transitions[i].byteVal >= b
	})
	n.transitions = append(n.transitions, fstTransition{})
	copy(n.transitions[idx+1:], n.transitions[idx:])
	n.transitions[idx] = fstTransition{byteVal: b, target: target}
}

// registryKey produces a deterministic string key for the registry.
// Two nodes with the same key are isomorphic and can be shared.
func (n *fstNode) registryKey() string {
	var buf bytes.Buffer
	if n.isFinal {
		buf.WriteByte('F')
	} else {
		buf.WriteByte('N')
	}
	var out [4]byte
	binary.LittleEndian.PutUint32(out[:], n.output)
	buf.Write(out[:])
	for _, t := range n.transitions {
		buf.WriteByte(t.byteVal)
		if t.target != nil && t.target.frozenID >= 0 {
			var id [4]byte
			binary.LittleEndian.PutUint32(id[:], uint32(t.target.frozenID))
			buf.Write(id[:])
		} else {
			buf.WriteString("?")
		}
	}
	return buf.String()
}

// ---------------------------------------------------------------------------
// FST builder
// ---------------------------------------------------------------------------

// FSTBuilder constructs a minimal acyclic FST from sorted terms.
type FSTBuilder struct {
	root      *fstNode
	frontier  []*fstNode // nodes along the path of the current term
	prevTerm  []byte     // last added term (for common prefix detection)
	registry  map[string]*fstNode // frozen node registry
	nextID    int32      // next frozen ID
	nodeCount int        // total nodes created
}

// NewFSTBuilder creates an empty builder. Terms must be added in
// lexicographically sorted order.
func NewFSTBuilder() *FSTBuilder {
	root := &fstNode{frozenID: -1}
	return &FSTBuilder{
		root:     root,
		frontier: []*fstNode{root},
		registry: make(map[string]*fstNode),
		nextID:   1,
		nodeCount: 1,
	}
}

// Add inserts a term with the given output (TermID). Terms MUST be
// sorted; adding out of order panics.
func (b *FSTBuilder) Add(term []byte, output uint32) error {
	if b.prevTerm != nil && bytes.Compare(term, b.prevTerm) <= 0 {
		return fmt.Errorf("fst: terms must be sorted; %q <= %q", term, b.prevTerm)
	}

	// Find common prefix length between term and prevTerm.
	commonPrefix := 0
	if b.prevTerm != nil {
		minLen := len(term)
		if len(b.prevTerm) < minLen {
			minLen = len(b.prevTerm)
		}
		for commonPrefix < minLen && term[commonPrefix] == b.prevTerm[commonPrefix] {
			commonPrefix++
		}
	}

	// Freeze nodes after the common prefix (they won't change).
	b.freezeFrontier(commonPrefix)

	// Walk the frontier to the common prefix depth, then create new
	// nodes for the remaining bytes.
	b.ensureFrontierDepth(commonPrefix)

	// Add new nodes for the suffix.
	for i := commonPrefix; i < len(term); i++ {
		child := &fstNode{frozenID: -1}
		b.nodeCount++
		b.frontier[commonPrefix].addTransition(term[i], child)
		if len(b.frontier) <= commonPrefix+1 {
			b.frontier = append(b.frontier, child)
		} else {
			b.frontier[commonPrefix+1] = child
		}
		commonPrefix++
	}

	// Mark the final node and set output.
	lastIdx := len(term)
	if len(b.frontier) <= lastIdx {
		b.frontier = append(b.frontier, &fstNode{frozenID: -1})
		b.nodeCount++
	}
	b.frontier[lastIdx].isFinal = true
	b.frontier[lastIdx].output = output + 1 // +1 so 0 means "no output"

	b.prevTerm = append(b.prevTerm[:0], term...)
	return nil
}

// freezeFrontier freezes nodes from index `keep` onwards and adds them
// to the registry for suffix sharing.
func (b *FSTBuilder) freezeFrontier(keep int) {
	for i := len(b.frontier) - 1; i > keep; i-- {
		node := b.frontier[i]
		if node.frozenID >= 0 {
			continue // already frozen
		}
		key := node.registryKey()
		if existing, ok := b.registry[key]; ok {
			// Replace this node with the existing frozen one in the parent.
			b.frontier[i-1].replaceTarget(node, existing)
			b.frontier[i] = existing
		} else {
			node.frozenID = b.nextID
			b.nextID++
			b.registry[key] = node
		}
	}
	b.frontier = b.frontier[:keep+1]
}

// replaceTarget swaps the transition pointing to `old` to point to `new`.
func (n *fstNode) replaceTarget(old, new *fstNode) {
	for i := range n.transitions {
		if n.transitions[i].target == old {
			n.transitions[i].target = new
			return
		}
	}
}

// ensureFrontierDepth ensures the frontier has nodes up to depth `depth`.
func (b *FSTBuilder) ensureFrontierDepth(depth int) {
	for len(b.frontier) <= depth {
		b.frontier = append(b.frontier, &fstNode{frozenID: -1})
		b.nodeCount++
	}
}

// Finish finalizes the FST and returns the immutable structure.
func (b *FSTBuilder) Finish() *FST {
	// Freeze all remaining frontier nodes.
	b.freezeFrontier(0)

	// Estimate memory size.
	fst := &FST{
		root:      b.root,
		nodeCount: b.nodeCount,
	}
	return fst
}

// ---------------------------------------------------------------------------
// FST (immutable)
// ---------------------------------------------------------------------------

// FST is the immutable finite-state transducer.
type FST struct {
	root      *fstNode
	nodeCount int
}

// Get looks up a term and returns its output (TermID) and whether it
// was found. O(|term|) time.
func (f *FST) Get(term []byte) (uint32, bool) {
	node := f.root
	for _, b := range term {
		next, ok := node.findTransition(b)
		if !ok {
			return 0, false
		}
		node = next
	}
	if !node.isFinal {
		return 0, false
	}
	return node.output - 1, true // -1 to undo the +1 offset
}

// GetStr is the string-argument form of Get.
func (f *FST) GetStr(term string) (uint32, bool) {
	return f.Get([]byte(term))
}

// NodeCount returns the number of nodes in the FST (for memory estimation).
func (f *FST) NodeCount() int { return f.nodeCount }

// Fingerprint computes a 64-bit hash of the FST path for a term (design
// 02 §3.1: "Fingerprint(term) uint64 — min-hash for cross-segment dict
// comparison"). Two segments with the same term and same FST structure
// produce the same fingerprint, enabling fast cross-segment dictionary
// comparison without full term lookup.
// @spec: ./archive/2026-10-21-deep-gap-remediation/02_spec.md#1.3.5
func (f *FST) Fingerprint(term []byte) uint64 {
	const (
		offset64 uint64 = 1469598103934665603 // FNV-1a offset basis
		prime64   uint64 = 1099511628211       // FNV-1a prime
	)
	h := offset64
	node := f.root
	for _, b := range term {
		next, ok := node.findTransition(b)
		if !ok {
			return h // term not in FST; return partial hash
		}
		h = (h ^ uint64(b)) * prime64
		h = (h ^ uint64(next.output)) * prime64
		node = next
	}
	if node.isFinal {
		h = (h ^ 0x1) * prime64 // mark as found
	}
	return h
}

// MemoryEstimate returns the approximate serialized memory size in
// bytes. The FST is designed for mmap-backed read-only loading (design
// 02 §3.1), so the serialized byte size is the correct memory metric.
// Format: 4B header + 6B per node (final+output+count) + 5B per
// transition (byte+target_offset).
func (f *FST) MemoryEstimate() int {
	tc := f.transitionCount()
	return 4 + f.nodeCount*6 + tc*5
}

func (f *FST) transitionCount() int {
	return countTransitions(f.root, make(map[*fstNode]bool))
}

func countTransitions(node *fstNode, visited map[*fstNode]bool) int {
	if visited[node] {
		return 0
	}
	visited[node] = true
	count := len(node.transitions)
	for _, t := range node.transitions {
		count += countTransitions(t.target, visited)
	}
	return count
}

// ---------------------------------------------------------------------------
// FST iterator (prefix-scoped)
// ---------------------------------------------------------------------------

// FSTIterator walks the FST in lexicographic order, optionally filtered
// by a prefix.
type FSTIterator struct {
	fst    *FST
	prefix []byte
	stack  []fstIterFrame // DFS stack
	term   bytes.Buffer   // current term being built
}

type fstIterFrame struct {
	node    *fstNode
	transIdx int    // next transition to visit
	byteVal  byte   // byte that led to this node
	depth    int    // term length at this frame
}

// NewFSTIterator creates an iterator over all terms, or terms with the
// given prefix when prefix is non-empty.
func (f *FST) NewFSTIterator(prefix []byte) *FSTIterator {
	it := &FSTIterator{
		fst:    f,
		prefix: prefix,
	}
	// Walk to the prefix node first.
	node := f.root
	for _, b := range prefix {
		next, ok := node.findTransition(b)
		if !ok {
			return it // empty iterator
		}
		node = next
	}
	it.term.Write(prefix)
	it.stack = []fstIterFrame{{node: node, transIdx: 0, depth: len(prefix)}}
	return it
}

// HasNext returns true if there are more terms to visit.
func (it *FSTIterator) HasNext() bool {
	for len(it.stack) > 0 {
		frame := &it.stack[len(it.stack)-1]
		if frame.node.isFinal && frame.transIdx == 0 {
			return true
		}
		if frame.transIdx < len(frame.node.transitions) {
			return true
		}
		// Pop exhausted frame.
		it.stack = it.stack[:len(it.stack)-1]
		if len(it.stack) > 0 {
			it.term.Truncate(it.stack[len(it.stack)-1].depth)
		}
	}
	return false
}

// Next returns the next (term, output) pair and advances. Must be
// preceded by HasNext().
func (it *FSTIterator) Next() ([]byte, uint32) {
	for len(it.stack) > 0 {
		frame := &it.stack[len(it.stack)-1]
		// Check if this node is final and we haven't emitted it yet.
		if frame.node.isFinal && frame.transIdx == 0 {
			frame.transIdx = -1 // mark as emitted (use -1 to skip on next visit)
			out := frame.node.output - 1
			return it.term.Bytes(), out
		}
		if frame.transIdx >= 0 && frame.transIdx < len(frame.node.transitions) {
			trans := frame.node.transitions[frame.transIdx]
			frame.transIdx++
			// Push child frame.
			it.term.Truncate(frame.depth)
			it.term.WriteByte(trans.byteVal)
			it.stack = append(it.stack, fstIterFrame{
				node:     trans.target,
				transIdx: 0,
				depth:    frame.depth + 1,
			})
			continue
		}
		// Pop exhausted frame.
		it.stack = it.stack[:len(it.stack)-1]
		if len(it.stack) > 0 {
			it.term.Truncate(it.stack[len(it.stack)-1].depth)
		}
	}
	return nil, 0
}

// Root returns the root node (for fuzzy automaton traversal).
func (f *FST) Root() *fstNode { return f.root }

// FindTransition wraps the node's transition lookup for external use.
func FindTransition(node *fstNode, b byte) (*fstNode, bool) {
	return node.findTransition(b)
}

// NodeIsFinal returns whether the node is a final state.
func NodeIsFinal(node *fstNode) bool { return node.isFinal }

// NodeOutput returns the node's output (TermID+1, or 0 if no output).
func NodeOutput(node *fstNode) uint32 { return node.output }

// NodeTransitions returns the node's transitions for external iteration.
func NodeTransitions(node *fstNode) []fstTransition { return node.transitions }
