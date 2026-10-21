package icloud

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestAuthenticateWithoutSessionRequiresLogin(t *testing.T) {
	sessionPath := filepath.Join(t.TempDir(), "missing-session.json")

	cl, err := New(sessionPath)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	err = cl.Authenticate(context.Background())
	if !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("Authenticate() error = %v, want ErrLoginRequired", err)
	}
}
