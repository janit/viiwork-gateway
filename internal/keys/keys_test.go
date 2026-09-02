package keys

import (
	"strings"
	"testing"
)

func TestLoadParsesLabels(t *testing.T) {
	set, err := Load([]string{
		"PATH=/usr/bin",
		"VIIWORK_KEY_janit=aaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"VIIWORK_KEY_laptop=bbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if set.Len() != 2 {
		t.Fatalf("Len = %d, want 2", set.Len())
	}
	label, ok := set.Lookup("aaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if !ok || label != "janit" {
		t.Errorf("Lookup = %q, %v; want janit, true", label, ok)
	}
	if _, ok := set.Lookup("not-a-key-not-a-key-not-a-key"); ok {
		t.Error("Lookup accepted an unknown secret")
	}
}

func TestSecretByLabel(t *testing.T) {
	set, err := Load([]string{"VIIWORK_KEY_janit=aaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	secret, ok := set.Secret("janit")
	if !ok || secret != "aaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("Secret = %q, %v", secret, ok)
	}
	if _, ok := set.Secret("nobody"); ok {
		t.Error("Secret returned a value for an unknown label")
	}
}

func TestLoadRejectsEmptySet(t *testing.T) {
	_, err := Load([]string{"PATH=/usr/bin"})
	if err == nil {
		t.Fatal("want error when no keys are configured")
	}
	if !strings.Contains(err.Error(), "VIIWORK_KEY_") {
		t.Errorf("error should name the variable prefix, got: %v", err)
	}
}

func TestLoadRejectsShortKey(t *testing.T) {
	_, err := Load([]string{"VIIWORK_KEY_weak=tooshort"})
	if err == nil {
		t.Fatal("want error for a key under the minimum length")
	}
	if !strings.Contains(err.Error(), "weak") {
		t.Errorf("error should name the offending label, got: %v", err)
	}
}

func TestLoadRejectsEmptyLabel(t *testing.T) {
	if _, err := Load([]string{"VIIWORK_KEY_=aaaaaaaaaaaaaaaaaaaaaaaaaaaa"}); err == nil {
		t.Fatal("want error for an empty label")
	}
}

func TestLoadRejectsDuplicateSecret(t *testing.T) {
	_, err := Load([]string{
		"VIIWORK_KEY_one=aaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"VIIWORK_KEY_two=aaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	})
	if err == nil {
		t.Fatal("want error when two labels share a secret: revoking one would not revoke the other")
	}
}
