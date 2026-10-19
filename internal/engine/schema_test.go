package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// Change records produced by the engine must validate against the published
// JSON Schema (spec/schemas/change-record.schema.json).
func TestChangeRecordMatchesSchema(t *testing.T) {
	b, err := os.ReadFile("../../spec/schemas/change-record.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	rs, err := s.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	f := setup(t)
	c := f.propose(t, "personal", "set a 1", "")
	c, err = f.eng.Apply(context.Background(), c.ID, f.target("personal"), "test")
	if err != nil {
		t.Fatal(err)
	}
	c, err = f.eng.Undo(context.Background(), c.ID, f.target("personal"), "test")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(c)
	var inst map[string]any
	_ = json.Unmarshal(raw, &inst)
	if err := rs.Validate(inst); err != nil {
		t.Fatalf("change record does not match schema: %v\n%s", err, raw)
	}
	inst["state"] = "teleported"
	if err := rs.Validate(inst); err == nil {
		t.Fatal("schema accepted an invalid state")
	}
}
