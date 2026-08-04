package metadata

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"os"
)

// decodeJPEGEXIF reads a JPEG's EXIF block and decodes it. Returns nil
// when the file carries no EXIF or the block does not decode.
func decodeJPEGEXIF(path string) *exifData {
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
