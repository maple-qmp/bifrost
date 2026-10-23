package localaccount

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"
	"github.com/maximhq/bifrost/core/providers/openai"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
	"github.com/valyala/fasthttp"
)

const maxAggregateBytes = 32 << 20

// Provider keeps credentials, the model catalog and bounded unary collectors off contexts.
type Provider struct {
	kind                    schemas.ModelProvider
	logger                  schemas.Logger
	client, streamingClient *fasthttp.Client
	network                 schemas.NetworkConfig
	account                 *Manager
	compat                  bool
	modelMu                 sync.Mutex
	models                  *schemas.BifrostListModelsResponse
	modelIdentity           string
	modelExpiry             time.Time
	collectors              sync.Map
}

var _ schemas.Provider = (*Provider)(nil)

// NewProvider builds independent account-backed inference clients.
func NewProvider(kind schemas.ModelProvider, cfg *schemas.ProviderConfig, logger schemas.Logger) (*Provider, error) {
	cfg.CheckAndSetDefaults()
	var ac schemas.LocalAccountConfig
	if cfg.LocalAccountConfig != nil {
		ac = *cfg.LocalAccountConfig
	}
	hc := providerUtils.NewProviderHTTPClient(cfg.ProxyConfig, cfg.NetworkConfig, logger)
	hc.Timeout = 15 * time.Second
	// Never follow an auth endpoint redirect with account headers.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	manager, err := newManager(kind, ac, hc)
	if err != nil {
		return nil, err
	}
	n := cfg.NetworkConfig
	if n.BaseURL == "" {
		if kind == schemas.ChatGPT {
			n.BaseURL = resource
		} else {
			n.BaseURL = buddyBase
		}
	}
	n.BaseURL = strings.TrimRight(n.BaseURL, "/")
	c := &fasthttp.Client{ReadTimeout: time.Duration(n.DefaultRequestTimeoutInSeconds) * time.Second, WriteTimeout: time.Duration(n.DefaultRequestTimeoutInSeconds) * time.Second, MaxConnsPerHost: n.MaxConnsPerHost, MaxIdleConnDuration: time.Duration(n.KeepAliveTimeoutInSeconds) * time.Second, MaxResponseBodySize: maxAuthBytes}
	c = providerUtils.ConfigureProxy(c, cfg.ProxyConfig, logger)
	c = providerUtils.ConfigureDialer(c, n.AllowPrivateNetwork)
	c = providerUtils.ConfigureTLS(c, n, logger)
	streaming := providerUtils.BuildStreamingClient(c)
	streaming.MaxResponseBodySize = 0
	return &Provider{kind: kind, logger: logger, client: c, streamingClient: streaming, network: n, account: manager, compat: ac.PromptCompat == nil || *ac.PromptCompat}, nil
}

func (p *Provider) GetProviderKey() schemas.ModelProvider { return p.kind }

// Account exposes only account lifecycle operations to the transport.
func (p *Provider) Account() *Manager { return p.account }

// InvalidateModels drops the one-account catalog when connection state changes.
func (p *Provider) InvalidateModels() {
	p.modelMu.Lock()
	p.models = nil
	p.modelIdentity = ""
	p.modelExpiry = time.Time{}
	p.modelMu.Unlock()
}

func (p *Provider) auth(ctx context.Context) (map[string]string, *schemas.BifrostError) {
	s, err := p.account.authorize(ctx)
	if err != nil {
		e := providerUtils.NewConfigurationError(err.Error())
		e.AllowFallbacks = schemas.Ptr(false)
		return nil, e
	}
	h := maps.Clone(s.headers)
	if h == nil {
		h = make(map[string]string, 1)
	}
	h["Authorization"] = "Bearer " + s.token
	return h, nil
}

func (p *Provider) ListModels(ctx *schemas.BifrostContext, _ []schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	s, err := p.account.authorize(ctx)
	if err != nil {
		return nil, providerUtils.NewConfigurationError(err.Error())
	}
	p.modelMu.Lock()
	defer p.modelMu.Unlock()
	if p.models != nil && p.modelIdentity == s.identity && time.Now().Before(p.modelExpiry) {
		copy := *p.models
		copy.Data = append([]schemas.Model(nil), p.models.Data...)
		return &copy, nil
	}
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	path := "/models"
	if p.kind == schemas.CodeBuddy {
		path = "/enterprises/personal/models"
	}
	req.SetRequestURI(p.network.BaseURL + path)
	req.Header.SetMethod("GET")
	providerUtils.SetExtraHeaders(ctx, req, p.network.ExtraHeaders, nil)
	for k, v := range s.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	_, requestErr, wait := providerUtils.MakeRequestWithContext(ctx, p.client, req, resp)
	defer wait()
	if requestErr != nil {
		return nil, requestErr
	}
	if resp.StatusCode() != 200 {
		return nil, p.apiError(resp)
	}
	var body struct {
		Data   json.RawMessage `json:"data"`
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
		Code int `json:"code"`
	}
	if len(resp.Body()) > maxAuthBytes || sonic.Unmarshal(resp.Body(), &body) != nil {
		return nil, providerUtils.NewBifrostOperationError("invalid model catalog", nil)
	}
	var entries []struct {
		ID string `json:"id"`
	}
	if p.kind == schemas.CodeBuddy {
		var data struct {
			Models []struct {
				ID string `json:"id"`
			} `json:"models"`
		}
		if body.Code != 0 || sonic.Unmarshal(body.Data, &data) != nil {
			return nil, providerUtils.NewBifrostOperationError("invalid CodeBuddy model catalog", nil)
		}
		entries = data.Models
	} else if len(body.Models) > 0 {
		for _, m := range body.Models {
			entries = append(entries, struct {
				ID string `json:"id"`
			}{m.Slug})
		}
	} else if sonic.Unmarshal(body.Data, &entries) != nil {
		return nil, providerUtils.NewBifrostOperationError("invalid ChatGPT model catalog", nil)
	}
	out := &schemas.BifrostListModelsResponse{Data: make([]schemas.Model, 0, min(len(entries), 4096))}
	seen := map[string]bool{}
	for _, m := range entries {
		if m.ID == "" || m.ID == "codex-auto-review" || seen[m.ID] {
			continue
		}
		if len(out.Data) >= 4096 {
			return nil, providerUtils.NewBifrostOperationError("account model catalog exceeds 4096 entries", nil)
		}
		seen[m.ID] = true
		out.Data = append(out.Data, schemas.Model{ID: string(p.kind) + "/" + m.ID})
	}
	p.models = out
	p.modelIdentity = s.identity
	p.modelExpiry = time.Now().Add(5 * time.Minute)
	copy := *out
	copy.Data = append([]schemas.Model(nil), out.Data...)
	return &copy, nil
}

func (p *Provider) apiError(resp *fasthttp.Response) *schemas.BifrostError {
	e := openai.ParseOpenAIError(resp)
	if detail := providerUtils.GetJSONField(resp.Body(), "detail"); detail.Type == gjson.String && detail.Str != "" {
		e.Error.Message = detail.Str
	}
	if requestID := string(resp.Header.Peek("x-request-id")); requestID != "" {
		e.EventID = schemas.Ptr(requestID)
	}
	if p.kind == schemas.ChatGPT {
		e.AllowFallbacks = schemas.Ptr(false)
	}
	if p.kind == schemas.CodeBuddy && providerUtils.GetJSONField(resp.Body(), "code").Int() == 11128 {
		e.Error.Code = schemas.Ptr("upstream_rejected_11128")
	}
	return e
}

// ChatGPT's direct-plan route is stateless HTTP Responses with a narrower wire surface.
// See https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations.
func (p *Provider) responseRequest(r *openai.OpenAIResponsesRequest) *openai.OpenAIResponsesRequest {
	r.Store = schemas.Ptr(false)
	r.Background = nil
	r.Conversation = nil
	r.PreviousResponseID = nil
	r.MaxOutputTokens = nil
	r.MaxToolCalls = nil
	r.Temperature = nil
	r.Metadata = nil
	r.PromptCacheRetention = nil
	r.SafetyIdentifier = nil
	r.TopLogProbs = nil
	r.TopP = nil
	r.Truncation = nil
	r.User = nil
	r.ExtraParams = maps.Clone(r.ExtraParams)
	for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens", "background", "conversation", "max_tool_calls", "metadata", "moderation", "multi_agent", "prompt", "prompt_cache_retention", "safety_identifier", "temperature", "top_logprobs", "top_p", "truncation", "user", "previous_response_id", "store", "stream", "input", "model", "tools"} {
		delete(r.ExtraParams, k)
	}
	if r.Input.OpenAIResponsesRequestInputStr != nil {
		r.Input.OpenAIResponsesRequestInputArray = (&schemas.BifrostChatRequest{Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: r.Input.OpenAIResponsesRequestInputStr}}}}).ToResponsesRequest().Input
		r.Input.OpenAIResponsesRequestInputStr = nil
	}
	for i, m := range r.Input.OpenAIResponsesRequestInputArray {
		if m.Role != nil && *m.Role == schemas.ResponsesInputMessageRoleSystem {
			r.Input.OpenAIResponsesRequestInputArray[i].Role = schemas.Ptr(schemas.ResponsesInputMessageRoleDeveloper)
		}
	}
	// Functions are declared through additional_tools so their names remain unchanged.
	// Decode the one constructed item to engage the schema's raw-preserved item path.
	var functions, hosted []schemas.ResponsesTool
	for _, tool := range r.Tools {
		if tool.Type == schemas.ResponsesToolTypeToolSearch || (tool.Name != nil && strings.HasPrefix(*tool.Name, "tool_search_tool_")) {
			continue
		}
		tool.DeferLoading = nil
		tool.CacheControl = nil
		tool.AllowedCallers = nil
		tool.InputExamples = nil
		tool.EagerInputStreaming = nil
		switch tool.Type {
		case schemas.ResponsesToolTypeFunction, schemas.ResponsesToolTypeCustom, schemas.ResponsesToolTypeNamespace:
			functions = append(functions, tool)
		default:
			hosted = append(hosted, tool)
		}
	}
	if len(functions) > 0 {
		raw, err := schemas.MarshalSorted(struct {
			Type  string                  `json:"type"`
			Tools []schemas.ResponsesTool `json:"tools"`
		}{"additional_tools", functions})
		if err == nil {
			var item schemas.ResponsesMessage
			if schemas.Unmarshal(raw, &item) == nil {
				r.Input.OpenAIResponsesRequestInputArray = append([]schemas.ResponsesMessage{item}, r.Input.OpenAIResponsesRequestInputArray...)
			}
		}
	}
	if len(r.Tools) > 0 && len(functions) == 0 && len(hosted) == 0 {
		r.ToolChoice = nil
	}
	r.Tools = hosted
	return r
}

func validatePlanTools(tools []schemas.ResponsesTool) error {
	for _, tool := range tools {
		switch tool.Type {
		case schemas.ResponsesToolTypeFunction, schemas.ResponsesToolTypeCustom, schemas.ResponsesToolTypeWebSearch, schemas.ResponsesToolTypeWebSearchPreview, schemas.ResponsesToolTypeToolSearch:
		case schemas.ResponsesToolTypeNamespace:
			if tool.ResponsesToolNamespace != nil {
				if err := validatePlanTools(tool.ResponsesToolNamespace.Tools); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("ChatGPT plan does not support hosted tool %q", tool.Type)
		}
	}
	return nil
}

func (p *Provider) responseStream(ctx *schemas.BifrostContext, hook schemas.PostHookRunner, final func(context.Context), r *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	if r != nil && r.Params != nil {
		if r.Params.PreviousResponseID != nil || r.Params.Conversation != nil || (r.Params.Background != nil && *r.Params.Background) {
			return nil, providerUtils.NewBifrostBadRequestError("ChatGPT plan HTTP requests require full input history; stored conversations, previous_response_id and background mode are unsupported")
		}
		if err := validatePlanTools(r.Params.Tools); err != nil {
			return nil, providerUtils.NewBifrostBadRequestError(err.Error())
		}
	}

	if hook == nil {
		hook = identityHook
	}
	if providerUtils.IsLargePayloadPassthroughEnabled(ctx) {
		return nil, providerUtils.NewBifrostBadRequestError("large payload passthrough is not supported for local account providers")
	}
	if r == nil || r.Input == nil {
		return nil, providerUtils.NewBifrostBadRequestError("account inference requires input")
	}
	h, e := p.auth(ctx)
	if e != nil {
		return nil, e
	}
	// Account providers require normalized requests; raw passthrough would bypass their protocol restrictions.
	ctx.SetValue(schemas.BifrostContextKeyUseRawRequestBody, false)
	wrapped := func(c *schemas.BifrostContext, r *schemas.BifrostResponse, e *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		if e != nil {
			e.AllowFallbacks = schemas.Ptr(false)
		}
		return hook(c, r, e)
	}
	return openai.HandleOpenAIResponsesStreaming(ctx, p.streamingClient, p.network.BaseURL+"/responses", r, h, p.network.ExtraHeaders, p.network.StreamIdleTimeoutInSeconds, false, false, p.kind, wrapped, nil, p.apiError, p.responseRequest, nil, nil, p.logger, final)
}

func (p *Provider) chatStream(ctx *schemas.BifrostContext, hook schemas.PostHookRunner, final func(context.Context), r *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	if r != nil && r.Params != nil && r.Params.ToolChoice != nil && r.Params.ToolChoice.ChatToolChoiceStruct != nil {
		return nil, providerUtils.NewBifrostBadRequestError("CodeBuddy does not support selecting a specific tool; use auto or required")
	}
	if hook == nil {
		hook = identityHook
	}
	if providerUtils.IsLargePayloadPassthroughEnabled(ctx) {
		return nil, providerUtils.NewBifrostBadRequestError("large payload passthrough is not supported for local account providers")
	}
	if r == nil || r.Input == nil {
		return nil, providerUtils.NewBifrostBadRequestError("account inference requires input")
	}
	h, e := p.auth(ctx)
	if e != nil {
		return nil, e
	}
	ctx.SetValue(schemas.BifrostContextKeyUseRawRequestBody, false)
	return openai.HandleOpenAIChatCompletionStreaming(ctx, p.streamingClient, p.network.BaseURL+"/chat/completions", r, h, p.network.ExtraHeaders, p.network.StreamIdleTimeoutInSeconds, false, false, p.kind, hook, nil, nil, p.apiError, p.chatRequest, nil, nil, p.logger, final)
}

func identityHook(_ *schemas.BifrostContext, r *schemas.BifrostResponse, e *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
	return r, e
}

func (p *Provider) ChatCompletionStream(ctx *schemas.BifrostContext, hook schemas.PostHookRunner, final func(context.Context), _ schemas.Key, r *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	if r == nil {
		return nil, providerUtils.NewBifrostBadRequestError("account inference requires input")
	}
	if hook == nil {
		hook = identityHook
	}
	if p.kind == schemas.CodeBuddy {
		return p.chatStream(ctx, hook, final, r)
	}
	// Responses deltas omit response metadata; retain only these small scalar handles.
	id, model := "", r.Model
	created := 0
	wrap := func(c *schemas.BifrostContext, out *schemas.BifrostResponse, e *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		if out != nil && out.ResponsesStreamResponse != nil {
			event := out.ResponsesStreamResponse
			if id == "" {
				created = int(time.Now().Unix())
				if event.Response != nil {
					if event.Response.ID != nil {
						id = *event.Response.ID
					}
					if event.Response.Model != "" {
						model = event.Response.Model
					}
					if event.Response.CreatedAt > 0 {
						created = event.Response.CreatedAt
					}
				}
				if id == "" {
					id = "chatcmpl-" + uuid.NewString()
				}
			}
			chat := event.ToBifrostChatResponse()
			chat.ID = id
			chat.Model = model
			chat.Created = created
			out = &schemas.BifrostResponse{ChatResponse: chat}
		}
		return hook(c, out, e)
	}
	return p.responseStream(ctx, wrap, final, r.ToResponsesRequest())
}

func (p *Provider) ResponsesStream(ctx *schemas.BifrostContext, hook schemas.PostHookRunner, final func(context.Context), _ schemas.Key, r *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	if p.kind == schemas.ChatGPT {
		return p.responseStream(ctx, hook, final, r)
	}
	ctx.SetValue(schemas.BifrostContextKeyIsResponsesToChatCompletionFallback, true)
	return p.chatStream(ctx, hook, final, r.ToChatRequest())
}

func (p *Provider) ChatCompletion(ctx *schemas.BifrostContext, _ schemas.Key, r *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	if p.kind == schemas.ChatGPT {
		out, e := p.Responses(ctx, schemas.Key{}, r.ToResponsesRequest())
		if e != nil {
			return nil, e
		}
		return out.ToBifrostChatResponse(), nil
	}
	child, cancel := schemas.NewBifrostContextWithCancel(ctx)
	defer cancel()
	ch, e := p.chatStream(child, identityHook, nil, r)
	if e != nil {
		return nil, e
	}
	state := newCollector()
	id := p.registerCollector(ctx, state)
	defer p.collectors.Delete(id)
	for chunk := range ch {
		if chunk.BifrostError != nil {
			return nil, chunk.BifrostError
		}
		if chunk.BifrostChatResponse == nil {
			continue
		}
		if err := state.addChat(chunk.BifrostChatResponse); err != nil {
			return nil, providerUtils.NewBifrostOperationError(err.Error(), nil)
		}
	}
	if child.Err() != nil {
		return nil, providerUtils.NewBifrostOperationError("account request cancelled", child.Err())
	}
	return state.chat()
}

func collectorID(ctx *schemas.BifrostContext) string {
	id, _ := ctx.Value(schemas.BifrostContextKeyRequestID).(string)
	if id == "" {
		id = uuid.NewString()
	}
	return id
}

func (p *Provider) Responses(ctx *schemas.BifrostContext, _ schemas.Key, r *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	if p.kind == schemas.CodeBuddy {
		out, e := p.ChatCompletion(ctx, schemas.Key{}, r.ToChatRequest())
		if e != nil {
			return nil, e
		}
		return out.ToBifrostResponsesResponse(), nil
	}
	child, cancel := schemas.NewBifrostContextWithCancel(ctx)
	defer cancel()
	ch, e := p.responseStream(child, identityHook, nil, r)
	if e != nil {
		return nil, e
	}
	state := newCollector()
	id := p.registerCollector(ctx, state)
	defer p.collectors.Delete(id)
	for chunk := range ch {
		if chunk.BifrostError != nil {
			return nil, chunk.BifrostError
		}
		rs := chunk.BifrostResponsesStreamResponse
		if rs == nil {
			continue
		}
		raw, err := schemas.MarshalSorted(rs)
		if err != nil {
			return nil, providerUtils.NewBifrostOperationError("cannot collect Responses event", err)
		}
		state.bytes += len(raw)
		if state.bytes > maxAggregateBytes {
			return nil, providerUtils.NewBifrostOperationError("account response exceeds 32 MiB aggregation limit", nil)
		}
		if rs.Type == schemas.ResponsesStreamResponseTypeOutputItemDone && rs.Item != nil && rs.OutputIndex != nil {
			state.items[*rs.OutputIndex] = *rs.Item
		}
		if rs.Type == schemas.ResponsesStreamResponseTypeCompleted {
			state.response = rs.Response
			if state.response != nil {
				state.response.ExtraFields = rs.ExtraFields
			}
		}
	}
	if state.response == nil {
		return nil, providerUtils.NewBifrostOperationError("upstream Responses stream did not complete", nil)
	}
	if len(state.items) > 0 {
		indexes := make([]int, 0, len(state.items))
		for i := range state.items {
			indexes = append(indexes, i)
		}
		sort.Ints(indexes)
		state.response.Output = nil
		for _, i := range indexes {
			state.response.Output = append(state.response.Output, state.items[i])
		}
	}
	return state.response, nil
}

// collector retains at most 32 MiB of serialized events and is released on every exit path.
type reasoningKey struct {
	choice, index int
	kind          string
}

type collector struct {
	reasoning map[reasoningKey]map[string]any
	bytes     int
	response  *schemas.BifrostResponsesResponse
	items     map[int]schemas.ResponsesMessage
	base      map[string]any
	choices   map[int]map[string]any
	tools     map[int]map[int]map[string]any
	finished  bool
}

func newCollector() *collector {
	return &collector{items: map[int]schemas.ResponsesMessage{}, base: map[string]any{}, choices: map[int]map[string]any{}, tools: map[int]map[int]map[string]any{}}
}
func appendText(dst map[string]any, key string, v any) {
	s, ok := v.(string)
	if !ok {
		return
	}
	if s == "" {
		return
	}
	builder, _ := dst[key].(*strings.Builder)
	if builder == nil {
		builder = &strings.Builder{}
		dst[key] = builder
	}
	builder.WriteString(s)
}
func (c *collector) addChat(r *schemas.BifrostChatResponse) error {
	b, err := schemas.MarshalSorted(r)
	if err != nil {
		return err
	}
	c.bytes += len(b)
	if c.bytes > maxAggregateBytes {
		return fmt.Errorf("account response exceeds 32 MiB aggregation limit")
	}
	var raw map[string]any
	if err = sonic.Unmarshal(b, &raw); err != nil {
		return err
	}
	for _, k := range []string{"id", "model", "created", "usage", "service_tier", "system_fingerprint", "extra_fields"} {
		if raw[k] != nil {
			c.base[k] = raw[k]
		}
	}
	choices, _ := raw["choices"].([]any)
	for _, entry := range choices {
		ch, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		idx, _ := ch["index"].(float64)
		i := int(idx)
		choice := c.choices[i]
		if choice == nil {
			choice = map[string]any{"index": i, "message": map[string]any{"role": "assistant"}}
			c.choices[i] = choice
			c.tools[i] = map[int]map[string]any{}
		}
		if ch["finish_reason"] != nil {
			choice["finish_reason"] = ch["finish_reason"]
			c.finished = true
		}
		msg := choice["message"].(map[string]any)
		delta, _ := ch["delta"].(map[string]any)
		for _, key := range []string{"content", "reasoning", "reasoning_content", "refusal"} {
			appendText(msg, key, delta[key])
		}
		if details, ok := delta["reasoning_details"].([]any); ok {
			if c.reasoning == nil {
				c.reasoning = map[reasoningKey]map[string]any{}
			}
			for _, entry := range details {
				node, ok := entry.(map[string]any)
				if !ok {
					continue
				}
				index, _ := node["index"].(float64)
				kind, _ := node["type"].(string)
				key := reasoningKey{i, int(index), kind}
				current := c.reasoning[key]
				if current == nil {
					current = map[string]any{"index": int(index), "type": kind}
					c.reasoning[key] = current
				}
				if node["id"] != nil {
					current["id"] = node["id"]
				}
				for _, field := range []string{"text", "summary", "signature", "data"} {
					appendText(current, field, node[field])
				}
			}
		}
		calls, _ := delta["tool_calls"].([]any)
		for _, v := range calls {
			call, ok := v.(map[string]any)
			if !ok {
				continue
			}
			index, _ := call["index"].(float64)
			tool := c.tools[i][int(index)]
			if tool == nil {
				tool = map[string]any{"type": "function", "function": map[string]any{}}
				c.tools[i][int(index)] = tool
			}
			appendText(tool, "id", call["id"])
			fn, _ := call["function"].(map[string]any)
			dst := tool["function"].(map[string]any)
			appendText(dst, "name", fn["name"])
			appendText(dst, "arguments", fn["arguments"])
		}
	}
	return nil
}
func (c *collector) chat() (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	if !c.finished {
		return nil, providerUtils.NewBifrostOperationError("upstream chat stream did not complete", nil)
	}
	indexes := make([]int, 0, len(c.choices))
	for i := range c.choices {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	choices := make([]any, 0, len(indexes))
	for _, i := range indexes {
		choice := c.choices[i]
		if choice["finish_reason"] == nil {
			return nil, providerUtils.NewBifrostOperationError("upstream chat stream did not complete every choice", nil)
		}
		msg := choice["message"].(map[string]any)
		var detailKeys []reasoningKey
		for key := range c.reasoning {
			if key.choice == i {
				detailKeys = append(detailKeys, key)
			}
		}
		sort.Slice(detailKeys, func(a, b int) bool {
			if detailKeys[a].index != detailKeys[b].index {
				return detailKeys[a].index < detailKeys[b].index
			}
			return detailKeys[a].kind < detailKeys[b].kind
		})
		if len(detailKeys) > 0 {
			details := make([]any, 0, len(detailKeys))
			for _, key := range detailKeys {
				details = append(details, c.reasoning[key])
			}
			msg["reasoning_details"] = details
		}
		ti := make([]int, 0, len(c.tools[i]))
		for idx := range c.tools[i] {
			ti = append(ti, idx)
		}
		sort.Ints(ti)
		if len(ti) > 0 {
			tools := make([]any, 0, len(ti))
			for _, idx := range ti {
				tools = append(tools, c.tools[i][idx])
			}
			msg["tool_calls"] = tools
		}
		choices = append(choices, choice)
	}
	c.base["choices"] = choices
	c.base["object"] = "chat.completion"
	b, err := schemas.MarshalSorted(materialize(c.base))
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError("cannot collect chat response", err)
	}
	var out schemas.BifrostChatResponse
	if err = sonic.Unmarshal(b, &out); err != nil {
		return nil, providerUtils.NewBifrostOperationError("cannot collect chat response", err)
	}
	return &out, nil
}

func materialize(value any) any {
	switch v := value.(type) {
	case *strings.Builder:
		return v.String()
	case map[string]any:
		for k, child := range v {
			v[k] = materialize(child)
		}
		return v
	case []any:
		for i, child := range v {
			v[i] = materialize(child)
		}
		return v
	default:
		return value
	}
}

// Close releases caches and pending login listeners without disconnecting the stored account.
func (p *Provider) Close() {
	p.account.mu.Lock()
	if p.account.login != nil {
		p.account.login.cancel()
		p.account.login = nil
	}
	p.account.current = nil
	p.account.mu.Unlock()
	p.InvalidateModels()
	p.client.CloseIdleConnections()
	p.streamingClient.CloseIdleConnections()
	p.account.http.CloseIdleConnections()
}

// A caller-supplied RequestID may repeat; suffix only collisions to keep collectors independent.
func (p *Provider) registerCollector(ctx *schemas.BifrostContext, state *collector) string {
	id := collectorID(ctx)
	if _, loaded := p.collectors.LoadOrStore(id, state); loaded {
		id += "/" + uuid.NewString()
		p.collectors.Store(id, state)
	}
	return id
}
