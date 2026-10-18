package segment

// msbBitWriter is a most-significant-bit-first bit stream writer used by
// the gorilla encoder: values are emitted starting from their highest
// meaningful bit, and the final partial byte is zero-padded. Bits are
// emitted one at a time through the carry byte, which keeps the writer
// correct for any pending/width combination (an accumulator wider than 64
// bits is impossible to hold in a u64).
type msbBitWriter struct {
	buf []byte
	cur byte // partially filled byte, bits accumulate from the MSB side
	n   int  // bits currently filled in cur, 0..7
}

// writeBit appends a single bit (the low bit of v).
func (w *msbBitWriter) writeBit(v uint64) {
	w.cur = w.cur<<1 | byte(v&1)
	w.n++
	if w.n == 8 {
		w.buf = append(w.buf, w.cur)
		w.cur = 0
		w.n = 0
	}
}

// write appends the low nbits of v to the stream, MSB of the group first.
func (w *msbBitWriter) write(v uint64, nbits int) {
	for i := nbits - 1; i >= 0; i-- {
		w.writeBit(v >> uint(i))
	}
}

// bytes flushes the pending partial byte (zero-padded) and returns the
// stream.
func (w *msbBitWriter) bytes() []byte {
	if w.n > 0 {
		w.buf = append(w.buf, w.cur<<uint(8-w.n))
		w.cur = 0
		w.n = 0
	}
	return w.buf
}

// msbBitReader is the inverse of msbBitWriter. Reads past the end of the
// stream return zero bits (matching the writer's zero padding).
type msbBitReader struct {
	buf []byte
	pos int // byte position
	cur byte
	n   int // bits still unconsumed in cur, 0..8
}

// readBit consumes one bit.
func (r *msbBitReader) readBit() uint64 {
	if r.n == 0 {
		if r.pos < len(r.buf) {
			r.cur = r.buf[r.pos]
			r.pos++
		} else {
			r.cur = 0
		}
		r.n = 8
	}
	r.n--
	return uint64(r.cur >> uint(r.n) & 1)
}

// read consumes nbits and returns them as the low bits of the result.
func (r *msbBitReader) read(nbits int) uint64 {
	var v uint64
	for i := 0; i < nbits; i++ {
		v = v<<1 | r.readBit()
	}
	return v
}
