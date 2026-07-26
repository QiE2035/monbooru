package metadata

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"

	"github.com/rwcarlsen/goexif/exif"
)

// errEXIFBounds rejects a block whose IFD entries describe more bytes
// than the block holds.
var errEXIFBounds = errors.New("exif: tag value length exceeds the block")

// tiffTypeSize mirrors goexif's per-type component width so the bounds
// check below multiplies the same numbers the decoder will.
var tiffTypeSize = map[uint16]uint64{
	1: 1, 2: 1, 3: 2, 4: 4, 5: 8, 6: 1, 7: 1, 8: 2, 9: 4, 10: 8, 11: 4, 12: 8,
}

// subIFDTags are the pointer tags whose value is the offset of another
// directory goexif will descend into, so their entries need checking too.
var subIFDTags = map[uint16]bool{0x8769: true, 0x8825: true, 0xA005: true}

// decodeEXIF decodes a TIFF/EXIF block, refusing one whose tags claim
// more value bytes than the block contains. goexif derives a tag's
// value length from a uint32 product of type size and component count;
// a count chosen to wrap that product small slips past its own guards
// and it then allocates on the unwrapped count - gigabytes out of a few
// dozen bytes, which is a fatal runtime OOM no recover can catch. Any
// entry this check rejects already fails goexif's own short-read guard
// when the product does not wrap, so nothing decodable is turned away.
func decodeEXIF(tiffData []byte) (*exif.Exif, error) {
	if !exifCountsInBounds(tiffData) {
		return nil, errEXIFBounds
	}
	return exif.Decode(io.MultiReader(bytes.NewReader(exifMagic), bytes.NewReader(tiffData)))
}

// decodeJPEGEXIF reads a JPEG's EXIF block and decodes it. Returns nil
// when the file carries no EXIF or the block fails the bounds check.
func decodeJPEGEXIF(path string) *exif.Exif {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	block := jpegEXIF(f)
	if block == nil {
		return nil
	}
	x, err := decodeEXIF(block)
	if err != nil {
		return nil
	}
	return x
}

// exifCountsInBounds walks the IFD chain and every sub-directory it
// points at, reporting whether each entry's declared value fits inside
// data.
func exifCountsInBounds(data []byte) bool {
	if len(data) < 8 {
		return false
	}
	var order binary.ByteOrder
	switch string(data[0:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return false
	}
	if order.Uint16(data[2:4]) != 42 {
		return false
	}
	seen := map[uint32]bool{}
	pending := []uint32{order.Uint32(data[4:8])}
	for len(pending) > 0 {
		off := pending[0]
		pending = pending[1:]
		if off == 0 || seen[off] {
			continue
		}
		seen[off] = true
		links, ok := checkIFD(data, order, off)
		if !ok {
			return false
		}
		pending = append(pending, links...)
	}
	return true
}

// checkIFD validates one directory's entries and returns the offsets it
// links to: the next directory in the chain plus any sub-IFD pointers.
func checkIFD(data []byte, order binary.ByteOrder, off uint32) ([]uint32, bool) {
	start := int64(off)
	if start+2 > int64(len(data)) {
		return nil, false
	}
	n := int64(order.Uint16(data[start : start+2]))
	end := start + 2 + n*12 + 4
	if end > int64(len(data)) {
		return nil, false
	}
	var links []uint32
	for i := int64(0); i < n; i++ {
		e := start + 2 + i*12
		id := order.Uint16(data[e : e+2])
		typ := order.Uint16(data[e+2 : e+4])
		count := uint64(order.Uint32(data[e+4 : e+8]))
		if tiffTypeSize[typ]*count > uint64(len(data)) {
			return nil, false
		}
		if subIFDTags[id] && typ == 4 && count == 1 {
			links = append(links, order.Uint32(data[e+8:e+12]))
		}
	}
	return append(links, order.Uint32(data[end-4:end])), true
}

// jpegEXIF returns the TIFF payload of a JPEG's APP1 EXIF segment, or
// nil when the file carries none. Segment lengths are uint16, so the
// buffered read is bounded by the format itself.
func jpegEXIF(r io.Reader) []byte {
	br := bufio.NewReader(r)
	head := make([]byte, 2)
	if _, err := io.ReadFull(br, head); err != nil || head[0] != 0xFF || head[1] != 0xD8 {
		return nil
	}
	for {
		marker, err := nextJPEGMarker(br)
		if err != nil {
			return nil
		}
		// SOS opens the entropy-coded data and EOI ends the file; EXIF
		// always sits in an APP segment ahead of both. The standalone
		// markers carry no length word, so skipping them keeps the
		// segment walk aligned.
		switch {
		case marker == 0xDA || marker == 0xD9:
			return nil
		case marker == 0x01 || marker == 0xD8 || (marker >= 0xD0 && marker <= 0xD7):
			continue
		}
		lenBuf := make([]byte, 2)
		if _, err := io.ReadFull(br, lenBuf); err != nil {
			return nil
		}
		size := int(binary.BigEndian.Uint16(lenBuf))
		if size < 2 {
			return nil
		}
		payload := make([]byte, size-2)
		if _, err := io.ReadFull(br, payload); err != nil {
			return nil
		}
		if marker == 0xE1 && bytes.HasPrefix(payload, exifMagic) {
			return payload[len(exifMagic):]
		}
	}
}

// nextJPEGMarker advances to the next marker byte, tolerating the 0xFF
// fill bytes a segment may be padded with and the 0xFF00 stuffing that
// stands for a literal 0xFF.
func nextJPEGMarker(br *bufio.Reader) (byte, error) {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return 0, err
		}
		if b != 0xFF {
			continue
		}
		for b == 0xFF {
			if b, err = br.ReadByte(); err != nil {
				return 0, err
			}
		}
		if b != 0x00 {
			return b, nil
		}
	}
}
