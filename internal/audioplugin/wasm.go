package audioplugin

import "bytes"

type wasmReader struct {
	b   []byte
	pos int
	bad bool
}

func (r *wasmReader) byte() byte {
	if r.pos >= len(r.b) {
		r.bad = true
		return 0
	}
	v := r.b[r.pos]
	r.pos++
	return v
}
func (r *wasmReader) number() uint32 {
	var n uint32
	for i := 0; i < 5; i++ {
		v := r.byte()
		if i == 4 && v > 15 {
			r.bad = true
		}
		n |= uint32(v&127) << uint(7*i)
		if v&128 == 0 {
			return n
		}
	}
	r.bad = true
	return 0
}
func (r *wasmReader) name() string {
	n := r.number()
	if uint64(r.pos)+uint64(n) > uint64(len(r.b)) {
		r.bad = true
		return ""
	}
	v := string(r.b[r.pos : r.pos+int(n)])
	r.pos += int(n)
	return v
}
func (r *wasmReader) limit(max uint32) bool {
	flags := r.number()
	min, cap := r.number(), r.number()
	return flags == 1 && min <= cap && cap <= max && !r.bad
}

// Parse only the resource/import declarations, without compiling or executing
// any code. The browser independently validates again and verifies the full Wasm.
func validWasm(b []byte) bool {
	if len(b) < 8 || int64(len(b)) > MaxBytes || !bytes.Equal(b[:8], []byte{0, 97, 115, 109, 1, 0, 0, 0}) {
		return false
	}
	r := wasmReader{b: b, pos: 8}
	memories := 0
	for r.pos < len(b) {
		section := r.byte()
		size := r.number()
		end := uint64(r.pos) + uint64(size)
		if r.bad || end > uint64(len(b)) {
			return false
		}
		switch section {
		case 1:
			count := r.number()
			if count > 2048 {
				return false
			}
			for i := uint32(0); i < count; i++ {
				if r.byte() != 0x60 {
					return false
				}
				for side := 0; side < 2; side++ {
					args := r.number()
					if args > 32 {
						return false
					}
					for j := uint32(0); j < args; j++ {
						v := r.byte()
						if v < 0x7b || v > 0x7f {
							return false
						}
					}
				}
			}
		case 2:
			count := r.number()
			if count > 8 {
				return false
			}
			for i := uint32(0); i < count; i++ {
				module, name := r.name(), r.name()
				if module != "wasi_snapshot_preview1" || r.byte() != 0 {
					return false
				}
				switch name {
				case "proc_exit", "fd_write", "fd_close", "fd_seek", "clock_time_get":
				default:
					return false
				}
				r.number()
			}
		case 4:
			count := r.number()
			if count > 1 {
				return false
			}
			for i := uint32(0); i < count; i++ {
				if r.byte() != 0x70 || !r.limit(8192) {
					return false
				}
			}
		case 5:
			if memories != 0 || r.number() != 1 || !r.limit(1024) {
				return false
			}
			memories++
		case 8:
			return false
		}
		if r.bad || uint64(r.pos) > end {
			return false
		}
		r.pos = int(end)
	}
	return memories == 1
}
