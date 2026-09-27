package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSecretBoxPersistsPrivateMasterKeyAndAuthenticatesCiphertext(t *testing.T) {
	dir := t.TempDir()
	box, err := openSecretBox(dir)
	if err != nil {
		t.Fatalf("openSecretBox(): %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatalf("stat master key: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("master key permissions = %04o, want 0600", info.Mode().Perm())
	}
	ciphertext, err := box.encrypt([]byte("desktop-password"))
	if err != nil {
		t.Fatalf("encrypt(): %v", err)
	}
	if string(ciphertext) == "desktop-password" {
		t.Fatal("encrypt() returned plaintext")
	}
	reopened, err := openSecretBox(dir)
	if err != nil {
		t.Fatalf("reopenSecretBox(): %v", err)
	}
	plaintext, err := reopened.decrypt(ciphertext)
	if err != nil || string(plaintext) != "desktop-password" {
		t.Fatalf("decrypt() = %q, %v", plaintext, err)
	}
	if _, err := reopened.decrypt(ciphertext[:len(ciphertext)-1]); err == nil {
		t.Fatal("decrypt() accepted tampered ciphertext")
	}
}
