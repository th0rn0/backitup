package model

import "testing"

func TestModeValid(t *testing.T) {
	valid := []Mode{ModeTarGz, ModeRsync}
	for _, m := range valid {
		if !m.Valid() {
			t.Errorf("%q should be valid", m)
		}
	}
	invalid := []Mode{"", "zip", "rsyncc", "TARGZ"}
	for _, m := range invalid {
		if m.Valid() {
			t.Errorf("%q should be invalid", m)
		}
	}
}
