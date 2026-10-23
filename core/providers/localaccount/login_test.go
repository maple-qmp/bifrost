package localaccount

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

func identityClient(t *testing.T, claims jwt.MapClaims) (*http.Client, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "key-1"
	signed, err := token.SignedString(key)
	require.NoError(t, err)
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			return jsonResponse(`{"issuer":"https://auth.openai.com","jwks_uri":"https://auth.openai.com/jwks"}`), nil
		case "/jwks":
			return jsonResponse(fmt.Sprintf(`{"keys":[{"kid":"key-1","kty":"RSA","n":"%s","e":"%s"}]}`, base64.RawURLEncoding.EncodeToString(key.N.Bytes()), base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()))), nil
		case "/api/accounts/oauth/token":
			return jsonResponse(fmt.Sprintf(`{"access_token":"access","refresh_token":"refresh","id_token":%q,"scope":"openid chatgpt.tokens.use.direct","expires_in":3600}`, signed)), nil
		default:
			t.Errorf("unexpected auth request %s", r.URL)
			return jsonResponse(`{}`), nil
		}
	})}
	return hc, signed
}

func TestLocalAccountIDTokenValidation(t *testing.T) {
	valid := jwt.MapClaims{"iss": authOrigin, "sub": "subject", "aud": "issued-client", "exp": time.Now().Add(time.Hour).Unix(), "nonce": "nonce"}
	for _, tc := range []struct {
		name  string
		key   string
		value any
	}{{"valid", "", nil}, {"issuer", "iss", "wrong"}, {"audience", "aud", "wrong"}, {"nonce", "nonce", "wrong"}, {"expired", "exp", time.Now().Add(-time.Hour).Unix()}, {"subject", "sub", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			claims := jwt.MapClaims{}
			for k, v := range valid {
				claims[k] = v
			}
			if tc.key != "" {
				claims[tc.key] = tc.value
			}
			hc, signed := identityClient(t, claims)
			m := &Manager{http: hc}
			id, err := m.verifyID(context.Background(), signed, "issued-client", "nonce")
			if tc.name == "valid" {
				require.NoError(t, err)
				require.NotEmpty(t, id)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestLocalAccountManualLogin(t *testing.T) {
	m, err := newManager(schemas.ChatGPT, schemas.LocalAccountConfig{DataDir: t.TempDir()}, nil)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	m.login = &loginSession{id: "session", state: "state", nonce: "nonce", verifier: "verifier", redirect: "http://127.0.0.1:1455/auth/callback", stateName: "pending", cancel: cancel, context: ctx}
	m.http, _ = identityClient(t, jwt.MapClaims{"iss": authOrigin, "sub": "subject", "aud": "issued-client", "exp": time.Now().Add(time.Hour).Unix(), "nonce": "nonce"})
	require.ErrorContains(t, m.CompleteLogin("session", "http://127.0.0.1:1455/auth/callback?code=code&state=wrong&client_id=issued-client"), "state mismatch")
	require.ErrorContains(t, m.CompleteLogin("session", "http://evil.test/auth/callback?code=code&state=state&client_id=issued-client"), "does not match")
	require.NoError(t, m.CompleteLogin("session", "http://127.0.0.1:1455/auth/callback?code=code&state=state&client_id=issued-client"))
	require.Equal(t, "completed", m.Status().LoginState)
	require.True(t, m.Status().Connected)
	require.Error(t, m.CompleteLogin("session", "http://127.0.0.1:1455/auth/callback?code=code&state=state&client_id=issued-client"))
	var saved credential
	require.NoError(t, readObject(m.path, &saved))
	require.Equal(t, "issued-client", saved.ClientID)
	info, err := os.Stat(m.root)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0700), info.Mode().Perm())
	// A new manager after restart sees metadata and refreshes from its own file.
	restored, err := newManager(schemas.ChatGPT, schemas.LocalAccountConfig{DataDir: filepath.Dir(m.root)}, m.http)
	require.NoError(t, err)
	require.True(t, restored.Status().Connected)
}

func TestLocalAccountReturningCallbackClientID(t *testing.T) {
	m, err := newManager(schemas.ChatGPT, schemas.LocalAccountConfig{DataDir: t.TempDir()}, nil)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	m.login = &loginSession{id: "session", state: "state", nonce: "nonce", verifier: "verifier", redirect: "http://127.0.0.1:1455/auth/callback", stateName: "pending", clientID: "issued-client", cancel: cancel, context: ctx}
	m.http, _ = identityClient(t, jwt.MapClaims{"iss": authOrigin, "sub": "subject", "aud": "issued-client", "exp": time.Now().Add(time.Hour).Unix(), "nonce": "nonce"})
	require.ErrorContains(t, m.CompleteLogin("session", "http://127.0.0.1:1455/auth/callback?code=code&state=state&client_id=other"), "does not match")
	require.NoError(t, m.CompleteLogin("session", "http://127.0.0.1:1455/auth/callback?code=code&state=state"))
}

func TestLocalAccountLoginListenerLifecycle(t *testing.T) {
	m, err := newManager(schemas.ChatGPT, schemas.LocalAccountConfig{DataDir: t.TempDir()}, nil)
	require.NoError(t, err)
	start, err := m.StartLogin()
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Disconnect() })
	u, err := url.Parse(start.URL)
	require.NoError(t, err)
	require.Equal(t, "dynamic_agent_client", u.Query().Get("client_id"))
	require.Equal(t, "Bifrost", u.Query().Get("agent_name_hint"))
	require.NotEmpty(t, u.Query().Get("code_challenge"))
	require.Equal(t, "S256", u.Query().Get("code_challenge_method"))
	_, err = m.StartLogin()
	require.ErrorContains(t, err, "already pending")
	other, err := newManager(schemas.ChatGPT, schemas.LocalAccountConfig{DataDir: t.TempDir()}, nil)
	require.NoError(t, err)
	_, err = other.StartLogin()
	require.ErrorContains(t, err, "cannot listen")
	require.NoError(t, m.Disconnect())
	require.Eventually(t, func() bool {
		l, err := net.Listen("tcp4", "127.0.0.1:1455")
		if err != nil {
			return false
		}
		l.Close()
		return true
	}, time.Second, 10*time.Millisecond)
}

func TestLocalAccountRejectsForgedSignature(t *testing.T) {
	hc, signed := identityClient(t, jwt.MapClaims{"iss": authOrigin, "sub": "subject", "aud": "client", "exp": time.Now().Add(time.Hour).Unix(), "nonce": "nonce"})
	parts := strings.Split(signed, ".")
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(t, err)
	signature[0] ^= 1
	parts[2] = base64.RawURLEncoding.EncodeToString(signature)
	_, err = (&Manager{http: hc}).verifyID(context.Background(), strings.Join(parts, "."), "client", "nonce")
	require.ErrorContains(t, err, "invalid ChatGPT ID token")
}

func TestLocalAccountLoginURLNeverReturnsTokens(t *testing.T) {
	m, err := newManager(schemas.ChatGPT, schemas.LocalAccountConfig{DataDir: t.TempDir()}, nil)
	require.NoError(t, err)
	require.NoError(t, atomicJSON(m.path, credential{Access: "private-access-token", Refresh: "private-refresh-token", IDToken: "private-id-token", ClientID: "issued-client", Identity: "identity"}))
	require.NoError(t, atomicJSON(filepath.Join(m.root, "registration.json"), map[string]string{"client_id": "issued-client"}))
	login, err := m.StartLogin()
	require.NoError(t, err)
	defer m.Disconnect()
	require.NotContains(t, login.URL, "private-")
	require.NotContains(t, login.URL, "id_token_hint")
	u, err := url.Parse(login.URL)
	require.NoError(t, err)
	require.Equal(t, "issued-client", u.Query().Get("client_id"))
	require.Empty(t, u.Query().Get("agent_name_hint"))
}
