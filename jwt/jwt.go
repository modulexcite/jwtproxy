// Copyright 2015 CoreOS, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package jwt

import (
	"errors"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/jose"
	"github.com/coreos/go-oidc/key"
	"github.com/coreos/go-oidc/oidc"

	"github.com/coreos-inc/jwtproxy/jwt/keyserver"
	"github.com/coreos-inc/jwtproxy/jwt/noncestorage"
)

const (
	nonceBytes   = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+/"
	nonceIdxBits = 6                   // 6 bits to represent a nonce index
	nonceIdxMask = 1<<nonceIdxBits - 1 // All 1-bits, as many as nonceIdxBits
	nonceIdxMax  = 63 / nonceIdxBits   // # of nonce indices fitting in 63 bits
)

var randSource rand.Source

func init() {
	randSource = rand.NewSource(time.Now().UnixNano())
}

func Sign(req *http.Request, issuer string, key *key.PrivateKey, nonceLength int, expirationTime, maxSkew time.Duration) error {
	// Create Claims.
	claims := jose.Claims{
		"kid": key.ID(),
		"iss": issuer,
		"aud": req.URL.String(),
		"iat": time.Now().Unix(),
		"nbf": time.Now().Add(-maxSkew).Unix(),
		"exp": time.Now().Add(expirationTime).Unix(),
		"jti": generateNonce(nonceLength),
	}

	// Create JWT.
	jwt, err := jose.NewSignedJWT(claims, key.Signer())
	if err != nil {
		return err
	}

	// Add it as a header in the request.
	req.Header.Add("Authorization", "Bearer "+jwt.Encode())

	return nil
}

func Verify(req *http.Request, keyServer keyserver.Reader, nonceVerifier noncestorage.NonceStorage, audience *url.URL, maxTTL time.Duration) error {
	// Extract token from request.
	token, err := oidc.ExtractBearerToken(req)
	if err != nil {
		return errors.New("no JWT found")
	}

	// Parse token.
	jwt, err := jose.ParseJWT(token)
	if err != nil {
		return errors.New("could not parse JWT")
	}

	claims, err := jwt.Claims()
	if err != nil {
		return errors.New("could not parse JWT claims")
	}

	// Verify claims.
	now := time.Now().UTC()

	kid, exists, err := claims.StringClaim("kid")
	if !exists || err != nil {
		return errors.New("missing or invalid 'kid' claim")
	}
	iss, exists, err := claims.StringClaim("iss")
	if !exists || err != nil {
		return errors.New("missing or invalid 'iss' claim")
	}
	aud, exists, err := claims.StringClaim("aud")
	if !exists || err != nil || !verifyAudience(aud, audience) {
		return errors.New("missing or invalid 'aud' claim")
	}
	exp, exists, err := claims.TimeClaim("exp")
	if !exists || err != nil || exp.Before(now) {
		return errors.New("missing or invalid 'exp' claim")
	}
	nbf, exists, err := claims.TimeClaim("nbf")
	if !exists || err != nil || nbf.After(now) {
		return errors.New("missing or invalid 'nbf' claim")
	}
	iat, exists, err := claims.TimeClaim("iat")
	if !exists || err != nil {
		return errors.New("missing or invalid 'iat' claim")
	}
	if exp.Sub(iat) > maxTTL {
		return errors.New("'exp' is too far in the future")
	}
	jti, exists, err := claims.StringClaim("jti")
	if !exists || err != nil || !nonceVerifier.Verify(jti, exp) {
		return errors.New("missing or invalid 'jti' claim")
	}

	// Verify signature.
	publicKey, err := keyServer.GetPublicKey(iss, kid)
	if err != nil {
		return err
	}
	verifier, err := publicKey.Verifier()
	if err != nil {
		return err
	}
	if verifier.Verify(jwt.Signature, []byte(jwt.Data())) != nil {
		return errors.New("invalid JWT signature")
	}

	return nil
}

func verifyAudience(actual string, expected *url.URL) bool {
	actualURL, err := url.Parse(actual)
	if err != nil {
		return false
	}
	return strings.EqualFold(actualURL.Scheme+actualURL.Host, expected.Scheme+expected.Host)
}

// https://stackoverflow.com/questions/22892120/how-to-generate-a-random-string-of-a-fixed-length-in-golang
func generateNonce(n int) string {
	b := make([]byte, n)
	for i, cache, remain := n-1, randSource.Int63(), nonceIdxMax; i >= 0; {
		if remain == 0 {
			cache, remain = randSource.Int63(), nonceIdxMax
		}
		if idx := int(cache & nonceIdxMask); idx < len(nonceBytes) {
			b[i] = nonceBytes[idx]
			i--
		}
		cache >>= nonceIdxBits
		remain--
	}
	return string(b)
}