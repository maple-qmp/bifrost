package localaccount

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/providers/anthropic"
	"github.com/maximhq/bifrost/core/providers/openai"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

type testLogger struct{ schemas.Logger }

func (testLogger) Debug(string, ...any) {}
func (testLogger) Info(string, ...any)  {}
func (testLogger) Warn(string, ...any)  {}
func (testLogger) Error(string, ...any) {}

func testProvider(t testing.TB, kind schemas.ModelProvider, url string) *Provider {
	t.Helper()
	p, err := NewProvider(kind, &schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: url}, LocalAccountConfig: &schemas.LocalAccountConfig{DataDir: t.TempDir()}}, testLogger{})
	require.NoError(t, err)
	p.client = &fasthttp.Client{ReadTimeout: time.Second * 3}
	p.streamingClient = providerUtils.BuildStreamingClient(p.client)
	p.account.nextCheck = time.Now().Add(time.Hour)
	p.account.path = filepath.Join(t.TempDir(), "mock-account.json")
	p.account.current = &session{token: "test-access", identity: "test-account", expires: time.Now().Add(time.Hour).UnixMilli(), headers: map[string]string{"X-User-Id": "42"}}
	return p
}
func testContext(t *testing.T) *schemas.BifrostContext {
	t.Helper()
	ctx, cancel := schemas.NewBifrostContextWithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

const completedSSE = "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"test-model\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n"
const chatSSE = "data: {\"id\":\"chat_1\",\"model\":\"test-model\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\",\"reasoning_content\":\"thinking\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chat_1\",\"model\":\"test-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n"

func TestLocalAccountCodeBuddyToolStreamEmptyFinishReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i, fragment := range []string{`{`, `"command": `, `"echo hello"`, `, "description": "Print greeting"}`} {
			tool := schemas.ChatAssistantMessageToolCall{Index: 0, Function: schemas.ChatAssistantMessageToolCallFunction{Arguments: fragment}}
			if i == 0 {
				tool.ID = schemas.Ptr("chatcmpl-tool-test")
				tool.Function.Name = schemas.Ptr("Bash")
			}
			frame := &schemas.BifrostChatResponse{ID: "chat_test", Model: "step-5-preview", Choices: []schemas.BifrostResponseChoice{{
				FinishReason:             schemas.Ptr(""),
				ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{Delta: &schemas.ChatStreamResponseChoiceDelta{ToolCalls: []schemas.ChatAssistantMessageToolCall{tool}}},
			}}}
			data, err := json.Marshal(frame)
			if err != nil {
				t.Error(err)
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		fmt.Fprint(w, "data: {\"id\":\"chat_test\",\"model\":\"step-5-preview\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	p := testProvider(t, schemas.CodeBuddy, server.URL)
	ctx := testContext(t)
	r := &schemas.BifrostResponsesRequest{Provider: schemas.CodeBuddy, Model: "step-5-preview", Input: []schemas.ResponsesMessage{{
		Role: schemas.Ptr(schemas.ResponsesInputMessageRoleUser), Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr("Call Bash")},
	}}}
	ch, err := p.ResponsesStream(ctx, identityHook, nil, schemas.Key{}, r)
	require.Nil(t, err)
	var arguments strings.Builder
	open := map[int]bool{}
	starts, stops := 0, 0
	for chunk := range ch {
		require.Nil(t, chunk.BifrostError)
		if chunk.BifrostResponsesStreamResponse == nil {
			continue
		}
		for _, event := range anthropic.ToAnthropicResponsesStreamResponse(ctx, chunk.BifrostResponsesStreamResponse) {
			switch event.Type {
			case anthropic.AnthropicStreamEventTypeContentBlockStart:
				require.False(t, open[*event.Index], "duplicate block start")
				open[*event.Index] = true
				starts++
			case anthropic.AnthropicStreamEventTypeContentBlockDelta:
				require.True(t, open[*event.Index], "delta after block stop")
				if event.Delta != nil && event.Delta.PartialJSON != nil {
					arguments.WriteString(*event.Delta.PartialJSON)
				}
			case anthropic.AnthropicStreamEventTypeContentBlockStop:
				require.True(t, open[*event.Index], "duplicate block stop")
				open[*event.Index] = false
				stops++
			}
		}
	}
	require.Equal(t, 1, starts)
	require.Equal(t, 1, stops)
	require.JSONEq(t, `{"command":"echo hello","description":"Print greeting"}`, arguments.String())
}

func parallelToolParams() *schemas.ChatParameters {
	return &schemas.ChatParameters{Tools: []schemas.ChatTool{
		{Type: "function", Function: &schemas.ChatToolFunction{Name: "first"}},
		{Type: "function", Function: &schemas.ChatToolFunction{Name: "second"}},
	}}
}

func TestLocalAccountParallelToolStreams(t *testing.T) {
	for _, scenario := range []struct {
		kind   schemas.ModelProvider
		offset int
	}{{schemas.ChatGPT, 0}, {schemas.ChatGPT, 1}, {schemas.ChatGPT, 3}, {schemas.CodeBuddy, 0}} {
		kind, offset := scenario.kind, scenario.offset
		t.Run(fmt.Sprintf("%s/offset=%d", kind, offset), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if kind == schemas.ChatGPT {
					for i, name := range []string{"first", "second"} {
						fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":%d,\"item\":{\"type\":\"function_call\",\"id\":\"fc_%d\",\"call_id\":\"call_%d\",\"name\":%q,\"arguments\":\"\"}}\n\n", i+offset, i, i, name)
					}
					for _, fragment := range []string{`{"value":`, `1}`} {
						for _, i := range []int{1, 0} {
							part := fragment
							if fragment == "1}" {
								part = fmt.Sprintf("%d}", i+1)
							}
							fmt.Fprintf(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":%d,\"item_id\":\"fc_%d\",\"delta\":%q}\n\n", i+offset, i, part)
						}
					}
					fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_parallel\",\"model\":\"test-model\",\"status\":\"completed\",\"output\":[]}}\n\n")
				} else {
					fmt.Fprint(w, "data: "+`{"id":"chat_parallel","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_0","function":{"name":"first","arguments":"{\"value\":"}},{"index":1,"id":"call_1","function":{"name":"second","arguments":"{\"value\":"}}]},"finish_reason":""}]}`+"\n\n")
					fmt.Fprint(w, "data: "+`{"id":"chat_parallel","model":"test-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}},{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":"tool_calls"}]}`+"\n\ndata: [DONE]\n\n")
				}
			}))
			defer server.Close()
			p := testProvider(t, kind, server.URL)
			ctx := testContext(t)
			request := &schemas.BifrostChatRequest{Provider: kind, Model: "test-model", Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("call both tools")}}}, Params: parallelToolParams()}
			var ch chan *schemas.BifrostStreamChunk
			var err *schemas.BifrostError
			if kind == schemas.ChatGPT {
				ch, err = p.ChatCompletionStream(ctx, identityHook, nil, schemas.Key{}, request)
			} else {
				ch, err = p.ResponsesStream(ctx, identityHook, nil, schemas.Key{}, request.ToResponsesRequest())
			}
			require.Nil(t, err)
			names, args, ids := map[int]string{}, map[int]string{}, map[int]string{}
			open := map[int]bool{}
			for chunk := range ch {
				require.Nil(t, chunk.BifrostError)
				if kind == schemas.ChatGPT {
					if chunk.BifrostChatResponse == nil {
						continue
					}
					for _, choice := range chunk.BifrostChatResponse.Choices {
						if choice.ChatStreamResponseChoice == nil || choice.Delta == nil {
							continue
						}
						for _, tool := range choice.Delta.ToolCalls {
							index := int(tool.Index)
							if tool.ID != nil {
								require.Empty(t, ids[index], "two tools share an index")
								ids[index] = *tool.ID
							}
							if tool.Function.Name != nil {
								names[index] = *tool.Function.Name
							}
							args[index] += tool.Function.Arguments
						}
					}
				} else if chunk.BifrostResponsesStreamResponse != nil {
					for _, event := range anthropic.ToAnthropicResponsesStreamResponse(ctx, chunk.BifrostResponsesStreamResponse) {
						if event.Index == nil {
							continue
						}
						index := *event.Index
						switch event.Type {
						case anthropic.AnthropicStreamEventTypeContentBlockStart:
							require.False(t, open[index])
							open[index] = true
							ids[index] = *event.ContentBlock.ID
							names[index] = *event.ContentBlock.Name
						case anthropic.AnthropicStreamEventTypeContentBlockDelta:
							require.True(t, open[index], "delta after stop")
							if event.Delta.PartialJSON != nil {
								args[index] += *event.Delta.PartialJSON
							}
						case anthropic.AnthropicStreamEventTypeContentBlockStop:
							require.True(t, open[index])
							open[index] = false
						}
					}
				}
			}
			p.collectors.Range(func(_, _ any) bool { t.Error("stream index state leaked after completion"); return true })
			require.Len(t, ids, 2)
			require.ElementsMatch(t, []string{"first", "second"}, []string{names[0], names[1]})
			for index := range ids {
				require.JSONEq(t, fmt.Sprintf(`{"value":%d}`, index+1), args[index])
				require.False(t, open[index])
			}
		})
	}
}

func BenchmarkLocalAccountChatToolIndexStream(b *testing.B) {
	for _, tools := range []bool{false, true} {
		b.Run(fmt.Sprintf("tools=%v", tools), func(b *testing.B) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if tools {
					for i := 0; i < 2; i++ {
						fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":%d,\"item\":{\"type\":\"function_call\",\"id\":\"fc_%d\",\"call_id\":\"call_%d\",\"name\":\"lookup\",\"arguments\":\"\"}}\n\n", i+1, i, i)
						fmt.Fprintf(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":%d,\"item_id\":\"fc_%d\",\"delta\":\"{}\"}\n\n", i+1, i)
					}
				}
				fmt.Fprint(w, completedSSE)
			}))
			defer server.Close()
			p := testProvider(b, schemas.ChatGPT, server.URL)
			request := &schemas.BifrostChatRequest{Provider: schemas.ChatGPT, Model: "test", Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hi")}}}}
			if tools {
				request.Params = parallelToolParams()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
				ch, err := p.ChatCompletionStream(ctx, identityHook, nil, schemas.Key{}, request)
				if err != nil {
					cancel()
					b.Fatal(err)
				}
				for chunk := range ch {
					if chunk.BifrostError != nil {
						cancel()
						b.Fatal(chunk.BifrostError)
					}
				}
				cancel()
			}
		})
	}
}

func TestLocalAccountChatGPTToolIndexStateCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"first","arguments":""}}`+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	p := testProvider(t, schemas.ChatGPT, server.URL)
	ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
	defer cancel()
	r := &schemas.BifrostChatRequest{Provider: schemas.ChatGPT, Model: "test", Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("use a tool")}}}, Params: parallelToolParams()}
	ch, err := p.ChatCompletionStream(ctx, identityHook, nil, schemas.Key{}, r)
	require.Nil(t, err)
	seen := false
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				require.True(t, seen)
				p.collectors.Range(func(_, _ any) bool { t.Error("stream index state leaked after cancellation"); return true })
				return
			}
			if chunk.BifrostChatResponse == nil {
				continue
			}
			for _, choice := range chunk.BifrostChatResponse.Choices {
				if choice.ChatStreamResponseChoice != nil && choice.Delta != nil && len(choice.Delta.ToolCalls) > 0 {
					seen = true
					cancel()
				}
			}
		case <-timeout.C:
			t.Fatal("cancelled tool stream did not close")
		}
	}
}

func TestLocalAccountChatGPTWire(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/responses", r.URL.Path)
		require.Equal(t, "Bearer test-access", r.Header.Get("Authorization"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, completedSSE)
	}))
	defer server.Close()
	p := testProvider(t, schemas.ChatGPT, server.URL)
	var req schemas.BifrostChatRequest
	require.NoError(t, json.Unmarshal([]byte(`{"provider":"chatgpt","model":"test-model","input":[{"role":"user","content":"hi"}],"params":{"temperature":0.5,"max_completion_tokens":10,"extra_params":{"metadata":{"test":true},"store":true}}}`), &req))
	out, e := p.ChatCompletion(testContext(t), schemas.Key{}, &req)
	require.Nil(t, e)
	require.NotNil(t, out)
	require.True(t, received["stream"].(bool))
	require.Equal(t, false, received["store"])
	require.NotContains(t, received, "temperature")
	require.NotContains(t, received, "max_output_tokens")
	require.NotContains(t, received, "metadata")
	require.Equal(t, "hello", *out.Choices[0].Message.Content.ContentStr)
	require.Equal(t, 5, out.Usage.TotalTokens)
	n := 0
	p.collectors.Range(func(_, _ any) bool { n++; return true })
	require.Zero(t, n)
}

func TestLocalAccountCodeBuddyWire(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/chat/completions", r.URL.Path)
		require.Equal(t, "42", r.Header.Get("X-User-Id"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, chatSSE)
	}))
	defer server.Close()
	p := testProvider(t, schemas.CodeBuddy, server.URL)
	var req schemas.BifrostChatRequest
	require.NoError(t, json.Unmarshal([]byte(`{"provider":"codebuddy","model":"test-model","input":[{"role":"system","content":"You are Claude Code, Anthropic's official CLI for Claude."},{"role":"user","content":"Claude Code"}]}`), &req))
	out, e := p.ChatCompletion(testContext(t), schemas.Key{}, &req)
	require.Nil(t, e)
	require.Equal(t, "hello", *out.Choices[0].Message.Content.ContentStr)
	require.Equal(t, "thinking", *out.Choices[0].Message.Reasoning)
	require.Equal(t, 5, out.Usage.TotalTokens)
	require.True(t, received["stream"].(bool))
	messages := received["messages"].([]any)
	require.Contains(t, messages[0].(map[string]any)["content"], "interactive software engineering assistant")
	require.Equal(t, "Claude Code", messages[1].(map[string]any)["content"])
	require.Equal(t, "You are Claude Code, Anthropic's official CLI for Claude.", *req.Input[0].Content.ContentStr)
	out2, e := p.Responses(testContext(t), schemas.Key{}, req.ToResponsesRequest())
	require.Nil(t, e)
	require.NotEmpty(t, out2.Output)
}

func TestLocalAccountStreamingConversion(t *testing.T) {
	for _, kind := range []schemas.ModelProvider{schemas.ChatGPT, schemas.CodeBuddy} {
		t.Run(string(kind), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if kind == schemas.ChatGPT {
					fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\",\"output_index\":0,\"content_index\":0}\n\n"+completedSSE)
				} else {
					fmt.Fprint(w, chatSSE)
				}
			}))
			defer server.Close()
			p := testProvider(t, kind, server.URL)
			var req schemas.BifrostChatRequest
			require.NoError(t, json.Unmarshal([]byte(`{"model":"test-model","input":[{"role":"user","content":"hi"}]}`), &req))
			req.Provider = kind
			calls := 0
			hook := func(_ *schemas.BifrostContext, r *schemas.BifrostResponse, e *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
				calls++
				return r, e
			}
			ch, e := p.ChatCompletionStream(testContext(t), hook, nil, schemas.Key{}, &req)
			require.Nil(t, e)
			text := ""
			var streamID string
			for c := range ch {
				require.Nil(t, c.BifrostError)
				if c.BifrostChatResponse == nil {
					continue
				}
				if streamID == "" {
					streamID = c.BifrostChatResponse.ID
				}
				require.NotEmpty(t, c.BifrostChatResponse.ID)
				require.Equal(t, streamID, c.BifrostChatResponse.ID)
				require.Equal(t, "test-model", c.BifrostChatResponse.Model)
				for _, choice := range c.BifrostChatResponse.Choices {
					if choice.ChatStreamResponseChoice != nil && choice.Delta != nil && choice.Delta.Content != nil {
						text += *choice.Delta.Content
					}
				}
			}
			require.Equal(t, "hello", text)
			require.Positive(t, calls)
			ch, e = p.ResponsesStream(testContext(t), identityHook, nil, schemas.Key{}, req.ToResponsesRequest())
			require.Nil(t, e)
			completed := false
			for c := range ch {
				require.Nil(t, c.BifrostError)
				if c.BifrostResponsesStreamResponse != nil && c.BifrostResponsesStreamResponse.Type == schemas.ResponsesStreamResponseTypeCompleted {
					completed = true
				}
			}
			require.True(t, completed)
		})
	}
}

func TestLocalAccountErrorAndTruncation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{{"quota", 429, `{"error":{"code":"subscription_sharing_usage_limit_exceeded","message":"limit"}}`}, {"revoked", 401, `{"detail":"revoked"}`}, {"truncated", 200, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"}} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			p := testProvider(t, schemas.ChatGPT, server.URL)
			_, e := p.Responses(testContext(t), schemas.Key{}, &schemas.BifrostResponsesRequest{Model: "test", Input: []schemas.ResponsesMessage{}})
			require.NotNil(t, e)
			if tc.status != 200 {
				require.Equal(t, tc.status, *e.StatusCode)
				require.NotNil(t, e.AllowFallbacks)
				require.False(t, *e.AllowFallbacks)
			}
			count := 0
			p.collectors.Range(func(_, _ any) bool { count++; return true })
			require.Zero(t, count)
		})
	}
}

func TestLocalAccountModelCache(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"data":[{"id":"model-a"}]}`)
	}))
	defer server.Close()
	p := testProvider(t, schemas.ChatGPT, server.URL)
	for i := 0; i < 2; i++ {
		out, e := p.ListModels(testContext(t), nil, nil)
		require.Nil(t, e)
		require.Equal(t, "chatgpt/model-a", out.Data[0].ID)
		out.Data[0].ID = "mutated"
	}
	require.EqualValues(t, 1, calls.Load())
	p.InvalidateModels()
	_, e := p.ListModels(testContext(t), nil, nil)
	require.Nil(t, e)
	require.EqualValues(t, 2, calls.Load())
}

func TestLocalAccountCollectorToolsAndLimit(t *testing.T) {
	c := newCollector()
	for _, raw := range []string{`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"x\":"}}]}}]}`, `{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":"tool_calls"}]}`} {
		var r schemas.BifrostChatResponse
		require.NoError(t, json.Unmarshal([]byte(raw), &r))
		require.NoError(t, c.addChat(&r))
	}
	out, e := c.chat()
	require.Nil(t, e)
	require.Equal(t, `{"x":1}`, out.Choices[0].Message.ToolCalls[0].Function.Arguments)
	c.bytes = maxAggregateBytes
	require.Error(t, c.addChat(&schemas.BifrostChatResponse{}))
}

func TestLocalAccountCompatibility(t *testing.T) {
	s := "x-anthropic-billing-header: v1\nClaude Code malware DoS"
	got := rewriteSystem(s)
	require.NotContains(t, got, "billing-header")
	require.NotContains(t, got, "Claude Code")
	require.Equal(t, got, rewriteSystem(got))
	p := &Provider{compat: false}
	r := &openai.OpenAIChatRequest{Messages: []openai.OpenAIMessage{{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr(s)}}}}
	require.Equal(t, s, *p.chatRequest(r).Messages[0].Content.ContentStr)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func jsonResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestLocalAccountRefreshCoalesces(t *testing.T) {
	root := t.TempDir()
	m, err := newManager(schemas.ChatGPT, schemas.LocalAccountConfig{DataDir: root}, nil)
	require.NoError(t, err)
	require.NoError(t, atomicJSON(m.path, credential{Access: "expired", Refresh: "refresh", ClientID: "client", Identity: "identity", Expires: 0, Scopes: []string{directScope}}))
	var calls atomic.Int32
	m.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		require.Equal(t, authOrigin+"/api/accounts/oauth/token", r.URL.String())
		return jsonResponse(`{"access_token":"new","refresh_token":"rotated","expires_in":3600,"scope":"chatgpt.tokens.use.direct"}`), nil
	})}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := m.authorize(context.Background())
			if err != nil || s.token != "new" {
				t.Error("refresh failed", err)
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, calls.Load())
	info, err := os.Stat(m.path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	statusBytes, err := schemas.MarshalSorted(m.Status())
	require.NoError(t, err)
	require.NotContains(t, string(statusBytes), "rotated")
	require.NotContains(t, string(statusBytes), "new")
	require.NoError(t, m.Disconnect())
	_, err = m.authorize(context.Background())
	require.Error(t, err)
	require.NoFileExists(t, m.path)
}

func TestLocalAccountCodeBuddyRefreshConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.info")
	require.NoError(t, atomicJSON(path, map[string]any{"auth": map[string]any{"accessToken": "old", "refreshToken": "refresh", "expiresAt": 0}, "account": map[string]any{"uid": "42", "enterpriseId": "tenant"}, "unknown": "preserve"}))
	m, err := newManager(schemas.CodeBuddy, schemas.LocalAccountConfig{DataDir: t.TempDir(), CredentialFile: path}, &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		require.NoError(t, atomicJSON(path, map[string]any{"auth": map[string]any{"accessToken": "desktop-new", "expiresAt": time.Now().Add(time.Hour).UnixMilli()}, "account": map[string]any{"uid": "42"}}))
		return jsonResponse(`{"code":0,"data":{"accessToken":"refresh-new","expiresIn":3600}}`), nil
	})})
	require.NoError(t, err)
	_, err = m.authorize(context.Background())
	require.ErrorContains(t, err, "changed during refresh")
	_, a, _, err := buddyDocument(path)
	require.NoError(t, err)
	require.Equal(t, "desktop-new", a.Access)
	require.NoError(t, m.Disconnect())
	require.FileExists(t, path)
}

func BenchmarkLocalAccountCredentialHit(b *testing.B) {
	m := &Manager{current: &session{expires: time.Now().Add(time.Hour).UnixMilli()}}
	b.ReportAllocs()
	for b.Loop() {
		_, _ = m.authorize(context.Background())
	}
}
func BenchmarkLocalAccountCompatibility(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		b.Run(fmt.Sprint(enabled), func(b *testing.B) {
			p := &Provider{compat: enabled}
			r := &openai.OpenAIChatRequest{Messages: []openai.OpenAIMessage{{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("You are a helpful assistant.")}}}}
			b.ReportAllocs()
			for b.Loop() {
				copy := *r
				p.chatRequest(&copy)
			}
		})
	}
}
func BenchmarkLocalAccountRequestConversion(b *testing.B) {
	r := &schemas.BifrostChatRequest{Model: "test", Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hello")}}}}
	b.ReportAllocs()
	for b.Loop() {
		_ = r.ToResponsesRequest()
	}
}
func BenchmarkLocalAccountAggregation(b *testing.B) {
	var r schemas.BifrostChatResponse
	_ = json.Unmarshal([]byte(`{"choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":"stop"}]}`), &r)
	b.ReportAllocs()
	for b.Loop() {
		c := newCollector()
		_ = c.addChat(&r)
		_, _ = c.chat()
	}
}

func TestLocalAccountCancellationCleansCollector(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	defer server.Close()
	p := testProvider(t, schemas.ChatGPT, server.URL)
	ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
	defer cancel()
	done := make(chan *schemas.BifrostError, 1)
	go func() {
		_, e := p.Responses(ctx, schemas.Key{}, &schemas.BifrostResponsesRequest{Model: "test", Input: []schemas.ResponsesMessage{}})
		done <- e
	}()
	<-started
	cancel()
	select {
	case e := <-done:
		require.NotNil(t, e)
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not stop unary stream collection")
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not close upstream connection")
	}
	count := 0
	p.collectors.Range(func(_, _ any) bool { count++; return true })
	require.Zero(t, count)
}

func TestLocalAccountImagesAndToolHistory(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, chatSSE)
	}))
	defer server.Close()
	p := testProvider(t, schemas.CodeBuddy, server.URL)
	var req schemas.BifrostChatRequest
	require.NoError(t, json.Unmarshal([]byte(`{"provider":"codebuddy","model":"test-model","input":[{"role":"user","content":[{"type":"text","text":"Describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw=="}}]},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"tool result"}]}`), &req))
	_, e := p.ChatCompletion(testContext(t), schemas.Key{}, &req)
	require.Nil(t, e)
	messages := received["messages"].([]any)
	require.Len(t, messages, 3)
	require.Equal(t, "image_url", messages[0].(map[string]any)["content"].([]any)[1].(map[string]any)["type"])
	require.Equal(t, "call_1", messages[2].(map[string]any)["tool_call_id"])
}

func TestLocalAccountPlanWireRestrictions(t *testing.T) {
	p := &Provider{}
	var r openai.OpenAIResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"test","input":[{"type":"message","role":"system","content":"instructions"},{"role":"user","content":"hi"}],"background":true,"top_p":0.7,"user":"user","truncation":"auto","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}},{"type":"tool_search"}]}`), &r))
	got := p.responseRequest(&r)
	raw, err := schemas.MarshalSorted(got)
	require.NoError(t, err)
	require.NotContains(t, string(raw), `"background"`)
	require.NotContains(t, string(raw), `"top_p"`)
	require.NotContains(t, string(raw), `"role":"system"`)
	require.Contains(t, string(raw), `"role":"developer"`)
	require.Contains(t, string(raw), `"type":"additional_tools"`)
	var wire struct {
		Input []struct {
			Type  string                  `json:"type"`
			Role  string                  `json:"role"`
			Tools []schemas.ResponsesTool `json:"tools"`
		} `json:"input"`
	}
	require.NoError(t, json.Unmarshal(raw, &wire))
	require.Equal(t, "additional_tools", wire.Input[0].Type)
	require.Equal(t, "developer", wire.Input[0].Role, "additional_tools requires an explicit developer role")
	require.Equal(t, "lookup", *wire.Input[0].Tools[0].Name)
	require.NotContains(t, string(raw), `"type":"tool_search"`)
	require.Empty(t, got.Tools)
}

func BenchmarkLocalAccountPlanWireConversion(b *testing.B) {
	p := &Provider{}
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	r := &schemas.BifrostChatRequest{Provider: schemas.ChatGPT, Model: "test", Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hello")}}}}
	b.ReportAllocs()
	for b.Loop() {
		p.responseRequest(openai.ToOpenAIResponsesRequest(ctx, r.ToResponsesRequest()))
	}
}

func TestLocalAccountChatGPTToolRoleWire(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", protocol, stream), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body struct {
						Input []struct {
							Type string `json:"type"`
							Role string `json:"role"`
						} `json:"input"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					if len(body.Input) == 0 || body.Input[0].Type != "additional_tools" || body.Input[0].Role != "developer" {
						w.WriteHeader(400)
						fmt.Fprint(w, `{"error":{"type":"invalid_request_error","message":"Missing required parameter: 'input[0].role'."}}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, completedSSE)
				}))
				defer server.Close()
				p := testProvider(t, schemas.ChatGPT, server.URL)
				ctx := testContext(t)
				var chat schemas.BifrostChatRequest
				require.NoError(t, json.Unmarshal([]byte(`{"provider":"chatgpt","model":"test","input":[{"role":"system","content":"instructions"},{"role":"user","content":"hi"}],"params":{"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{}}}}]}}`), &chat))
				responses := chat.ToResponsesRequest()
				if protocol == "messages" {
					var a anthropic.AnthropicMessageRequest
					require.NoError(t, json.Unmarshal([]byte(`{"model":"chatgpt/test","system":"instructions","messages":[{"role":"user","content":"hi"}],"max_tokens":32,"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{}}}]}`), &a))
					responses = a.ToBifrostResponsesRequest(ctx)
				}
				var ch chan *schemas.BifrostStreamChunk
				var e *schemas.BifrostError
				if protocol == "chat" {
					if stream {
						ch, e = p.ChatCompletionStream(ctx, nil, nil, schemas.Key{}, &chat)
					} else {
						_, e = p.ChatCompletion(ctx, schemas.Key{}, &chat)
					}
				} else if stream {
					ch, e = p.ResponsesStream(ctx, nil, nil, schemas.Key{}, responses)
				} else {
					_, e = p.Responses(ctx, schemas.Key{}, responses)
				}
				require.Nil(t, e)
				if stream {
					for chunk := range ch {
						require.Nil(t, chunk.BifrostError)
					}
				}
			})
		}
	}
}

func BenchmarkLocalAccountPlanToolWireConversion(b *testing.B) {
	p := &Provider{}
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	var r schemas.BifrostResponsesRequest
	if err := json.Unmarshal([]byte(`{"model":"test","input":[{"role":"user","content":"hello"}],"params":{"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}]}}`), &r); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		p.responseRequest(openai.ToOpenAIResponsesRequest(ctx, &r))
	}
}

func TestLocalAccountFractionalDesktopExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desktop.info")
	require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf(`{"auth":{"accessToken":"desktop-token","expiresAt":%d.5},"account":{"uid":"42"}}`, time.Now().Add(time.Hour).UnixMilli())), 0600))
	m, err := newManager(schemas.CodeBuddy, schemas.LocalAccountConfig{DataDir: t.TempDir(), CredentialFile: path}, nil)
	require.NoError(t, err)
	s, err := m.authorize(context.Background())
	require.NoError(t, err)
	require.Equal(t, "desktop-token", s.token)
}

func TestLocalAccountReasoningFragmentsCoalesce(t *testing.T) {
	c := newCollector()
	for _, raw := range []string{`{"choices":[{"index":0,"delta":{"reasoning_content":"first"}}]}`, `{"choices":[{"index":0,"delta":{"reasoning_content":"second"},"finish_reason":"stop"}]}`} {
		var r schemas.BifrostChatResponse
		require.NoError(t, json.Unmarshal([]byte(raw), &r))
		require.NoError(t, c.addChat(&r))
	}
	out, e := c.chat()
	require.Nil(t, e)
	require.Equal(t, "firstsecond", *out.Choices[0].Message.Reasoning)
	require.Len(t, out.Choices[0].Message.ReasoningDetails, 1)
	require.Equal(t, "firstsecond", *out.Choices[0].Message.ReasoningDetails[0].Text)
}

func TestLocalAccountRepeatedRequestIDs(t *testing.T) {
	p := &Provider{}
	ctx := testContext(t)
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "repeated")
	first, second := p.registerCollector(ctx, newCollector()), p.registerCollector(ctx, newCollector())
	require.NotEqual(t, first, second)
	p.collectors.Delete(first)
	_, found := p.collectors.Load(second)
	require.True(t, found)
	p.collectors.Delete(second)
}

func TestLocalAccountDesktopRefreshPreservesFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "desktop.info")
	require.NoError(t, atomicJSON(path, map[string]any{"auth": map[string]any{"accessToken": "old", "refreshToken": "old-refresh", "expiresAt": 0, "customAuthField": "retain"}, "account": map[string]any{"uid": "42", "enterpriseId": "tenant"}, "customField": "retain"}))
	var calls atomic.Int32
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		require.Equal(t, "old-refresh", r.Header.Get("X-Refresh-Token"))
		require.Equal(t, "42", r.Header.Get("X-User-Id"))
		require.Equal(t, "CLI/2.63.2 CodeBuddy/2.63.2", r.Header.Get("User-Agent"))
		return jsonResponse(`{"code":0,"data":{"accessToken":"new","refreshToken":"rotated","expiresIn":3600.5,"domain":"www.codebuddy.cn"}}`), nil
	})}
	m, err := newManager(schemas.CodeBuddy, schemas.LocalAccountConfig{DataDir: t.TempDir(), CredentialFile: path}, hc)
	require.NoError(t, err)
	session, err := m.authorize(context.Background())
	require.NoError(t, err)
	require.Equal(t, "new", session.token)
	require.Equal(t, "CLI/2.63.2 CodeBuddy/2.63.2", session.headers["User-Agent"])
	var document map[string]any
	require.NoError(t, readObject(path, &document))
	require.Equal(t, "retain", document["customField"])
	auth := document["auth"].(map[string]any)
	require.Equal(t, "retain", auth["customAuthField"])
	require.Equal(t, "rotated", auth["refreshToken"])
	_, err = m.authorize(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 1, calls.Load())
}

func TestLocalAccountDeepSeekHarnessCompatibility(t *testing.T) {
	const generic = "You are an interactive software engineering assistant operating in a command-line environment."
	const identity = "You are an AI agent powered by DeepSeek Harness."
	for _, tc := range []struct {
		name, system, want string
		enabled, blocks    bool
	}{
		{"identity", identity, generic, true, false},
		{"multiline", identity + "\nKeep these instructions.", generic + "\nKeep these instructions.", true, false},
		{"blocks", identity + "\nKeep these instructions.", generic + "\nKeep these instructions.", true, true},
		{"variant", "You are a coding agent powered by DeepSeek Harness (CLI).", generic, true, false},
		{"case insensitive", "You are an AI agent powered by deepseek harness.", generic, true, false},
		{"billing header", "x-anthropic-billing-header: test\n" + identity + "\nKeep these instructions.", generic + "\nKeep these instructions.", true, false},
		{"not first line", "Keep these instructions.\n" + identity, "Keep these instructions.\n" + identity, true, false},
		{"mention only", "Documentation mentions DeepSeek Harness.", "Documentation mentions DeepSeek Harness.", true, false},
		{"disabled", identity, identity, false, false},
		{"ordinary", "Original system", "Original system", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Provider{compat: tc.enabled}
			content := &schemas.ChatMessageContent{ContentStr: schemas.Ptr(tc.system)}
			if tc.blocks {
				content = &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{{Type: schemas.ChatContentBlockTypeText, Text: schemas.Ptr(tc.system)}}}
			}
			r := &openai.OpenAIChatRequest{Model: "any-codebuddy-model", Messages: []openai.OpenAIMessage{{Role: schemas.ChatMessageRoleSystem, Content: content}, {Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr(identity)}}, {Role: schemas.ChatMessageRoleAssistant, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr(identity)}}, {Role: schemas.ChatMessageRoleTool, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr(identity)}}}}
			before, err := json.Marshal(r)
			require.NoError(t, err)
			copy := *r
			out := p.chatRequest(&copy)
			text := out.Messages[0].Content.ContentStr
			if tc.blocks {
				text = out.Messages[0].Content.ContentBlocks[0].Text
			}
			require.Equal(t, tc.want, *text)
			for _, m := range out.Messages[1:] {
				require.Equal(t, identity, *m.Content.ContentStr)
			}
			after, err := json.Marshal(r)
			require.NoError(t, err)
			require.JSONEq(t, string(before), string(after), "caller-owned history changed")
		})
	}
}

func BenchmarkLocalAccountDeepSeekHarnessCompatibility(b *testing.B) {
	p := &Provider{compat: true}
	r := &openai.OpenAIChatRequest{Model: "any-codebuddy-model", Messages: []openai.OpenAIMessage{{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("You are an AI agent powered by DeepSeek Harness.\nKeep these instructions.")}}}}
	b.ReportAllocs()
	for b.Loop() {
		copy := *r
		p.chatRequest(&copy)
	}
}

func TestLocalAccountDeepSeekHarnessOnlyFirstSystemLine(t *testing.T) {
	identity := "You are an AI agent powered by DeepSeek Harness."
	r := &openai.OpenAIChatRequest{Messages: []openai.OpenAIMessage{
		{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{{Type: schemas.ChatContentBlockTypeText, Text: schemas.Ptr("Original first line")}, {Type: schemas.ChatContentBlockTypeText, Text: &identity}}}},
		{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: &identity}},
	}}
	before, err := json.Marshal(r)
	require.NoError(t, err)
	out, err := json.Marshal((&Provider{compat: true}).chatRequest(r))
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(out))
}

func TestLocalAccountDeepSeekHarnessWire(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", protocol, stream), func(t *testing.T) {
				var captured string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body openai.OpenAIChatRequest
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					captured = *body.Messages[0].Content.ContentStr
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, chatSSE)
				}))
				defer server.Close()
				p := testProvider(t, schemas.CodeBuddy, server.URL)
				ctx := testContext(t)
				r := &schemas.BifrostChatRequest{Provider: schemas.CodeBuddy, Model: "any-model", Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("You are an AI agent powered by DeepSeek Harness.\nOriginal instructions.")}}, {Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("hi")}}}}
				responses := r.ToResponsesRequest()
				if protocol == "messages" {
					var a anthropic.AnthropicMessageRequest
					require.NoError(t, json.Unmarshal([]byte(`{"model":"codebuddy/any-model","system":"You are an AI agent powered by DeepSeek Harness.\nOriginal instructions.","messages":[{"role":"user","content":"hi"}],"max_tokens":32}`), &a))
					responses = a.ToBifrostResponsesRequest(ctx)
				}
				var ch chan *schemas.BifrostStreamChunk
				var e *schemas.BifrostError
				if protocol == "chat" {
					if stream {
						ch, e = p.ChatCompletionStream(ctx, nil, nil, schemas.Key{}, r)
					} else {
						_, e = p.ChatCompletion(ctx, schemas.Key{}, r)
					}
				} else if stream {
					ch, e = p.ResponsesStream(ctx, nil, nil, schemas.Key{}, responses)
				} else {
					_, e = p.Responses(ctx, schemas.Key{}, responses)
				}
				require.Nil(t, e)
				if stream {
					for chunk := range ch {
						require.Nil(t, chunk.BifrostError)
					}
				}
				require.Equal(t, "You are an interactive software engineering assistant operating in a command-line environment.\nOriginal instructions.", captured)
			})
		}
	}
}
