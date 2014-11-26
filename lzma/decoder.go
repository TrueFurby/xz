package lzma

import (
	"bufio"
	"errors"
	"fmt"
	"io"

	"github.com/uli-go/xz/xlog"
)

// states defines the overall state count
const states = 12

// bufferLen is the value used for the bufferLen used by the decoder.
var bufferLen = 64 * (1 << 10)

// noUnpackLen requires an explicit end of stream marker
const noUnpackLen uint64 = 1<<64 - 1

// Decoder is able to read a LZMA byte stream and to read the plain text.
//
// Note that an unpackLen of 0xffffffffffffffff requires an explicit end of
// stream marker.
type Decoder struct {
	properties Properties
	// length to unpack
	unpackLen        uint64
	decodedLen       uint64
	dict             *decoderDict
	state            uint32
	posBitMask       uint32
	rd               *rangeDecoder
	isMatch          [states << maxPosBits]prob
	isRep            [states]prob
	isRepG0          [states]prob
	isRepG1          [states]prob
	isRepG2          [states]prob
	isRepG0Long      [states << maxPosBits]prob
	rep              [4]uint32
	litDecoder       *literalCodec
	lengthDecoder    *lengthCodec
	repLengthDecoder *lengthCodec
	distDecoder      *distCodec
}

// NewDecoder creates an LZMA decoder. It reads the classic, original LZMA
// format. Note that LZMA2 uses a different header format.
func NewDecoder(r io.Reader) (d *Decoder, err error) {
	f := bufio.NewReader(r)
	properties, err := readProperties(f)
	if err != nil {
		return nil, err
	}
	historyLen := int(properties.DictLen)
	if historyLen < 0 {
		return nil, errors.New(
			"LZMA property DictLen exceeds maximum int value")
	}
	d = &Decoder{
		properties: *properties,
	}
	if d.unpackLen, err = readUint64LE(f); err != nil {
		return nil, err
	}
	if d.dict, err = newDecoderDict(bufferLen, historyLen); err != nil {
		return nil, err
	}
	d.posBitMask = (uint32(1) << uint(d.properties.PB)) - 1
	if d.rd, err = newRangeDecoder(f); err != nil {
		return nil, err
	}
	initProbSlice(d.isMatch[:])
	initProbSlice(d.isRep[:])
	initProbSlice(d.isRepG0[:])
	initProbSlice(d.isRepG1[:])
	initProbSlice(d.isRepG2[:])
	initProbSlice(d.isRepG0Long[:])
	d.litDecoder = newLiteralCodec(d.properties.LC, d.properties.LP)
	d.lengthDecoder = newLengthCodec()
	d.repLengthDecoder = newLengthCodec()
	d.distDecoder = newDistCodec()
	return d, nil
}

// Properties returns a set of properties.
func (d *Decoder) Properties() Properties {
	return d.properties
}

// getUint64LE converts the uint64 value stored as little endian to an uint64
// value.
func getUint64LE(b []byte) uint64 {
	x := uint64(b[7]) << 56
	x |= uint64(b[6]) << 48
	x |= uint64(b[5]) << 40
	x |= uint64(b[4]) << 32
	x |= uint64(b[3]) << 24
	x |= uint64(b[2]) << 16
	x |= uint64(b[1]) << 8
	x |= uint64(b[0])
	return x
}

// readUint64LE reads a uint64 little-endian integer from reader.
func readUint64LE(r io.Reader) (x uint64, err error) {
	b := make([]byte, 8)
	if _, err = io.ReadFull(r, b); err != nil {
		return 0, err
	}
	x = getUint64LE(b)
	return x, nil
}

// initProbSlice initializes a slice of probabilities.
func initProbSlice(p []prob) {
	for i := range p {
		p[i] = probInit
	}
}

// Reads reads data from the decoder stream.
//
// The method might block and is not reentrant.
//
// The end of the LZMA stream is indicated by EOF. There might be other errors
// returned. The decoder will not be able to recover from an error returned.
func (d *Decoder) Read(p []byte) (n int, err error) {
	for {
		var k int
		k, err = d.dict.Read(p[n:])
		n += k
		switch {
		case err == io.EOF:
			if n <= 0 {
				return 0, io.EOF
			}
			return n, nil
		case err != nil:
			return n, fmt.Errorf("LZMA - %s", err)
		case n == len(p):
			return n, nil
		}
		if err = d.fill(); err != nil {
			return n, fmt.Errorf("LZMA - %s", err)
		}
	}
}

// errUnexpectedEOS indicates that the function decoded an unexpected end of
// stream marker
var errUnexpectedEOS = errors.New("unexpected end of stream marker")

// fill puts at lest the requested number of bytes into the decoder dictionary.
func (d *Decoder) fill() error {
	if d.dict.eof {
		return nil
	}
	for d.dict.readable() < d.dict.b {
		op, err := d.decodeOp()
		if err != nil {
			switch {
			case err == eofDecoded:
				if d.unpackLen != noUnpackLen &&
					d.decodedLen != d.unpackLen {
					return errUnexpectedEOS
				}
				d.dict.eof = true
				return nil
			case err == io.EOF:
				return errors.New(
					"unexpected end of compressed stream")
			default:
				return err
			}
		}

		n := d.decodedLen + uint64(op.Len())
		if n < d.decodedLen {
			panic("negative op length or overflow of decodedLen")
		}
		if n > d.unpackLen {
			d.dict.eof = true
			return errors.New("decoded stream too long")
		}
		d.decodedLen = n

		if err = op.applyDecoderDict(d.dict); err != nil {
			return err
		}
		if n == d.unpackLen {
			d.dict.eof = true
			return nil
		}
	}
	return nil
}

// updateStateLiteral updates the state for a literal.
func (d *Decoder) updateStateLiteral() {
	switch {
	case d.state < 4:
		d.state = 0
		return
	case d.state < 10:
		d.state -= 3
		return
	}
	d.state -= 6
}

// updateStateMatch updates the state for a match.
func (d *Decoder) updateStateMatch() {
	if d.state < 7 {
		d.state = 7
	} else {
		d.state = 10
	}
}

// updateStateRep updates the state for a repetition.
func (d *Decoder) updateStateRep() {
	if d.state < 7 {
		d.state = 8
	} else {
		d.state = 11
	}
}

// updateStateShortRep updates the state for a short repetition.
func (d *Decoder) updateStateShortRep() {
	if d.state < 7 {
		d.state = 9
	} else {
		d.state = 11
	}
}

var litCounter int

// decodeLiteral decodes a literal.
func (d *Decoder) decodeLiteral() (op operation, err error) {
	prevByte := d.dict.getByte(1)
	lp, lc := uint(d.properties.LP), uint(d.properties.LC)
	litState := ((uint32(d.dict.total) & ((1 << lp) - 1)) << lc) |
		(uint32(prevByte) >> (8 - lc))

	litCounter++
	xlog.Printf(Debug, "L %3d %2d 0x%02x %3d\n", litCounter, litState,
		prevByte, d.dict.total)

	match := d.dict.getByte(int(d.rep[0]) + 1)
	s, err := d.litDecoder.Decode(d.rd, d.state, match, litState)
	if err != nil {
		return nil, err
	}
	return lit{s}, nil
}

// errWrongTermination indicates that a termination symbol has been received,
// but the range decoder could still produces more data
var errWrongTermination = errors.New(
	"range decoder doesn't support termination")

// eofDecoded indicates an EOF of the decoded file
var eofDecoded = errors.New("EOF of decoded stream")

var opCounter int

// decodeOp decodes an operation. The function returns eofDecoded if there is
// an explicit termination marker.
func (d *Decoder) decodeOp() (op operation, err error) {
	posState := uint32(d.dict.total) & d.posBitMask

	opCounter++
	xlog.Printf(Debug, "S %3d %2d %2d\n", opCounter, d.state, posState)

	state2 := (d.state << maxPosBits) | posState

	b, err := d.isMatch[state2].Decode(d.rd)
	if err != nil {
		return nil, err
	}
	if b == 0 {
		// literal
		op, err := d.decodeLiteral()
		if err != nil {
			return nil, err
		}
		d.updateStateLiteral()
		return op, nil
	}
	b, err = d.isRep[d.state].Decode(d.rd)
	if err != nil {
		return nil, err
	}
	if b == 0 {
		// simple match
		d.rep[3], d.rep[2], d.rep[1] = d.rep[2], d.rep[1], d.rep[0]
		d.updateStateMatch()
		// The length decoder returns the length offset.
		l, err := d.lengthDecoder.Decode(d.rd, posState)
		if err != nil {
			return nil, err
		}
		// The dist decoder returns the distance offset. The actual
		// distance is 1 higher.
		d.rep[0], err = d.distDecoder.Decode(l, d.rd)
		if err != nil {
			return nil, err
		}
		if d.rep[0] == 0xffffffff {
			if !d.rd.possiblyAtEnd() {
				return nil, errWrongTermination
			}
			return nil, eofDecoded
		}
		op = rep{length: int(l) + minLength,
			distance: int(d.rep[0]) + minDistance}
		return op, nil
	}
	b, err = d.isRepG0[d.state].Decode(d.rd)
	if err != nil {
		return nil, err
	}
	dist := d.rep[0]
	if b == 0 {
		// rep match 0
		b, err = d.isRepG0Long[state2].Decode(d.rd)
		if err != nil {
			return nil, err
		}
		if b == 0 {
			d.updateStateShortRep()
			op = rep{length: 1,
				distance: int(d.rep[0]) + minDistance}
			return op, nil
		}
	} else {
		b, err = d.isRepG1[d.state].Decode(d.rd)
		if err != nil {
			return nil, err
		}
		if b == 0 {
			dist = d.rep[1]
		} else {
			b, err = d.isRepG2[d.state].Decode(d.rd)
			if err != nil {
				return nil, err
			}
			if b == 0 {
				dist = d.rep[2]
			} else {
				dist = d.rep[3]
				d.rep[3] = d.rep[2]
			}
			d.rep[2] = d.rep[1]
		}
		d.rep[1] = d.rep[0]
		d.rep[0] = dist
	}
	l, err := d.repLengthDecoder.Decode(d.rd, posState)
	if err != nil {
		return nil, err
	}
	d.updateStateRep()
	op = rep{length: int(l) + minLength, distance: int(dist) + minDistance}
	return op, nil
}
