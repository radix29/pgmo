package pgmo

import (
	"errors"
	"testing"
)

// A create's read-back that finds the name overloaded returns the handle:
// the create succeeded, and which overload is new is unknown. Any other
// error, and a found object, pass through.
func TestUnlessAmbiguous(t *testing.T) {
	handle, found := &Function{}, &Function{}
	if got, err := unlessAmbiguous(handle)(nil, ambiguousf("two")); got != handle || err != nil {
		t.Errorf("ambiguous: %v, %v; want the handle", got, err)
	}
	other := invalidf("x")
	if got, err := unlessAmbiguous(handle)(nil, other); got != nil || !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("another error: %v, %v", got, err)
	}
	if got, err := unlessAmbiguous(handle)(found, nil); got != found || err != nil {
		t.Errorf("found: %v, %v", got, err)
	}
}
