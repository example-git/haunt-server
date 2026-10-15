package identity

import (
	"context"
	"errors"
	"testing"
)

func TestPasswordVerificationAndAdmission(t *testing.T) {
	p, err := NewPasswords(1)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := p.Hash(context.Background(), "my long test password")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []struct {
		password string
		want     bool
	}{{"my long test password", true}, {"wrong password", false}} {
		match, err := p.Verify(context.Background(), v.password, hash)
		if err != nil || match != v.want {
			t.Fatalf("verification got %v, %v", match, err)
		}
	}
	p.slots <- struct{}{}
	if _, err := p.Hash(context.Background(), "password"); !errors.Is(err, ErrBusy) {
		t.Fatalf("saturated hashing admission got %v", err)
	}
	<-p.slots
	if _, err := p.Verify(context.Background(), "password", "$argon2id$v=19$m=4294967295,t=1000,p=4$AAAA$AAAA"); err == nil {
		t.Fatal("accepted unbounded hash parameters")
	}
}
