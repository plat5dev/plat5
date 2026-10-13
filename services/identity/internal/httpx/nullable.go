package httpx

import "encoding/json"

// Nullable is a body field where null means something: Set is false when the
// field is omitted, and Value is nil when it is null.
type Nullable struct {
	Set   bool
	Value *string
}

func (n *Nullable) UnmarshalJSON(data []byte) error {
	n.Set = true
	if string(data) == "null" {
		n.Value = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	n.Value = &s
	return nil
}
