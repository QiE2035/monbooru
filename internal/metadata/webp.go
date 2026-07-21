package metadata

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"

	"github.com/monbooru/monbooru/internal/models"
	"github.com/rwcarlsen/goexif/exif"
)

// exifMagic is the JPEG-style EXIF header WebP encoders may or may not
// prepend to the RIFF EXIF chunk; we strip it on read and re-prepend a
// known-good copy before handing the payload to exif.Decode.
var exifMagic = []byte("Exif\x00\x00")

// extractSDFromWebP reads A1111 metadata from a WebP's EXIF chunk.
func extractSDFromWebP(path string) *models.SDMetadata {
	x, err := decodeWebPEXIF(path)
	if err != nil || x == nil {
		return nil
	}
	return sdFromEXIF(x)
}

// decodeWebPEXIF walks the WebP RIFF container for its EXIF chunk and
// decodes it, re-prefixing exifMagic so exif.Decode succeeds whether or
// not the chunk already carries the header. Returns nil for non-WebP or
// no EXIF chunk.
func decodeWebPEXIF(path string) (*exif.Exif, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	exifData, err := readWebPEXIF(f)
	if err != nil || exifData == nil {
		return nil, err
	}
	return exif.Decode(io.MultiReader(bytes.NewReader(exifMagic), bytes.NewReader(exifData)))
}

// readWebPEXIF returns the raw EXIF chunk bytes from a WebP RIFF
// stream, or nil for non-WebP or no EXIF chunk.
func readWebPEXIF(r io.Reader) ([]byte, error) {
	header := make([]byte, 12)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WEBP" {
		return nil, nil
	}
	for {
		chunk := make([]byte, 8)
		if _, err := io.ReadFull(r, chunk); err != nil {
			return nil, nil
		}
		chunkType := string(chunk[0:4])
		size := binary.LittleEndian.Uint32(chunk[4:8])
		if size > maxChunkBytes {
			// Skip oversize chunks wholesale, advancing past payload +
			// padding so subsequent chunks still line up.
			toSkip := int64(size)
			if size%2 == 1 {
				toSkip++
			}
			if _, err := io.CopyN(io.Discard, r, toSkip); err != nil {
				return nil, nil
			}
			continue
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, nil
		}
		if size%2 == 1 {
			// RIFF chunks are word-aligned; skip the pad byte.
			pad := make([]byte, 1)
			_, _ = io.ReadFull(r, pad)
		}
		if chunkType == "EXIF" {
			// Some encoders prepend the JPEG-style EXIF magic; strip it so
			// the caller can re-prepend a known-good copy.
			data = bytes.TrimPrefix(data, exifMagic)
			return data, nil
		}
	}
}

// genericFromWebP returns EXIF tags from a WebP file (UserComment excluded).
func genericFromWebP(path string) []models.SDParam {
	x, err := decodeWebPEXIF(path)
	if err != nil || x == nil {
		return nil
	}
	return collectEXIFTags(x)
}
