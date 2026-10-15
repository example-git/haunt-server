package identity

import (
	"bytes"
	"encoding/binary"
	"image/gif"
)

const MaxArtBytes = 20 << 20

// Validate frames independently: animated images cannot force all decoded frames
// into memory at once. The total pixel/frame budgets also bound decoder CPU.
func validateGIF(raw []byte, maxWidth, maxHeight int) error {
	if len(raw) < 14 || (string(raw[:6]) != "GIF87a" && string(raw[:6]) != "GIF89a") {
		return ErrInvalid
	}
	width, height := int(binary.LittleEndian.Uint16(raw[6:8])), int(binary.LittleEndian.Uint16(raw[8:10]))
	if width < 1 || height < 1 || width > maxWidth || height > maxHeight {
		return ErrInvalid
	}
	at := 13
	if raw[10]&0x80 != 0 {
		at += 3 * (1 << ((raw[10] & 7) + 1))
	}
	if at >= len(raw) {
		return ErrInvalid
	}
	header := raw[:at]
	frames, pixels := 0, 0
	blocks := func() bool {
		for at < len(raw) {
			n := int(raw[at])
			at++
			if n == 0 {
				return true
			}
			if n > len(raw)-at {
				return false
			}
			at += n
		}
		return false
	}
	for at < len(raw) {
		start := at
		tag := raw[at]
		at++
		switch tag {
		case 0x3b:
			if frames == 0 || at != len(raw) {
				return ErrInvalid
			}
			return nil
		case 0x21:
			if at >= len(raw) {
				return ErrInvalid
			}
			label := raw[at]
			at++
			if label == 0xf9 && (len(raw)-at < 6 || raw[at] != 4 || raw[at+5] != 0) {
				return ErrInvalid
			}
			if !blocks() {
				return ErrInvalid
			}
		case 0x2c:
			if len(raw)-at < 9 {
				return ErrInvalid
			}
			w, h := int(binary.LittleEndian.Uint16(raw[at+4:at+6])), int(binary.LittleEndian.Uint16(raw[at+6:at+8]))
			pixels += w * h
			frames++
			if w < 1 || h < 1 || w > width || h > height || frames > 1000 || pixels > 256*1024*1024 {
				return ErrInvalid
			}
			packed := raw[at+8]
			at += 9
			if packed&0x80 != 0 {
				at += 3 * (1 << ((packed & 7) + 1))
			}
			if at >= len(raw) {
				return ErrInvalid
			}
			at++ // LZW code size
			if !blocks() {
				return ErrInvalid
			}
			frame := make([]byte, 0, len(header)+at-start+1)
			frame = append(frame, header...)
			frame = append(frame, raw[start:at]...)
			frame = append(frame, 0x3b)
			if _, err := gif.Decode(bytes.NewReader(frame)); err != nil {
				return ErrInvalid
			}
		default:
			return ErrInvalid
		}
	}
	return ErrInvalid
}
