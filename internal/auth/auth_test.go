package auth

import (
	"strings"
	"testing"
)

func TestGenerateKey(t *testing.T) {
	k1, h1, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k1, KeyPrefix) {
		t.Errorf("key %q missing prefix %q", k1, KeyPrefix)
	}
	if len(k1) != len(KeyPrefix)+48 {
		t.Errorf("key length = %d, want %d", len(k1), len(KeyPrefix)+48)
	}
	if len(h1) != 64 {
		t.Errorf("hash length = %d, want 64 hex chars", len(h1))
	}
	if h1 != HashKey(k1) {
		t.Error("returned hash does not match HashKey(plaintext)")
	}
	if !LooksLikeKey(k1) {
		t.Error("LooksLikeKey(generated) = false")
	}

	k2, h2, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k2 || h1 == h2 {
		t.Error("two generated keys collide")
	}
}

func TestHashKeyDeterministic(t *testing.T) {
	if HashKey("sk-gw-abc") != HashKey("sk-gw-abc") {
		t.Error("HashKey is not deterministic")
	}
	if HashKey("sk-gw-abc") == HashKey("sk-gw-abd") {
		t.Error("different keys hash equal")
	}
}

func TestEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"secret", "secret", true},
		{"secret", "Secret", false},
		{"secret", "secret2", false}, // different lengths must not panic or match
		{"", "", true},
		{"", "x", false},
	}
	for _, tc := range cases {
		if got := Equal(tc.a, tc.b); got != tc.want {
			t.Errorf("Equal(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
