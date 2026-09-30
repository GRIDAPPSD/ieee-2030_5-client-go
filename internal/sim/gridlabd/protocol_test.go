package gridlabd

import (
	"encoding/json"
	"testing"
)

func TestValue_Float64_Real(t *testing.T) {
	var v Value
	if err := json.Unmarshal([]byte(`1234.5`), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := v.Float64()
	if err != nil {
		t.Fatalf("Float64: %v", err)
	}
	if got != 1234.5 {
		t.Errorf("Float64() = %v, want 1234.5", got)
	}
}

func TestValue_Float64_ComplexZeroImaginary(t *testing.T) {
	var v Value
	if err := json.Unmarshal([]byte(`{"re":42.0,"im":0}`), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := v.Float64()
	if err != nil {
		t.Fatalf("Float64: %v", err)
	}
	if got != 42.0 {
		t.Errorf("Float64() = %v, want 42.0", got)
	}
}

// TestValue_Float64_ComplexNonzeroImaginary proves the check can fail: a
// complex value with real work to lose (a nonzero imaginary part) is
// refused, not silently truncated to its real part.
func TestValue_Float64_ComplexNonzeroImaginary(t *testing.T) {
	var v Value
	if err := json.Unmarshal([]byte(`{"re":1.0,"im":2.0}`), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, err := v.Float64(); err == nil {
		t.Fatal("Float64() on a complex value with nonzero imaginary part: want error, got nil")
	}
}

func TestValue_Float64_Garbage(t *testing.T) {
	var v Value
	if err := json.Unmarshal([]byte(`"not a number"`), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, err := v.Float64(); err == nil {
		t.Fatal("Float64() on a string value: want error, got nil")
	}
}

func TestFloatValue_RoundTrips(t *testing.T) {
	v := FloatValue(-10.5)
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != "-10.5" {
		t.Errorf("marshal(FloatValue(-10.5)) = %s, want -10.5", b)
	}
	got, err := v.Float64()
	if err != nil {
		t.Fatalf("Float64: %v", err)
	}
	if got != -10.5 {
		t.Errorf("Float64() = %v, want -10.5", got)
	}
}
