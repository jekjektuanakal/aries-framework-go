/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ed25519signature2018

import (
	"github.com/hyperledger/aries-framework-go/pkg/crypto"
	sigverifier "github.com/hyperledger/aries-framework-go/pkg/doc/signature/verifier"
)

// CryptoSigner defines signer based on crypto.
type CryptoSigner struct {
	cr crypto.Crypto
	kh interface{}
}

// NewCryptoSigner creates a new CryptoSigner.
func NewCryptoSigner(cr crypto.Crypto, kh interface{}) *CryptoSigner {
	return &CryptoSigner{
		cr: cr,
		kh: kh,
	}
}

// Sign will sign document and return signature.
func (s *CryptoSigner) Sign(msg []byte) ([]byte, error) {
	return s.cr.Sign(msg, s.kh)
}

// CryptoVerifier defines signature verifier based on crypto.
type CryptoVerifier struct {
	cr crypto.Crypto
}

// NewCryptoVerifier creates a new CryptoVerifier.
func NewCryptoVerifier(cr crypto.Crypto) *CryptoVerifier {
	return &CryptoVerifier{
		cr: cr,
	}
}

// Verify will verify a signature.
func (v *CryptoVerifier) Verify(kh *sigverifier.PublicKey, msg, signature []byte) error {
	return v.cr.Verify(signature, msg, kh)
}
