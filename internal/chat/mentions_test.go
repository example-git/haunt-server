package chat

import (
	"reflect"
	"testing"
)

func TestMentionNamesBoundaries(t *testing.T) {
	names := MentionNames("🙂 (@Alice), @bob! @owner_123\n@CAROL @alice mail@alice.test https://host/@bob @ab @abcdefghijklmnopqrstuvwxyz123456789")
	if !reflect.DeepEqual(names, []string{"alice", "bob", "owner_123", "carol"}) {
		t.Fatal(names)
	}
}
