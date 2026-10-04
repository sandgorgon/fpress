package entropy

import "errors"

// A carry-less binary arithmetic coder over a 32-bit interval. p is the
// probability that the bit is 1, scaled to 16 bits and kept in [1, 65535].

type encoder struct {
	x1, x2 uint32
	out    []byte
}

func newEncoder() *encoder { return &encoder{x2: 0xffffffff} }

func (e *encoder) encode(bit int, p int) {
	mid := e.x1 + uint32((uint64(e.x2-e.x1)*uint64(p))>>16)
	if bit != 0 {
		e.x2 = mid
	} else {
		e.x1 = mid + 1
	}
	for (e.x1^e.x2)&0xff000000 == 0 {
		e.out = append(e.out, byte(e.x2>>24))
		e.x1 <<= 8
		e.x2 = e.x2<<8 | 255
	}
}

// finish writes the four bytes that pin down the final interval.
func (e *encoder) finish() []byte {
	return append(e.out, byte(e.x1>>24), byte(e.x1>>16), byte(e.x1>>8), byte(e.x1))
}

type decoder struct {
	x1, x2, x uint32
	in        []byte
	pos       int
	overrun   bool
}

func newDecoder(in []byte) *decoder {
	d := &decoder{x2: 0xffffffff, in: in}
	for i := 0; i < 4; i++ {
		d.x = d.x<<8 | uint32(d.next())
	}
	return d
}

func (d *decoder) next() byte {
	if d.pos >= len(d.in) {
		d.overrun = true
		return 0
	}
	b := d.in[d.pos]
	d.pos++
	return b
}

func (d *decoder) decode(p int) int {
	mid := d.x1 + uint32((uint64(d.x2-d.x1)*uint64(p))>>16)
	var bit int
	if d.x <= mid {
		bit = 1
		d.x2 = mid
	} else {
		d.x1 = mid + 1
	}
	for (d.x1^d.x2)&0xff000000 == 0 {
		d.x1 <<= 8
		d.x2 = d.x2<<8 | 255
		d.x = d.x<<8 | uint32(d.next())
	}
	return bit
}

var errStream = errors.New("entropy: malformed stream")
