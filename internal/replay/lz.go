// Package replay talks to the Madrid Monitor's select.jsp endpoint directly,
// over plain HTTP: the same POST the browser's own JavaScript issues, decoded
// and rebuilt here so a query costs one round trip instead of a browser
// navigation.
package replay

import (
	"errors"
	"strings"
	"unicode/utf16"
)

// keyStrBase64 is LZString's base64 alphabet as embedded in the site's
// branddb-required bundle, '=' included (index 64).
const keyStrBase64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/="

// ErrCorruptLZ is returned when a payload does not decompress: the site never
// sends one, so it means a capture was truncated or the port drifted.
var ErrCorruptLZ = errors.New("lzstring: corrupt payload")

// CompressToBase64 is LZString.compressToBase64 — the exact encoding the site
// wraps its query state in (_compressJSON in branddb-required).
//
// The algorithm works on UTF-16 code units, not bytes: JS strings are UTF-16
// and charCodeAt reads units, so a term outside the BMP must take the same
// path here or the server sees a different query than the one displayed.
func CompressToBase64(s string) string {
	comp := compressUnits(utf16.Encode([]rune(s)))

	var b strings.Builder
	i := 0 // bit cursor in half-bytes, exactly the JS loop's b
	for i < len(comp)*2 {
		var k, h, f int
		hNaN, fNaN := false, false
		if i%2 == 0 {
			k = int(comp[i/2]) >> 8
			h = int(comp[i/2]) & 255
			if i/2+1 < len(comp) {
				f = int(comp[i/2+1]) >> 8
			} else {
				fNaN = true
			}
		} else {
			k = int(comp[(i-1)/2]) & 255
			if (i+1)/2 < len(comp) {
				u := comp[(i+1)/2]
				h = int(u) >> 8
				f = int(u) & 255
			} else {
				hNaN, fNaN = true, true
			}
		}
		i += 3
		j := k >> 2
		g := ((k & 3) << 4) | (h >> 4)
		e := ((h & 15) << 2) | (f >> 6)
		d := f & 63
		switch {
		case hNaN:
			e, d = 64, 64
		case fNaN:
			d = 64
		}
		b.WriteByte(keyStrBase64[j])
		b.WriteByte(keyStrBase64[g])
		b.WriteByte(keyStrBase64[e])
		b.WriteByte(keyStrBase64[d])
	}
	return b.String()
}

// DecompressFromBase64 is LZString.decompressFromBase64. Production only ever
// compresses; this exists so tests can prove the port against payloads
// captured from the live site.
func DecompressFromBase64(g string) (string, error) {
	var sym []int
	for _, r := range g {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
			r == '+' || r == '/' || r == '=' {
			idx := strings.IndexRune(keyStrBase64, r)
			if idx < 0 {
				return "", ErrCorruptLZ
			}
			sym = append(sym, idx)
		}
	}
	if len(sym)%4 != 0 {
		return "", ErrCorruptLZ
	}
	var a []uint16
	d := 0
	var e int // carries across groups: an odd group ORs into the previous even group's stash
	for i := 0; i+3 < len(sym); i += 4 {
		n, l, j, h := sym[i], sym[i+1], sym[i+2], sym[i+3]
		o := (n << 2) | (l >> 4)
		m := ((l & 15) << 4) | (j >> 2)
		k := ((j & 3) << 6) | h
		if d%2 == 0 {
			e = o << 8
			if j != 64 {
				a = append(a, uint16(e|m))
			}
			if h != 64 {
				e = k << 8
			}
		} else {
			a = append(a, uint16(e|o))
			if j != 64 {
				e = m << 8
			}
			if h != 64 {
				a = append(a, uint16(e|k))
			}
		}
		d += 3
	}
	return decompressUnits(a)
}

// packKey renders a unit sequence as a map key: two bytes per unit, so two
// different sequences can never collide the way a lossy string conversion
// could.
func packKey(u []uint16) string {
	b := make([]byte, len(u)*2)
	for i, v := range u {
		b[i*2] = byte(v >> 8)
		b[i*2+1] = byte(v)
	}
	return string(b)
}

// bitWriter mirrors the JS accumulator: 16 bits per emitted char, flush when
// the pre-shift counter reads 15.
type bitWriter struct {
	out  []uint16
	acc  int
	bits int
}

func (w *bitWriter) writeBit(bit int) {
	w.acc = (w.acc << 1) | bit
	if w.bits == 15 {
		w.out = append(w.out, uint16(w.acc))
		w.acc, w.bits = 0, 0
		return
	}
	w.bits++
}

// pad flushes the accumulator with zero bits up to a full char, the way the
// JS `while(true){a=(a<<1);if(j==15){q+=k(a);break}else{j++}}` does.
func (w *bitWriter) pad() {
	for {
		w.acc <<= 1
		if w.bits == 15 {
			w.out = append(w.out, uint16(w.acc))
			w.acc, w.bits = 0, 0
			return
		}
		w.bits++
	}
}

// compressUnits is LZString.compress over UTF-16 units, ported statement for
// statement from the minified bundle — including its two dictionary-size
// decrements on the raw-emission path and one on the code path.
func compressUnits(e []uint16) []uint16 {
	dict := map[string]int{}   // n
	inUse := map[string]bool{} // m
	dictSize := 3              // g — JS starts at 3: 0,1,2 are reserved by the decoder
	enlargeIn := 2             // d
	numBits := 2               // b
	w := &bitWriter{}

	grow := func() {
		enlargeIn--
		if enlargeIn == 0 {
			enlargeIn = 1 << numBits
			numBits++
		}
	}
	// emit writes w (already non-empty) as the raw first char when its
	// membership bit is still set, otherwise as its dictionary code.
	emit := func(seq []uint16) {
		key := packKey(seq)
		if inUse[key] {
			v := int(seq[0])
			if v < 256 {
				for i := 0; i < numBits; i++ {
					w.writeBit(0)
				}
				for i := 0; i < 8; i++ {
					w.writeBit(v & 1)
					v >>= 1
				}
			} else {
				w.writeBit(1)
				for i := 0; i < numBits-1; i++ {
					w.writeBit(0)
				}
				for i := 0; i < 16; i++ {
					w.writeBit(v & 1)
					v >>= 1
				}
			}
			grow()
			delete(inUse, key)
		} else {
			v := dict[key]
			for i := 0; i < numBits; i++ {
				w.writeBit(v & 1)
				v >>= 1
			}
		}
		grow()
	}

	var cur []uint16
	for _, c := range e {
		single := []uint16{c}
		keyO := packKey(single)
		if _, ok := dict[keyO]; !ok {
			dict[keyO] = dictSize
			inUse[keyO] = true
			dictSize++
		}
		next := append(append([]uint16{}, cur...), c)
		keyNext := packKey(next)
		if _, ok := dict[keyNext]; ok {
			cur = next
			continue
		}
		emit(cur)
		dict[keyNext] = dictSize
		dictSize++
		cur = single
	}
	if len(cur) > 0 {
		emit(cur)
	}
	// End-of-stream marker: the literal value 2 over numBits bits, then the
	// zero-pad flush.
	v := 2
	for i := 0; i < numBits; i++ {
		w.writeBit(v & 1)
		v >>= 1
	}
	w.pad()
	return w.out
}

// bitReader mirrors the JS decompress reader: LSB-first, 32768-byte refill
// window, exhausted input reads as zeros (JS's NaN & position is falsy).
type bitReader struct {
	k        []uint16
	val      int
	position int
	index    int
}

func newBitReader(k []uint16) *bitReader {
	return &bitReader{k: k, val: int(k[0]), position: 32768, index: 1}
}

func (r *bitReader) read(n int) int {
	res, multiplier := 0, 1
	for i := 0; i < n; i++ {
		bit := r.val & r.position
		r.position >>= 1
		if r.position == 0 {
			r.position = 32768
			if r.index < len(r.k) {
				r.val = int(r.k[r.index])
			} else {
				r.val = 0
			}
			r.index++
		}
		if bit > 0 {
			res += multiplier
		}
		multiplier <<= 1
	}
	return res
}

// decompressUnits is LZString.decompress.
func decompressUnits(k []uint16) (string, error) {
	if len(k) == 0 {
		return "", nil
	}
	dict := [][]uint16{{0}, {1}, {2}}
	enlargeIn := 4 // d
	numBits := 3   // h
	r := newBitReader(k)

	code := r.read(2) // first read is hard-wired to 2 bits
	var first []uint16
	switch code {
	case 0:
		first = []uint16{uint16(r.read(8))}
	case 1:
		first = []uint16{uint16(r.read(16))}
	case 2:
		return "", nil
	default:
		return "", ErrCorruptLZ
	}
	dict = append(dict, first) // o[3] = n
	p := append([]uint16{}, first...)
	total := append([]uint16{}, first...)

	for {
		if r.index > len(r.k) {
			return "", nil
		}
		code = r.read(numBits)
		switch code {
		case 0:
			dict = append(dict, []uint16{uint16(r.read(8))})
			code = len(dict) - 1
			enlargeIn--
		case 1:
			dict = append(dict, []uint16{uint16(r.read(16))})
			code = len(dict) - 1
			enlargeIn--
		case 2:
			return string(utf16.Decode(total)), nil
		}
		// Any other code is a dictionary reference and flows through unchanged,
		// exactly like the JS switch falling out with n untouched.
		if enlargeIn == 0 {
			enlargeIn = 1 << numBits
			numBits++
		}
		var q []uint16
		switch {
		case code < len(dict):
			q = dict[code]
		case code == len(dict):
			if len(p) == 0 {
				return "", ErrCorruptLZ
			}
			q = append(append([]uint16{}, p...), p[0])
		default:
			return "", ErrCorruptLZ
		}
		total = append(total, q...)
		if len(p) == 0 || len(q) == 0 {
			return "", ErrCorruptLZ
		}
		dict = append(dict, append(append([]uint16{}, p...), q[0]))
		enlargeIn--
		if enlargeIn == 0 {
			enlargeIn = 1 << numBits
			numBits++
		}
		p = q
	}
}
