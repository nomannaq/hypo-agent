package memory

import (
	"fmt"
	"slices"
)

type Kind uint8

const (
	invalidKind Kind = iota
	Preference
	Note
	RunSummary
	numKinds
)

var kindNames = [...]string{Preference: "preference", Note: "note", RunSummary: "run_summary"}

var _ [numKinds]string = kindNames

func (k Kind) Valid() bool { return k > invalidKind && k < numKinds }

func (k Kind) String() string {
	if k.Valid() {
		return kindNames[k]
	}
	return fmt.Sprintf("Kind(%d)", k)
}

func (k Kind) MarshalText() ([]byte, error) {
	if !k.Valid() {
		return nil, fmt.Errorf("memory: invalid %s", k)
	}
	return []byte(k.String()), nil
}

func (k *Kind) UnmarshalText(b []byte) error {
	parsed := Kind(slices.Index(kindNames[:], string(b)))
	if !parsed.Valid() {
		return fmt.Errorf("memory: unknown kind %q", b)
	}
	*k = parsed
	return nil
}
