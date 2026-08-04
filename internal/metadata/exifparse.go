package metadata

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

// TIFF 6.0 value types. A type outside this set has no defined width, so
// an entry carrying one cannot be located and the block is refused.
const (
	exifByte      uint16 = 1
	exifASCII     uint16 = 2
	exifShort     uint16 = 3
	exifLong      uint16 = 4
	exifRational  uint16 = 5
	exifSByte     uint16 = 6
	exifUndefined uint16 = 7
	exifSShort    uint16 = 8
	exifSLong     uint16 = 9
	exifSRational uint16 = 10
	exifFloat     uint16 = 11
	exifDouble    uint16 = 12
)

// exifTypeSize is each type's width in bytes; a missing entry means the
// type is unknown.
var exifTypeSize = map[uint16]uint64{
	exifByte: 1, exifASCII: 1, exifShort: 2, exifLong: 4,
	exifRational: 8, exifSByte: 1, exifUndefined: 1, exifSShort: 2,
	exifSLong: 4, exifSRational: 8, exifFloat: 4, exifDouble: 8,
}

// The sub-IFD pointer tags, by the name the tables give them.
const (
	exifIFDPointer    = "ExifIFDPointer"
	gpsIFDPointer     = "GPSInfoIFDPointer"
	interopIFDPointer = "InteroperabilityIFDPointer"
	userCommentField  = "UserComment"
)

var (
	errEXIFHeader = errors.New("exif: not a TIFF header")
	errEXIFEntry  = errors.New("exif: entry value lies outside the block")
	errEXIFType   = errors.New("exif: unknown entry type")
	errEXIFNoDirs = errors.New("exif: no image file directory")
)

// maxEXIFDirs bounds the IFD chain. Real files carry one or two; the cap
// only has to keep a hostile chain from walking forever, and the visited
// set below already rejects the cycles that would.
const maxEXIFDirs = 64

// exifTag is one decoded IFD entry. val always holds exactly
// size(typ)*count bytes, so the accessors below cannot run off the end.
type exifTag struct {
	id    uint16
	typ   uint16
	count uint32
	val   []byte
	order binary.ByteOrder
}

// exifData is the named tags an EXIF block yielded, keyed by EXIF field
// name. Tags the tables do not name are dropped.
type exifData struct {
	tags map[string]*exifTag
}

func (x *exifData) get(name string) (*exifTag, bool) {
	t, ok := x.tags[name]
	return t, ok
}

// walk calls fn for every named tag. Map order, so callers that render a
// list sort it themselves.
func (x *exifData) walk(fn func(name string, t *exifTag)) {
	for name, t := range x.tags {
		fn(name, t)
	}
}

// decodeEXIF parses a raw TIFF/EXIF block: the header, the IFD chain, and
// the Exif, GPS and Interoperability sub-IFDs the chain points at. Every
// offset and length is checked against the block, so a truncated or
// hostile block is read within its bounds or not at all.
//
// Decoding past IFD0 is best-effort. IFD0 is where the tags an operator
// recognises live, so a block whose header or IFD0 will not parse yields
// nothing; but a later directory or a sub-IFD that will not parse is
// dropped rather than discarding what already decoded. A directory that
// fails contributes no tags either way, so keeping the rest cannot
// surface a wrong value - only fewer of them.
func decodeEXIF(data []byte) (*exifData, error) {
	order, first, err := exifHeader(data)
	if err != nil {
		return nil, err
	}
	dirs, err := exifDirs(data, order, first)
	if err != nil {
		return nil, err
	}

	x := &exifData{tags: make(map[string]*exifTag)}
	x.load(dirs[0], exifTagNames)
	// IFD1 describes the embedded thumbnail, so its ids are read against
	// the thumbnail table rather than IFD0's.
	if len(dirs) > 1 {
		x.load(dirs[1], thumbTagNames)
	}

	subIFDs := []struct {
		pointer string
		names   map[uint16]string
	}{
		{exifIFDPointer, exifTagNames},
		{gpsIFDPointer, gpsTagNames},
		{interopIFDPointer, interopTagNames},
	}
	for _, sub := range subIFDs {
		x.loadSubIFD(data, order, sub.pointer, sub.names)
	}
	return x, nil
}

// exifHeader reads the 8-byte TIFF header and returns the byte order and
// the offset of the first IFD.
func exifHeader(data []byte) (binary.ByteOrder, uint64, error) {
	if len(data) < 8 {
		return nil, 0, errEXIFHeader
	}
	var order binary.ByteOrder
	switch string(data[0:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return nil, 0, errEXIFHeader
	}
	if order.Uint16(data[2:4]) != 42 {
		return nil, 0, errEXIFHeader
	}
	return order, uint64(order.Uint32(data[4:8])), nil
}

// exifDirs walks the IFD chain from off, stopping at the first directory
// that will not decode and returning the ones before it. A directory
// already visited also ends the walk: a chain that revisits one is cyclic,
// and following it would not terminate.
func exifDirs(data []byte, order binary.ByteOrder, off uint64) ([][]*exifTag, error) {
	var dirs [][]*exifTag
	seen := make(map[uint64]bool)
	for off != 0 && !seen[off] && len(dirs) < maxEXIFDirs {
		seen[off] = true
		tags, next, err := exifDir(data, order, off)
		if err != nil {
			break
		}
		dirs = append(dirs, tags)
		off = next
	}
	if len(dirs) == 0 {
		return nil, errEXIFNoDirs
	}
	return dirs, nil
}

// exifDir decodes the directory at off and returns its entries plus the
// offset of the next directory in the chain.
func exifDir(data []byte, order binary.ByteOrder, off uint64) ([]*exifTag, uint64, error) {
	start := off
	if start+2 > uint64(len(data)) {
		return nil, 0, errEXIFEntry
	}
	n := uint64(order.Uint16(data[start : start+2]))
	// count word + n 12-byte entries + the next-directory offset.
	end := start + 2 + n*12 + 4
	if end > uint64(len(data)) {
		return nil, 0, errEXIFEntry
	}

	tags := make([]*exifTag, 0, n)
	for i := uint64(0); i < n; i++ {
		e := data[start+2+i*12:][:12]
		tag, err := exifEntry(data, order, e)
		if err != nil {
			return nil, 0, err
		}
		tags = append(tags, tag)
	}
	return tags, uint64(order.Uint32(data[end-4 : end])), nil
}

// exifEntry decodes one 12-byte IFD entry, resolving its value either
// inline or through the offset field.
func exifEntry(data []byte, order binary.ByteOrder, e []byte) (*exifTag, error) {
	id := order.Uint16(e[0:2])
	typ := order.Uint16(e[2:4])
	count := order.Uint32(e[4:8])

	size, ok := exifTypeSize[typ]
	if !ok {
		return nil, errEXIFType
	}
	// The product is computed in 64 bits, so a count chosen to wrap a
	// 32-bit width cannot make an oversized value look small.
	length := size * uint64(count)
	if length == 0 {
		return nil, errEXIFEntry
	}

	var val []byte
	if length > 4 {
		off := uint64(order.Uint32(e[8:12]))
		if off+length > uint64(len(data)) {
			return nil, errEXIFEntry
		}
		val = data[off : off+length]
	} else {
		val = e[8 : 8+length]
	}
	return &exifTag{id: id, typ: typ, count: count, val: val, order: order}, nil
}

// load records every entry the table names. A later directory overwrites
// an earlier one's tag of the same name, which is how a sub-IFD's copy of
// a field wins over IFD0's.
func (x *exifData) load(tags []*exifTag, names map[uint16]string) {
	for _, t := range tags {
		if name, ok := names[t.id]; ok {
			x.tags[name] = t
		}
	}
}

// loadSubIFD follows a pointer tag loaded from IFD0 and reads the
// directory it addresses. A pointer that is absent, not an integer, or
// aimed at something that will not decode leaves the tag set as it was.
func (x *exifData) loadSubIFD(data []byte, order binary.ByteOrder, pointer string, names map[uint16]string) {
	t, ok := x.get(pointer)
	if !ok {
		return
	}
	off, ok := t.intAt(0)
	if !ok || off < 0 || uint64(off) > uint64(len(data)) {
		return
	}
	tags, _, err := exifDir(data, order, uint64(off))
	if err != nil {
		return
	}
	x.load(tags, names)
}

// intAt returns the i'th value of an integer-typed tag.
func (t *exifTag) intAt(i int) (int64, bool) {
	vals := t.ints()
	if vals == nil || i >= len(vals) {
		return 0, false
	}
	return vals[i], true
}

// stringVal returns the text of an ASCII tag, stopping at the first NUL so
// a padded or over-counted value does not carry its padding along.
func (t *exifTag) stringVal() (string, bool) {
	if t.typ != exifASCII {
		return "", false
	}
	if n := bytes.IndexByte(t.val, 0); n >= 0 {
		return string(t.val[:n]), true
	}
	return string(t.val), true
}

func (t *exifTag) ints() []int64 {
	width, signed := 0, false
	switch t.typ {
	case exifByte:
		width = 1
	case exifSByte:
		width, signed = 1, true
	case exifShort:
		width = 2
	case exifSShort:
		width, signed = 2, true
	case exifLong:
		width = 4
	case exifSLong:
		width, signed = 4, true
	default:
		return nil
	}
	out := make([]int64, t.count)
	for i := range out {
		b := t.val[i*width:]
		var u uint64
		switch width {
		case 1:
			u = uint64(b[0])
		case 2:
			u = uint64(t.order.Uint16(b))
		case 4:
			u = uint64(t.order.Uint32(b))
		}
		if signed {
			// Sign-extend from the type's width.
			shift := 64 - width*8
			out[i] = int64(u<<shift) >> shift
			continue
		}
		out[i] = int64(u)
	}
	return out
}

func (t *exifTag) floats() []float64 {
	out := make([]float64, t.count)
	for i := range out {
		switch t.typ {
		case exifFloat:
			out[i] = float64(math.Float32frombits(t.order.Uint32(t.val[i*4:])))
		case exifDouble:
			out[i] = math.Float64frombits(t.order.Uint64(t.val[i*8:]))
		default:
			return nil
		}
	}
	return out
}

func (t *exifTag) rats() [][2]int64 {
	out := make([][2]int64, t.count)
	for i := range out {
		b := t.val[i*8:]
		n, d := t.order.Uint32(b), t.order.Uint32(b[4:])
		switch t.typ {
		case exifRational:
			out[i] = [2]int64{int64(n), int64(d)}
		case exifSRational:
			out[i] = [2]int64{int64(int32(n)), int64(int32(d))}
		default:
			return nil
		}
	}
	return out
}

// String renders the tag for the detail page's metadata panel: a bare
// value when the tag holds one, a bracketed list when it holds several.
// Text and undefined tags render quoted, and the quoting plus the
// single-value unwrapping are what the panel has always shown.
func (t *exifTag) String() string {
	body := t.render()
	if t.count == 1 {
		return strings.Trim(body, "[]")
	}
	return body
}

func (t *exifTag) render() string {
	if t.typ == exifASCII || t.typ == exifUndefined {
		return quotePrintable(t.val)
	}
	parts := make([]string, 0, t.count)
	switch t.typ {
	case exifRational, exifSRational:
		for _, r := range t.rats() {
			parts = append(parts, fmt.Sprintf(`"%v/%v"`, r[0], r[1]))
		}
	case exifFloat, exifDouble:
		for _, f := range t.floats() {
			parts = append(parts, fmt.Sprintf("%v", f))
		}
	default:
		for _, n := range t.ints() {
			parts = append(parts, fmt.Sprintf("%v", n))
		}
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// quotePrintable renders raw tag bytes as a quoted string, dropping the
// bytes that would not print. Dropping them one byte at a time can split a
// multi-byte rune, so a result that is no longer valid UTF-8 is reported
// as empty rather than handed to a template as broken text.
func quotePrintable(in []byte) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range in {
		if unicode.IsPrint(rune(c)) {
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	if s := b.String(); utf8.ValidString(s) {
		return s
	}
	return `""`
}
