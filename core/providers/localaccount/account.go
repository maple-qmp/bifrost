// Package localaccount implements locally authorized ChatGPT and CodeBuddy providers.
package localaccount

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/maximhq/bifrost/core/schemas"
)

const authOrigin = "https://auth.openai.com"
const resource = "https://api.openai.com/v1"
const directScope = "chatgpt.tokens.use.direct"
const buddyBase = "https://copilot.tencent.com/v2"
const maxAuthBytes = 1 << 20

// Cold credential loads and mutations serialize across overlapping provider rebuilds.
var credentialMutation sync.Mutex

// AccountStatus is safe to send to the management UI.
type AccountStatus struct {
	Connected  bool   `json:"connected"`
	Identity   string `json:"identity,omitempty"`
	ExpiresAt  int64  `json:"expires_at,omitempty"`
	LoginState string `json:"login_state,omitempty"`
	Error      string `json:"error,omitempty"`
}

type credential struct {
	Access   string   `json:"access"`
	Refresh  string   `json:"refresh"`
	ClientID string   `json:"client_id"`
	IDToken  string   `json:"id_token"`
	Expires  int64    `json:"expires"`
	Scopes   []string `json:"scopes"`
	Identity string   `json:"identity"`
}

type session struct {
	token    string
	headers  map[string]string
	identity string
	expires  int64
	source   string
	modified time.Time
}

// Manager owns one credential and at most one pending login. No tokens reach config.json.
type Manager struct {
	modelsDirty bool
	nextCheck   time.Time
	mu          sync.Mutex
	kind        schemas.ModelProvider
	path, root  string
	http        *http.Client
	current     *session
	disabled    bool
	login       *loginSession
	status      AccountStatus
}

func newManager(kind schemas.ModelProvider, cfg schemas.LocalAccountConfig, hc *http.Client) (*Manager, error) {
	root := cfg.DataDir
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		root = filepath.Join(home, ".config", "bifrost")
	}
	root = filepath.Join(root, "accounts")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0700); err != nil {
		return nil, err
	}
	path := cfg.CredentialFile
	if kind == schemas.ChatGPT && path == "" {
		path = filepath.Join(root, "chatgpt.json")
	}
	m := &Manager{kind: kind, path: path, root: root, http: hc}
	_, err := os.Stat(filepath.Join(root, string(kind)+".disconnected"))
	m.disabled = err == nil
	return m, nil
}

func readObject(path string, dst any) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.New("account credentials are unavailable")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxAuthBytes+1))
	if err != nil || len(b) > maxAuthBytes {
		return errors.New("invalid account credential file")
	}
	if json.Unmarshal(b, dst) != nil {
		return errors.New("invalid account credential file")
	}
	return nil
}

func atomicJSON(path string, value any) error {
	b, err := schemas.MarshalSorted(value)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".bifrost-account-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (m *Manager) authorize(ctx context.Context) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.disabled {
		return nil, errors.New("account disconnected; connect it in Providers")
	}
	if m.current != nil && m.current.expires > time.Now().Add(time.Minute).UnixMilli() {
		if m.kind != schemas.CodeBuddy || time.Now().Before(m.nextCheck) {
			return m.current, nil
		}
		m.nextCheck = time.Now().Add(5 * time.Second)
		path, err := m.buddyPath()
		if err == nil {
			if info, err := os.Stat(path); err == nil && path == m.current.source && info.ModTime().Equal(m.current.modified) {
				return m.current, nil
			}
		}
		m.current = nil
	}
	var s *session
	var err error
	if m.kind == schemas.ChatGPT {
		s, err = m.chatGPT(ctx)
	} else {
		s, err = m.codeBuddy(ctx)
	}
	if err != nil {
		m.current = nil
		m.status = AccountStatus{Error: err.Error()}
		return nil, err
	}
	m.current = s
	m.status = AccountStatus{Connected: true, Identity: s.identity, ExpiresAt: s.expires}
	return s, nil
}

func (m *Manager) exchange(ctx context.Context, endpoint string, form url.Values, headers map[string]string) (map[string]json.RawMessage, error) {
	var body io.Reader = strings.NewReader(form.Encode())
	if form == nil {
		body = strings.NewReader("{}")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if form == nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, errors.New("account token exchange failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("account token exchange rejected (HTTP %d); reconnect the account", resp.StatusCode)
	}
	var out map[string]json.RawMessage
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAuthBytes+1))
	if err != nil || len(b) > maxAuthBytes || json.Unmarshal(b, &out) != nil {
		return nil, errors.New("invalid account token response")
	}
	return out, nil
}

func tokenCredential(raw map[string]json.RawMessage, old credential) (credential, error) {
	var tok struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		ID      string `json:"id_token"`
		Scope   string `json:"scope"`
		Exp     int64  `json:"expires_in"`
	}
	b, _ := schemas.MarshalSorted(raw)
	if json.Unmarshal(b, &tok) != nil || tok.Access == "" || tok.Refresh == "" || tok.Exp <= 60 {
		return old, errors.New("incomplete ChatGPT token grant")
	}
	found := false
	for _, s := range strings.Fields(tok.Scope) {
		if s == directScope {
			found = true
		}
	}
	if !found {
		return old, errors.New("ChatGPT plan permission was not granted")
	}
	old.Access = tok.Access
	old.Refresh = tok.Refresh
	old.Expires = time.Now().Add(time.Duration(tok.Exp) * time.Second).UnixMilli()
	old.Scopes = strings.Fields(tok.Scope)
	if tok.ID != "" {
		old.IDToken = tok.ID
	}
	return old, nil
}

func (m *Manager) chatGPT(ctx context.Context) (*session, error) {
	credentialMutation.Lock()
	defer credentialMutation.Unlock()
	var c credential
	if err := readObject(m.path, &c); err != nil {
		return nil, errors.New("ChatGPT is not authorized; connect it in Providers")
	}
	if c.Access == "" || c.Refresh == "" || c.ClientID == "" || c.Identity == "" {
		return nil, errors.New("incomplete ChatGPT credentials; reconnect")
	}
	granted := false
	for _, s := range c.Scopes {
		if s == directScope {
			granted = true
		}
	}
	if !granted {
		return nil, errors.New("ChatGPT plan permission was not granted")
	}
	if c.Expires <= time.Now().Add(time.Minute).UnixMilli() {
		raw, err := m.exchange(ctx, authOrigin+"/api/accounts/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {c.ClientID}, "refresh_token": {c.Refresh}, "resource": {resource}}, nil)
		if err != nil {
			return nil, err
		}
		c, err = tokenCredential(raw, c)
		if err != nil {
			return nil, err
		}
		if err = atomicJSON(m.path, c); err != nil {
			return nil, errors.New("cannot save refreshed ChatGPT credentials")
		}
	}
	return &session{token: c.Access, identity: c.Identity, expires: c.Expires}, nil
}

type buddyAuth struct {
	Access    string `json:"accessToken"`
	Refresh   string `json:"refreshToken"`
	Domain    string `json:"domain"`
	Expires   int64  `json:"expiresAt"`
	ExpiresIn int64  `json:"expiresIn"`
}

// Desktop and refresh endpoints may serialize millisecond timestamps with fractional digits.
func (a *buddyAuth) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Access    string          `json:"accessToken"`
		Refresh   string          `json:"refreshToken"`
		Domain    string          `json:"domain"`
		Expires   json.RawMessage `json:"expiresAt"`
		ExpiresIn json.RawMessage `json:"expiresIn"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	number := func(raw json.RawMessage) (int64, error) {
		if len(raw) == 0 {
			return 0, nil
		}
		var value float64
		if err := json.Unmarshal(raw, &value); err != nil {
			return 0, err
		}
		if value < 0 || value >= float64(math.MaxInt64) {
			return 0, errors.New("invalid CodeBuddy token timestamp")
		}
		return int64(value), nil
	}
	expiry, err := number(wire.Expires)
	if err != nil {
		return err
	}
	seconds, err := number(wire.ExpiresIn)
	if err != nil {
		return err
	}
	*a = buddyAuth{Access: wire.Access, Refresh: wire.Refresh, Domain: wire.Domain, Expires: expiry, ExpiresIn: seconds}
	return nil
}

type buddyAccount struct {
	UID        json.Number `json:"uid"`
	Enterprise json.Number `json:"enterpriseId"`
}

func desktopDirectory() string {
	home, _ := os.UserHomeDir()
	root := os.Getenv("XDG_DATA_HOME")
	if root == "" {
		root = filepath.Join(home, ".local", "share")
	}
	if runtime.GOOS == "darwin" {
		root = filepath.Join(home, "Library", "Application Support")
	}
	if runtime.GOOS == "windows" {
		root = os.Getenv("LOCALAPPDATA")
	}
	return filepath.Join(root, "CodeBuddyExtension", "Data", "Public", "auth")
}

func buddyDocument(path string) (map[string]json.RawMessage, buddyAuth, map[string]any, error) {
	var doc map[string]json.RawMessage
	var a buddyAuth
	var acct map[string]any
	err := readObject(path, &doc)
	if err == nil {
		err = json.Unmarshal(doc["auth"], &a)
	}
	if err == nil {
		d := json.NewDecoder(strings.NewReader(string(doc["account"])))
		d.UseNumber()
		err = d.Decode(&acct)
	}
	if err != nil || a.Access == "" || acct["uid"] == nil {
		return nil, a, acct, errors.New("invalid CodeBuddy credential file")
	}
	switch uid := acct["uid"].(type) {
	case string:
		if uid == "" {
			return nil, a, acct, errors.New("invalid CodeBuddy user ID")
		}
	case json.Number:
	default:
		return nil, a, acct, errors.New("invalid CodeBuddy user ID")
	}
	for _, v := range []string{a.Access, a.Refresh, a.Domain, fmt.Sprint(acct["uid"]), fmt.Sprint(acct["enterpriseId"])} {
		if strings.ContainsAny(v, "\r\n") {
			return nil, a, acct, errors.New("invalid CodeBuddy credential header")
		}
	}
	return doc, a, acct, nil
}

func (m *Manager) buddyPath() (string, error) {
	if m.path != "" {
		return m.path, nil
	}
	entries, err := os.ReadDir(desktopDirectory())
	if err != nil {
		return "", errors.New("CodeBuddy desktop credentials were not found")
	}
	var best string
	var latest time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".info") {
			continue
		}
		p := filepath.Join(desktopDirectory(), e.Name())
		if _, _, _, err := buddyDocument(p); err != nil {
			continue
		}
		info, err := e.Info()
		if err == nil && (best == "" || info.ModTime().After(latest)) {
			best = p
			latest = info.ModTime()
		}
	}
	if best == "" {
		return "", errors.New("sign in to the CodeBuddy desktop app first")
	}
	return best, nil
}

func (m *Manager) codeBuddy(ctx context.Context) (*session, error) {
	credentialMutation.Lock()
	defer credentialMutation.Unlock()
	path, err := m.buddyPath()
	if err != nil {
		return nil, err
	}
	doc, a, acct, err := buddyDocument(path)
	if err != nil {
		return nil, err
	}
	domain := a.Domain
	if domain == "" {
		domain = "www.codebuddy.cn"
	}
	enterprise := ""
	if acct["enterpriseId"] != nil {
		enterprise = fmt.Sprint(acct["enterpriseId"])
	}
	headers := map[string]string{"X-User-Id": fmt.Sprint(acct["uid"]), "X-Enterprise-Id": enterprise, "X-Tenant-Id": enterprise, "X-Domain": domain, "User-Agent": "CLI/2.63.2 CodeBuddy/2.63.2"}
	if a.Expires <= time.Now().Add(time.Minute).UnixMilli() {
		if a.Refresh == "" {
			return nil, errors.New("CodeBuddy token expired; sign in to the desktop app again")
		}
		before, _ := schemas.MarshalSorted(doc)
		refreshHeaders := map[string]string{}
		for k, v := range headers {
			refreshHeaders[k] = v
		}
		refreshHeaders["Authorization"] = "Bearer " + a.Access
		refreshHeaders["X-Refresh-Token"] = a.Refresh
		refreshHeaders["X-Auth-Refresh-Source"] = "plugin"
		raw, err := m.exchange(ctx, buddyBase+"/plugin/auth/token/refresh", nil, refreshHeaders)
		if err != nil {
			return nil, err
		}
		var code int
		json.Unmarshal(raw["code"], &code)
		var fresh buddyAuth
		if code != 0 || json.Unmarshal(raw["data"], &fresh) != nil || fresh.Access == "" {
			return nil, errors.New("invalid CodeBuddy refresh response")
		}
		var check map[string]json.RawMessage
		if err = readObject(path, &check); err != nil {
			return nil, err
		}
		after, _ := schemas.MarshalSorted(check)
		if string(before) != string(after) {
			return nil, errors.New("CodeBuddy credentials changed during refresh; retry")
		}
		if fresh.Refresh == "" {
			fresh.Refresh = a.Refresh
		}
		if fresh.Domain == "" {
			fresh.Domain = domain
		}
		if fresh.Expires == 0 {
			fresh.Expires = time.Now().Add(time.Duration(fresh.ExpiresIn) * time.Second).UnixMilli()
		}
		if strings.ContainsAny(fresh.Access+fresh.Refresh+fresh.Domain, "\r\n") {
			return nil, errors.New("invalid CodeBuddy refresh header")
		}
		if fresh.Expires <= time.Now().Add(time.Minute).UnixMilli() {
			return nil, errors.New("invalid CodeBuddy token expiry")
		}
		var fields map[string]json.RawMessage
		json.Unmarshal(doc["auth"], &fields)
		var updates map[string]json.RawMessage
		json.Unmarshal(raw["data"], &updates)
		for k, v := range updates {
			fields[k] = v
		}
		fields["refreshToken"], _ = json.Marshal(fresh.Refresh)
		fields["domain"], _ = json.Marshal(fresh.Domain)
		fields["expiresAt"], _ = json.Marshal(fresh.Expires)
		doc["auth"], _ = schemas.MarshalSorted(fields)
		if err = atomicJSON(path, doc); err != nil {
			return nil, errors.New("cannot save refreshed CodeBuddy credentials")
		}
		a = fresh
		headers["X-Domain"] = fresh.Domain
	}
	hash := sha256.Sum256([]byte(headers["X-User-Id"] + "\x00" + enterprise + "\x00" + headers["X-Domain"]))
	var modified time.Time
	if info, err := os.Stat(path); err == nil {
		modified = info.ModTime()
	}
	m.nextCheck = time.Now().Add(5 * time.Second)
	return &session{token: a.Access, headers: headers, identity: fmt.Sprintf("%x", hash[:8]), expires: a.Expires, source: path, modified: modified}, nil
}

// Status reads persisted state without making upstream calls.
func (m *Manager) Status() AccountStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.disabled {
		return AccountStatus{}
	}
	status := m.status
	if m.current == nil && status.Error == "" {
		if m.kind == schemas.ChatGPT {
			var c credential
			if readObject(m.path, &c) == nil && c.Access != "" && c.Identity != "" {
				status = AccountStatus{Connected: true, Identity: c.Identity, ExpiresAt: c.Expires}
			}
		} else if path, err := m.buddyPath(); err == nil {
			if _, a, _, err := buddyDocument(path); err == nil {
				status = AccountStatus{Connected: true, ExpiresAt: a.Expires}
			}
		}
	}
	if m.current != nil {
		status = AccountStatus{Connected: true, Identity: m.current.identity, ExpiresAt: m.current.expires}
	}
	if m.login != nil {
		status.LoginState = m.login.stateName
		if m.login.err != "" {
			status.Error = m.login.err
		}
	}
	return status
}

// Detect reconnects the configured local credential source.
func (m *Manager) Detect(ctx context.Context) error {
	m.mu.Lock()
	m.disabled = false
	m.current = nil
	m.mu.Unlock()
	if _, err := m.authorize(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	m.modelsDirty = true
	m.mu.Unlock()
	err := os.Remove(filepath.Join(m.root, string(m.kind)+".disconnected"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Disconnect removes only Bifrost-owned ChatGPT credentials, never desktop credentials.
func (m *Manager) Disconnect() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	credentialMutation.Lock()
	defer credentialMutation.Unlock()
	if err := atomicJSON(filepath.Join(m.root, string(m.kind)+".disconnected"), true); err != nil {
		return err
	}
	m.disabled = true
	m.modelsDirty = false
	m.current = nil
	m.status = AccountStatus{}
	if m.login != nil {
		m.login.cancel()
		m.login = nil
	}
	if m.kind == schemas.ChatGPT {
		if err := os.Remove(m.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func randomID() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func (m *Manager) verifyID(ctx context.Context, raw, clientID, nonce string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, authOrigin+"/.well-known/openid-configuration", nil)
	resp, err := m.http.Do(req)
	if err != nil {
		return "", errors.New("cannot load ChatGPT identity metadata")
	}
	defer resp.Body.Close()
	var meta struct {
		Issuer string `json:"issuer"`
		JWKS   string `json:"jwks_uri"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, maxAuthBytes)).Decode(&meta) != nil || meta.Issuer != authOrigin {
		return "", errors.New("invalid ChatGPT identity metadata")
	}
	u, err := url.Parse(meta.JWKS)
	if err != nil || u.Scheme != "https" || u.Host != "auth.openai.com" || u.User != nil {
		return "", errors.New("invalid ChatGPT signing key URL")
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, meta.JWKS, nil)
	resp2, err := m.http.Do(req)
	if err != nil {
		return "", errors.New("cannot load ChatGPT signing keys")
	}
	defer resp2.Body.Close()
	var keys struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if resp2.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp2.Body, maxAuthBytes)).Decode(&keys) != nil {
		return "", errors.New("invalid ChatGPT signing keys")
	}
	tok, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		for _, k := range keys.Keys {
			if k.Kid != kid || k.Kty != "RSA" {
				continue
			}
			n, e1 := base64.RawURLEncoding.DecodeString(k.N)
			e, e2 := base64.RawURLEncoding.DecodeString(k.E)
			if e1 != nil || e2 != nil || len(e) > 4 {
				continue
			}
			exp := 0
			for _, b := range e {
				exp = exp*256 + int(b)
			}
			if exp < 3 {
				continue
			}
			return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}, nil
		}
		return nil, errors.New("unknown ChatGPT signing key")
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(meta.Issuer), jwt.WithAudience(clientID), jwt.WithExpirationRequired())
	if err != nil || !tok.Valid {
		return "", errors.New("invalid ChatGPT ID token")
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok || claims["nonce"] != nonce {
		return "", errors.New("ChatGPT ID token nonce mismatch")
	}
	sub, err := claims.GetSubject()
	if err != nil || sub == "" {
		return "", errors.New("missing ChatGPT identity")
	}
	hash := sha256.Sum256([]byte(sub + "\x00" + clientID))
	return fmt.Sprintf("%x", hash[:8]), nil
}

// TakeModelRefresh coalesces catalog refreshes after browser callbacks and reconnects.
func (m *Manager) TakeModelRefresh() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	dirty := m.modelsDirty
	m.modelsDirty = false
	return dirty
}
