package gridlabd

import (
	"encoding/json"
	"math"
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

// TestValue_Float64_Null is the classic Go gotcha this fixes: unmarshaling
// JSON null into a *float64 destination succeeds and leaves it at its zero
// value, so an unwary Float64() would read a malformed or absent reply as
// a silent, plausible-looking 0.
func TestValue_Float64_Null(t *testing.T) {
	var v Value
	if err := json.Unmarshal([]byte(`null`), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, err := v.Float64(); err == nil {
		t.Fatal("Float64() on null: want error, got nil")
	}
}

// TestValue_Float64_EmptyObject proves {} (decodes into the complex
// struct as zero re/im with no error) is refused too, the same failure
// shape as null but via the object branch instead of the scalar one.
func TestValue_Float64_EmptyObject(t *testing.T) {
	var v Value
	if err := json.Unmarshal([]byte(`{}`), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, err := v.Float64(); err == nil {
		t.Fatal("Float64() on {}: want error, got nil")
	}
}

func TestValue_Magnitude_Real(t *testing.T) {
	var v Value
	if err := json.Unmarshal([]byte(`-120.0`), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := v.Magnitude()
	if err != nil {
		t.Fatalf("Magnitude: %v", err)
	}
	if got != 120.0 {
		t.Errorf("Magnitude() = %v, want 120.0", got)
	}
}

func TestValue_Magnitude_Complex(t *testing.T) {
	var v Value
	if err := json.Unmarshal([]byte(`{"re":90,"im":90}`), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := v.Magnitude()
	if err != nil {
		t.Fatalf("Magnitude: %v", err)
	}
	want := 127.27922061357855 // 90*sqrt(2)
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("Magnitude() = %v, want %v", got, want)
	}
}

func TestValue_Magnitude_Null(t *testing.T) {
	var v Value
	if err := json.Unmarshal([]byte(`null`), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, err := v.Magnitude(); err == nil {
		t.Fatal("Magnitude() on null: want error, got nil")
	}
}

func TestFloatValue_RoundTrips(t *testing.T) {
	v, err := FloatValue(-10.5)
	if err != nil {
		t.Fatalf("FloatValue: %v", err)
	}
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

// TestFloatValue_NaNReturnsErrorNotPanic proves a NaN setpoint (e.g. from
// a division by zero headroom in a caller's control-loop math) is refused
// with an error, not a panic that would crash the whole aggregator process
// over one bad calculation.
func TestFloatValue_NaNReturnsErrorNotPanic(t *testing.T) {
	if _, err := FloatValue(math.NaN()); err == nil {
		t.Fatal("FloatValue(NaN): want error, got nil")
	}
	if _, err := FloatValue(math.Inf(1)); err == nil {
		t.Fatal("FloatValue(+Inf): want error, got nil")
	}
}
