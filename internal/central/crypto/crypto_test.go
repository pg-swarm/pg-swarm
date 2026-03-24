package crypto

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const testKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestNewEncryptor_Valid(t *testing.T) {
	enc, err := NewEncryptor(testKey)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if enc == nil {
		t.Fatal("expected non-nil encryptor")
	}
}

func TestNewEncryptor_InvalidHex(t *testing.T) {
	_, err := NewEncryptor("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz")
	if err == nil {
		t.Fatal("expected error for invalid hex")
	}
}

func TestNewEncryptor_WrongLength(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"short", "0123456789abcdef0123456789abcdef0123456789abcdef"},
		{"long", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewEncryptor(tt.key)
			if err == nil {
				t.Fatalf("expected error for key length %d chars", len(tt.key))
			}
		})
	}
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	enc, err := NewEncryptor(testKey)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte(`{"access_key":"AKIAIOSFODNN7EXAMPLE","secret_key":"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}`)

	ciphertext, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	got, err := enc.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round-trip mismatch: got %q, want %q", got, plaintext)
	}
}

func TestEncryptDecrypt_EmptyPlaintext(t *testing.T) {
	enc, err := NewEncryptor(testKey)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := enc.Encrypt([]byte{})
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// 12-byte nonce + 16-byte tag = 28 bytes minimum
	if len(ciphertext) != 28 {
		t.Fatalf("expected 28 bytes for empty plaintext, got %d", len(ciphertext))
	}
	got, err := enc.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty plaintext, got %d bytes", len(got))
	}
}

func TestEncrypt_DifferentNonces(t *testing.T) {
	enc, err := NewEncryptor(testKey)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("same plaintext")

	ct1, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	ct2, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ct1, ct2) {
		t.Fatal("two encryptions of same plaintext should produce different ciphertexts")
	}
}

func TestDecrypt_WrongKey(t *testing.T) {
	encA, err := NewEncryptor(testKey)
	if err != nil {
		t.Fatal(err)
	}
	encB, err := NewEncryptor("abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd")
	if err != nil {
		t.Fatal(err)
	}

	ciphertext, err := encA.Encrypt([]byte("secret data"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = encB.Decrypt(ciphertext)
	if err == nil {
		t.Fatal("expected error decrypting with wrong key")
	}
}

func TestDecrypt_TamperedCiphertext(t *testing.T) {
	enc, err := NewEncryptor(testKey)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := enc.Encrypt([]byte("important data"))
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte in the middle of the ciphertext (after the nonce)
	ciphertext[15] ^= 0xff

	_, err = enc.Decrypt(ciphertext)
	if err == nil {
		t.Fatal("expected error for tampered ciphertext")
	}
}

func TestDecrypt_TruncatedCiphertext(t *testing.T) {
	enc, err := NewEncryptor(testKey)
	if err != nil {
		t.Fatal(err)
	}
	_, err = enc.Decrypt([]byte{1, 2, 3, 4, 5})
	if err == nil {
		t.Fatal("expected error for truncated ciphertext")
	}
	if !strings.Contains(err.Error(), "ciphertext too short") {
		t.Fatalf("expected 'ciphertext too short' error, got: %v", err)
	}
}

func TestDecrypt_LargePayload(t *testing.T) {
	enc, err := NewEncryptor(testKey)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a ~10KB GCS service account JSON
	svcAccount := map[string]interface{}{
		"type":                        "service_account",
		"project_id":                  "my-project-123456",
		"private_key_id":              "key123456789",
		"private_key":                 strings.Repeat("A", 8000),
		"client_email":                "sa@my-project-123456.iam.gserviceaccount.com",
		"client_id":                   "123456789012345678901",
		"auth_uri":                    "https://accounts.google.com/o/oauth2/auth",
		"token_uri":                   "https://oauth2.googleapis.com/token",
		"auth_provider_x509_cert_url": "https://www.googleapis.com/oauth2/v1/certs",
	}
	plaintext, err := json.Marshal(svcAccount)
	if err != nil {
		t.Fatal(err)
	}
	if len(plaintext) < 8000 {
		t.Fatalf("expected large payload, got %d bytes", len(plaintext))
	}

	ciphertext, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	got, err := enc.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatal("large payload round-trip mismatch")
	}
}
