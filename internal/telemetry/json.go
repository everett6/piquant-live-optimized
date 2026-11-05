package telemetry

import (
	"bytes"
	"encoding/json"
	"math"
)

// Float renders a float for JSON, mapping a non-finite value to null.
//
// encoding/json refuses NaN and ±Inf outright, and because the failure happens
// while encoding a struct it fails the *whole* document: one missing sensor
// would blank the entire dashboard status instead of showing "n/a" for that
// one field. Every sampled float is therefore nullable at the JSON boundary,
// which is also what the browser's chart code expects (a gap, not a zero).
type Float float64

// MarshalJSON implements json.Marshaler.
func (f Float) MarshalJSON() ([]byte, error) {
	v := float64(f)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return []byte("null"), nil
	}
	return json.Marshal(v)
}

// UnmarshalJSON implements json.Unmarshaler, so a round trip through the wire
// keeps an unavailable reading unavailable.
func (f *Float) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		*f = Float(math.NaN())
		return nil
	}
	var v float64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*f = Float(v)
	return nil
}

// Value returns the reading, or NaN when it was unavailable.
func (f Float) Value() float64 { return float64(f) }
