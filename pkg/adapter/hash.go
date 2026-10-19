package adapter

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// HashPlan computes the content hash of a plan: every field that determines
// what apply will do. IDs, timestamps and the display diff are excluded, so
// two identical plans hash the same. Approvals bind to this hash.
func HashPlan(p *Plan) string {
	h := struct {
		Adapter          string          `json:"adapter"`
		Target           string          `json:"target"`
		Env              string          `json:"env"`
		Operation        Operation       `json:"operation"`
		Effects          []Effect        `json:"effects"`
		Reversibility    Reversibility   `json:"reversibility"`
		Destructive      bool            `json:"destructive"`
		BlastRows        int64           `json:"blast_rows"`
		PreconditionHash string          `json:"precondition_hash"`
		AdapterData      json.RawMessage `json:"adapter_data"`
	}{p.Adapter, p.Target, p.Env, p.Operation, p.Effects, p.Reversibility, p.Destructive, p.BlastRows, p.PreconditionHash, p.AdapterData}
	// encoding/json sorts map keys, so this is canonical for our types.
	b, _ := json.Marshal(h)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// NewID returns a short random identifier with the given prefix, e.g. "p_3f9a1c2b7d".
func NewID(prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}
