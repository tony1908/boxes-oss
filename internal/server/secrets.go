package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type secretBox struct{ aead cipher.AEAD }

func openSecretBox(dataDir string) (*secretBox, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	keyPath := filepath.Join(dataDir, "master.key")
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, fmt.Errorf("generate master key: %w", err)
		}
		file, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			key, err = os.ReadFile(keyPath)
		} else if err != nil {
			return nil, fmt.Errorf("create master key: %w", err)
		} else {
			if _, writeErr := file.Write(key); writeErr != nil {
				_ = file.Close()
				return nil, fmt.Errorf("write master key: %w", writeErr)
			}
			if err := file.Close(); err != nil {
				return nil, fmt.Errorf("close master key: %w", err)
			}
		}
	} else if err != nil {
		return nil, fmt.Errorf("read master key: %w", err)
	}
	if len(key) != 32 {
		return nil, errors.New("master key must contain exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create master cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create master cipher: %w", err)
	}
	return &secretBox{aead: aead}, nil
}

func (b *secretBox) encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate encryption nonce: %w", err)
	}
	return b.aead.Seal(nonce, nonce, plaintext, nil), nil
}

func (b *secretBox) decrypt(ciphertext []byte) ([]byte, error) {
	nonceSize := b.aead.NonceSize()
	if len(ciphertext) < nonceSize+b.aead.Overhead() {
		return nil, errors.New("ciphertext is too short")
	}
	plaintext, err := b.aead.Open(nil, ciphertext[:nonceSize], ciphertext[nonceSize:], nil)
	if err != nil {
		return nil, errors.New("decrypt secret: authentication failed")
	}
	return plaintext, nil
}
