// Package google serves Google Analytics (GA4 Data API) and Search Console reports with a service
// account (moved from zengine's stat tasks).
package google

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type serviceAccount struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

func parseServiceAccount(raw string) (*serviceAccount, *rsa.PrivateKey, error) {
	var sa serviceAccount
	if err := json.Unmarshal([]byte(raw), &sa); err != nil {
		return nil, nil, fmt.Errorf("service account JSON: %w", err)
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, nil, fmt.Errorf("service account missing client_email/private_key")
	}
	if sa.TokenURI == "" {
		sa.TokenURI = "https://oauth2.googleapis.com/token"
	}
	key, err := parseRSAPrivateKey(sa.PrivateKey)
	if err != nil {
		return nil, nil, err
	}
	return &sa, key, nil
}

func parseRSAPrivateKey(key string) (*rsa.PrivateKey, error) {
	key = strings.ReplaceAll(key, `\n`, "\n")
	block, _ := pem.Decode([]byte(key))
	if block == nil {
		return nil, fmt.Errorf("invalid private key pem")
	}
	if pkcs8, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := pkcs8.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private key is not RSA")
		}
		return rsaKey, nil
	}
	if pkcs1, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return pkcs1, nil
	}
	return nil, fmt.Errorf("unsupported private key format")
}

// tokenSource gets OAuth access tokens with a signed JWT and reuses each until shortly before it expires.
type tokenSource struct {
	sa     *serviceAccount
	key    *rsa.PrivateKey
	client *http.Client

	mu     sync.Mutex
	cached map[string]cachedToken
}

type cachedToken struct {
	value   string
	expires time.Time
}

func (s *tokenSource) token(ctx context.Context, scope string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.cached[scope]; ok && time.Now().Before(t.expires) {
		return t.value, nil
	}

	now := time.Now()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	input := enc(map[string]any{"alg": "RS256", "typ": "JWT"}) + "." + enc(map[string]any{
		"iss": s.sa.ClientEmail, "scope": scope, "aud": s.sa.TokenURI,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {input + "." + base64.RawURLEncoding.EncodeToString(sig)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.sa.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("token request failed: %d %s", resp.StatusCode, string(raw))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("missing access_token")
	}
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 3600
	}
	if s.cached == nil {
		s.cached = map[string]cachedToken{}
	}
	s.cached[scope] = cachedToken{value: out.AccessToken, expires: now.Add(time.Duration(out.ExpiresIn)*time.Second - 5*time.Minute)}
	return out.AccessToken, nil
}
