package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/core"
)

func TestLookup(t *testing.T) {
	t.Parallel()
	digest := sha256.Sum256([]byte("sk-hashed"))
	keys := NewKeys([]config.Key{
		{Name: "plain", Key: "sk-plain"},
		{Name: "hashed", SHA256: hex.EncodeToString(digest[:]), Models: []string{"m1"}},
	})
	tests := []struct {
		key      string
		wantName string
		wantKind core.ErrorKind
	}{
		{"sk-plain", "plain", 0},
		{"sk-hashed", "hashed", 0},
		{"", "", core.KindAuthentication},
		{"sk-plai", "", core.KindAuthentication},
		{hex.EncodeToString(digest[:]), "", core.KindAuthentication}, // the digest is not the key
	}
	for _, tt := range tests {
		p, err := keys.Lookup(tt.key)
		if tt.wantName == "" {
			if err == nil || core.AsError(err).Kind != tt.wantKind {
				t.Errorf("Lookup(%q) = %v, %v; want %v", tt.key, p, err, tt.wantKind)
			}
			continue
		}
		if err != nil || p.Name != tt.wantName {
			t.Errorf("Lookup(%q) = %v, %v; want %s", tt.key, p, err, tt.wantName)
		}
	}
	p, _ := keys.Lookup("sk-hashed")
	if !p.Allows("m1") || p.Allows("m2") {
		t.Error("model allow list not applied")
	}
	p, _ = keys.Lookup("sk-plain")
	if !p.Allows("anything") {
		t.Error("key without a list should allow every model")
	}
	ctx := WithPrincipal(context.Background(), p)
	if FromContext(ctx) != p || FromContext(context.Background()) != nil {
		t.Error("context round trip")
	}
}
