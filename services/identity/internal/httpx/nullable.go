package httpx

import (
	"encoding/json"
	"time"
)

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

// NullableTime is Nullable for an RFC 3339 time.
type NullableTime struct {
	Set   bool
	Value *time.Time
}

func (n *NullableTime) UnmarshalJSON(data []byte) error {
	n.Set = true
	if string(data) == "null" {
		n.Value = nil
		return nil
	}
	var t time.Time
	if err := json.Unmarshal(data, &t); err != nil {
		return err
	}
	n.Value = &t
	return nil
}

// NullableInt is Nullable for an integer.
type NullableInt struct {
	Set   bool
	Value *int
}

func (n *NullableInt) UnmarshalJSON(data []byte) error {
	n.Set = true
	if string(data) == "null" {
		n.Value = nil
		return nil
	}
	var v int
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	n.Value = &v
	return nil
}
