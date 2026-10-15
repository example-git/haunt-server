package identity

import (
	"strings"
	"testing"
)

func TestPasswordPolicyEightCharacters(t *testing.T) {
	for _, value := range []string{"1234567", strings.Repeat("é", 7), "\xff12345678", strings.Repeat("x", 1025)} {
		if validPassword(value) {
			t.Fatal("invalid password accepted")
		}
	}
	for _, value := range []string{"12345678", strings.Repeat("é", 8), strings.Repeat("x", 1024)} {
		if !validPassword(value) {
			t.Fatal("valid password rejected")
		}
	}
}
