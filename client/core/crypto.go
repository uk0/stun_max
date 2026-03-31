package core

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
)

// PeerCrypto holds the encryption state for a P2P peer connection.
type PeerCrypto struct {
	PrivKey   *ecdh.PrivateKey
	PubKey    []byte // our public key (raw bytes)
	SharedKey []byte // derived AES-256 key
	AEAD      cipher.AEAD
	Encrypted bool

	// Counter-based nonce (no rand.Read per packet)
	nonceCounter uint64
	nonceMu      sync.Mutex
}

// NewPeerCrypto generates a new X25519 key pair.
func NewPeerCrypto() (*PeerCrypto, error) {
	curve := ecdh.X25519()
	priv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate X25519 key: %w", err)
	}
	// Random starting counter to avoid nonce reuse across sessions
	var startCounter uint64
	b := make([]byte, 8)
	rand.Read(b)
	startCounter = binary.LittleEndian.Uint64(b)

	return &PeerCrypto{
		PrivKey:      priv,
		PubKey:       priv.PublicKey().Bytes(),
		nonceCounter: startCounter,
	}, nil
}

// DeriveKey performs ECDH with the peer's public key and derives AES-256-GCM.
func (pc *PeerCrypto) DeriveKey(peerPubKeyBytes []byte) error {
	curve := ecdh.X25519()
	peerPub, err := curve.NewPublicKey(peerPubKeyBytes)
	if err != nil {
		return fmt.Errorf("parse peer public key: %w", err)
	}

	shared, err := pc.PrivKey.ECDH(peerPub)
	if err != nil {
		return fmt.Errorf("ECDH: %w", err)
	}

	hash := sha256.Sum256(shared)
	pc.SharedKey = hash[:]

	block, err := aes.NewCipher(pc.SharedKey)
	if err != nil {
		return fmt.Errorf("AES cipher: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("GCM: %w", err)
	}

	pc.AEAD = aead
	pc.Encrypted = true
	return nil
}

// nextNonce returns a unique nonce using an atomic counter.
// 12-byte nonce: [4-byte zero padding][8-byte counter]
// No syscall, no allocation on the nonce itself.
func (pc *PeerCrypto) nextNonce() []byte {
	n := atomic.AddUint64(&pc.nonceCounter, 1)
	nonce := make([]byte, 12)
	binary.LittleEndian.PutUint64(nonce[4:], n)
	return nonce
}

// EncryptTo encrypts plaintext into dst buffer, returning the slice.
// Format: [12-byte nonce][ciphertext+16-byte tag]
// Overhead: 28 bytes per packet.
func (pc *PeerCrypto) EncryptTo(dst, plaintext []byte) []byte {
	if !pc.Encrypted || pc.AEAD == nil {
		return plaintext
	}
	nonce := pc.nextNonce()
	// dst must have room for nonce + plaintext + tag
	copy(dst, nonce)
	sealed := pc.AEAD.Seal(dst[:12], nonce, plaintext, nil)
	return sealed
}

// Encrypt encrypts plaintext using AES-256-GCM with counter nonce.
func (pc *PeerCrypto) Encrypt(plaintext []byte) ([]byte, error) {
	if !pc.Encrypted || pc.AEAD == nil {
		return plaintext, nil
	}
	nonce := pc.nextNonce()
	ciphertext := pc.AEAD.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

// Decrypt decrypts ciphertext using AES-256-GCM.
func (pc *PeerCrypto) Decrypt(data []byte) ([]byte, error) {
	if !pc.Encrypted || pc.AEAD == nil {
		return data, nil
	}
	nonceSize := pc.AEAD.NonceSize()
	if len(data) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce := data[:nonceSize]
	ciphertext := data[nonceSize:]
	plaintext, err := pc.AEAD.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plaintext, nil
}
