package audioplugin

import "testing"

func TestWasmCapabilitiesAndResourceBounds(t *testing.T) {
	header := []byte{0, 97, 115, 109, 1, 0, 0, 0}
	memory := []byte{5, 4, 1, 1, 1, 1}
	if !validWasm(append(append([]byte{}, header...), memory...)) {
		t.Fatal("bounded memory rejected")
	}
	for name, section := range map[string][]byte{
		"unbounded memory": {5, 3, 1, 0, 1},
		"shared memory":    {5, 4, 1, 3, 1, 1},
		"large memory":     {5, 5, 1, 1, 1, 0x81, 8},
		"start function":   {8, 1, 0},
		"unbounded table":  {4, 4, 1, 0x70, 0, 1},
		"GC type":          {1, 2, 1, 0x5f},
		"host fetch":       {2, 13, 1, 3, 'e', 'n', 'v', 5, 'f', 'e', 't', 'c', 'h', 0, 0},
		"imported memory":  {2, 13, 1, 3, 'e', 'n', 'v', 3, 'm', 'e', 'm', 2, 1, 1, 1},
		"duplicate memory": memory,
	} {
		t.Run(name, func(t *testing.T) {
			b := append(append(append([]byte{}, header...), memory...), section...)
			if validWasm(b) {
				t.Fatal("accepted forbidden declaration")
			}
		})
	}
	if validWasm([]byte("fetch('https://example.invalid')")) {
		t.Fatal("accepted script")
	}
}
