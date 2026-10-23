package handlers

import (
	"context"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/providers/localaccount"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// localAccountRequest prevents remote management and cross-origin browser access to local credentials.
func localAccountRequest(ctx *fasthttp.RequestCtx) bool {
	if !ctx.RemoteIP().IsLoopback() {
		return false
	}
	host := string(ctx.Host())
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}
	ip := net.ParseIP(hostname)
	if hostname != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return false
	}
	if origin := string(ctx.Request.Header.Peek("Origin")); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || (u.Host != host && !(IsDevMode() && isLocalhostOrigin(origin) && u.Port() == "3000")) {
			return false
		}
	}
	return string(ctx.Request.Header.Peek("Sec-Fetch-Site")) != "cross-site"
}

func (h *ProviderHandler) providerAccount(ctx *fasthttp.RequestCtx) {
	ctx.Response.Header.Set("Cache-Control", "no-store")
	if !localAccountRequest(ctx) {
		SendError(ctx, fasthttp.StatusForbidden, "Account management requires a same-origin connection on this computer")
		return
	}
	name, _ := ctx.UserValue("provider").(string)
	p, ok := h.client.GetProviderByKey(schemas.ModelProvider(name)).(*localaccount.Provider)
	if !ok {
		SendError(ctx, fasthttp.StatusNotFound, "Local account provider is not configured")
		return
	}
	manager := p.Account()
	path := string(ctx.Path())
	method := string(ctx.Method())
	var result any
	var err error
	switch {
	case method == "GET":
		result = manager.Status()
	case method == "DELETE":
		err = manager.Disconnect()
		if h.inMemoryStore.ModelCatalog != nil {
			h.inMemoryStore.ModelCatalog.InvalidateLiveProvider(schemas.ModelProvider(name))
		}
		p.InvalidateModels()
		result = manager.Status()
	case strings.HasSuffix(path, "/login"):
		result, err = manager.StartLogin()
	case strings.HasSuffix(path, "/callback"):
		var payload struct {
			SessionID string `json:"session_id"`
			URL       string `json:"url"`
		}
		if len(ctx.PostBody()) > 16384 || sonic.Unmarshal(ctx.PostBody(), &payload) != nil {
			SendError(ctx, 400, "Invalid login callback payload")
			return
		}
		err = manager.CompleteLogin(payload.SessionID, payload.URL)
		p.InvalidateModels()
		result = manager.Status()
	default:
		authCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err = manager.Detect(authCtx)
		p.InvalidateModels()
		result = manager.Status()
	}
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	if manager.TakeModelRefresh() {
		p.InvalidateModels()
		if h.inMemoryStore.ModelCatalog != nil {
			h.inMemoryStore.ModelCatalog.InvalidateLiveProvider(schemas.ModelProvider(name))
		}
		refreshCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_ = h.modelsManager.RefreshLiveModelsForAllKeys(refreshCtx, schemas.ModelProvider(name))
		cancel()
	}
	data, err := schemas.MarshalSorted(result)
	if err != nil {
		SendError(ctx, 500, "Cannot encode account status")
		return
	}
	ctx.SetContentType("application/json")
	ctx.SetBody(data)
}

// Existing custom providers may already use a newly introduced first-party name.
func (h *ProviderHandler) isNativeAccountProvider(provider schemas.ModelProvider) bool {
	if !schemas.IsLocalAccountProvider(provider) {
		return false
	}
	if h.inMemoryStore == nil {
		return true
	}
	config, err := h.inMemoryStore.GetProviderConfigRaw(provider)
	return err != nil || config == nil || config.CustomProviderConfig == nil
}
