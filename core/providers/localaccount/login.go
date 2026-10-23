package localaccount

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/schemas"
)

type loginSession struct {
	id, state, nonce, verifier, redirect string
	clientID, expectedIdentity           string
	stateName, err                       string
	cancel                               context.CancelFunc
	context                              context.Context
}

// LoginStart contains an authorization URL and an opaque session handle, never tokens.
type LoginStart struct {
	SessionID string `json:"session_id"`
	URL       string `json:"url"`
}

// StartLogin starts a short-lived loopback callback server for this installation.
func (m *Manager) StartLogin() (*LoginStart, error) {
	if m.kind != schemas.ChatGPT {
		return nil, errors.New("CodeBuddy uses desktop sign-in")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.login != nil && m.login.stateName == "pending" {
		return nil, errors.New("a ChatGPT login is already pending")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:1455")
	if err != nil {
		return nil, errors.New("cannot listen on 127.0.0.1:1455; close the other login and retry")
	}
	hostPath := filepath.Join(m.root, "host.json")
	var host string
	if err = readObject(hostPath, &host); err != nil {
		if _, statErr := os.Stat(hostPath); !os.IsNotExist(statErr) {
			listener.Close()
			return nil, errors.New("invalid Bifrost account host ID")
		}
		host = uuid.NewString()
		if err = atomicJSON(hostPath, host); err != nil {
			listener.Close()
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	l := &loginSession{id: randomID(), state: randomID(), nonce: randomID(), verifier: randomID(), redirect: "http://127.0.0.1:1455/auth/callback", stateName: "pending", cancel: cancel, context: ctx}
	m.login = l
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 20 * time.Second, MaxHeaderBytes: 8192}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet || r.URL.Path != "/auth/callback" {
			http.Error(w, "invalid callback", 400)
			return
		}
		// Only exact loopback callback URLs are accepted, irrespective of Host headers.
		if err := m.CompleteLogin(l.id, l.redirect+"?"+r.URL.RawQuery); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("Bifrost ChatGPT sign-in completed. You can close this window."))
	})
	go func() { _ = server.Serve(listener) }()
	go func() {
		<-ctx.Done()
		_ = server.Close()
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.login == l && (l.stateName == "pending" || l.stateName == "exchanging") {
			l.stateName = "expired"
			l.err = "login timed out; try again"
		}
	}()
	challenge := sha256.Sum256([]byte(l.verifier))
	q := url.Values{"client_id": {"dynamic_agent_client"}, "agent_name_hint": {"Bifrost"}, "ext_agent_host_id": {"urn:uuid:" + host}, "response_type": {"code"}, "redirect_uri": {l.redirect}, "resource": {resource}, "scope": {"openid profile email offline_access resource.invoke " + directScope}, "state": {l.state}, "nonce": {l.nonce}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}}
	// Reuse the issued client registration on subsequent sign-ins on this host.
	var old credential
	if readObject(filepath.Join(m.root, "registration.json"), &old) == nil && old.ClientID != "" {
		q.Set("client_id", old.ClientID)
		q.Del("agent_name_hint")
		l.clientID = old.ClientID
		var active credential
		if readObject(m.path, &active) == nil {
			l.expectedIdentity = active.Identity
		}
	}
	return &LoginStart{SessionID: l.id, URL: authOrigin + "/api/accounts/authorize?" + q.Encode()}, nil
}

// CompleteLogin validates and consumes either a browser callback or a manually pasted URL.
func (m *Manager) CompleteLogin(id, callback string) error {
	m.mu.Lock()
	l := m.login
	if l == nil || l.id != id || l.stateName != "pending" || l.context.Err() != nil {
		m.mu.Unlock()
		return errors.New("login session is no longer pending")
	}
	u, err := url.Parse(strings.TrimSpace(callback))
	if err != nil || u.Scheme != "http" || u.Host != "127.0.0.1:1455" || u.Path != "/auth/callback" || u.User != nil || u.Fragment != "" {
		m.mu.Unlock()
		return errors.New("callback URL does not match the Bifrost redirect URI")
	}
	q := u.Query()
	if len(q["state"]) != 1 || q.Get("state") != l.state {
		m.mu.Unlock()
		return errors.New("OAuth state mismatch")
	}
	if q.Get("error") != "" {
		l.stateName = "failed"
		l.err = "ChatGPT authorization declined"
		l.cancel()
		m.mu.Unlock()
		return errors.New("ChatGPT authorization declined")
	}
	if len(q["code"]) != 1 || q.Get("code") == "" || len(q["client_id"]) > 1 {
		m.mu.Unlock()
		return errors.New("callback is missing code or issued client ID")
	}
	clientID := q.Get("client_id")
	if l.clientID != "" {
		if clientID != "" && clientID != l.clientID {
			m.mu.Unlock()
			return errors.New("callback client ID does not match the pending registration")
		}
		clientID = l.clientID
	}
	if strings.TrimSpace(clientID) == "" || clientID == "dynamic_agent_client" {
		m.mu.Unlock()
		return errors.New("callback is missing an issued client ID")
	}
	l.stateName = "exchanging"
	m.mu.Unlock()
	raw, err := m.exchange(l.context, authOrigin+"/api/accounts/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {q.Get("code")}, "code_verifier": {l.verifier}, "redirect_uri": {l.redirect}, "resource": {resource}}, nil)
	var c credential
	if err == nil {
		c, err = tokenCredential(raw, credential{ClientID: clientID})
		if err == nil {
			c.Identity, err = m.verifyID(l.context, c.IDToken, clientID, l.nonce)
		}
	}
	if err == nil && l.expectedIdentity != "" && c.Identity != l.expectedIdentity {
		err = errors.New("ChatGPT account identity changed during reauthorization")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.login != l || l.context.Err() != nil {
		l.cancel()
		return errors.New("login session was cancelled")
	}
	if err == nil {
		credentialMutation.Lock()
		defer credentialMutation.Unlock()
		err = atomicJSON(filepath.Join(m.root, "registration.json"), map[string]string{"client_id": c.ClientID})
		if err == nil {
			err = atomicJSON(m.path, c)
		}
	}
	if err != nil {
		l.stateName = "failed"
		l.err = "ChatGPT sign-in failed; retry authorization"
		l.cancel()
		return errors.New(l.err)
	}
	if err = os.Remove(filepath.Join(m.root, string(schemas.ChatGPT)+".disconnected")); err != nil && !os.IsNotExist(err) {
		l.stateName = "failed"
		l.err = "cannot reconnect account"
		l.cancel()
		return errors.New(l.err)
	}
	m.disabled = false
	m.modelsDirty = true
	m.current = &session{token: c.Access, identity: c.Identity, expires: c.Expires}
	m.status = AccountStatus{Connected: true, Identity: c.Identity, ExpiresAt: c.Expires}
	l.stateName = "completed"
	// Close after the callback response has been flushed.
	time.AfterFunc(100*time.Millisecond, l.cancel)
	return nil
}
