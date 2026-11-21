package tool

import (
	"fmt"
	"slices"
)

// Effect is what calling a tool can do to the world. It decides the
// executor's policy: a Write call waits for the user's approval and goes
// through the idempotency ledger, so a retried activity doesn't repeat it.
// A closed set, like agent.Status: the constants, one name each, and text
// encoding that rejects anything else.
type Effect uint8

const (
	// Read only observes (search, read a page); repeating it is harmless.
	Read Effect = iota
	// Write changes something outside the run (submits a form, sends a
	// message). It needs approval and must not repeat.
	Write

	numEffects // not an effect: the count, for the array below
)

var effectNames = [...]string{
	Read:  "read",
	Write: "write",
}

// Compile-time check: adding an effect without a name changes the array length.
var _ [numEffects]string = effectNames

func (e Effect) Valid() bool { return e < numEffects }

func (e Effect) String() string {
	if !e.Valid() {
		return fmt.Sprintf("Effect(%d)", e)
	}
	return effectNames[e]
}

func (e Effect) MarshalText() ([]byte, error) {
	if !e.Valid() {
		return nil, fmt.Errorf("tool: invalid effect %d", e)
	}
	return []byte(effectNames[e]), nil
}

func (e *Effect) UnmarshalText(b []byte) error {
	i := slices.Index(effectNames[:], string(b))
	if i < 0 {
		return fmt.Errorf("tool: unknown effect %q", b)
	}
	*e = Effect(i)
	return nil
}
