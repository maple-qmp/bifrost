package localaccount

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/maximhq/bifrost/core/providers/openai"
	"github.com/maximhq/bifrost/core/schemas"
)

var compatibilityTerms = []string{"supply chain compromise", "supply-chain compromise", "remote code execution", "noreply@anthropic.com", "privilege escalation", "credential stuffing", "command and control", "credential testing", "malicious purposes", "detection evasion", "malicious intent", "mass targeting", "Co-Authored-By", "agent-identity", "C2 frameworks", "reverse shell", "SQL injection", "Claude Sonnet", "C2 framework", "Claude Haiku", "Claude Fable", "OhMyOpenCode", "brute force", "brute-force", "Claude Code", "Claude Opus", "ransomware", "keylogger", "Anthropic", "DeepSeek", "phishing", "backdoor", "zero-day", "OpenCode", "exploit", "malware", "rootkit", "botnet", "Claude", "OpenAI", "Gemini", "Cursor", "DDoS", "CSRF", "0day", "Kimi", "Qwen", "DoS", "XSS"}
var fingerprints = [][2]string{{"You are Claude Code, Anthropic's official CLI for Claude.", "You are an interactive software engineering assistant operating in a command-line environment."}, {"You are Claude Code, Anthropic's official CLI", "You are an interactive software engineering assistant operating in a command-line environment"}, {"Main branch (you will usually use this for PRs)", "Main branch"}}
var compatibilityPattern = func() *regexp.Regexp {
	parts := make([]string, len(compatibilityTerms))
	for i, t := range compatibilityTerms {
		parts[i] = regexp.QuoteMeta(t)
	}
	return regexp.MustCompile(`(?i)\b(?:` + strings.Join(parts, "|") + `)\b`)
}()

const deepSeekHarnessGUIOpening = "You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names."
const deepSeekHarnessOpening = "You are an AI agent powered by DeepSeek Harness."
const guiDoubleQuotedTerms = `"this page", "this GUI", or "this app"`
const guiSingleQuotedTerms = `'this page', 'this GUI', or 'this app'`

func removeDeepSeekHarnessOpening(s string) string {
	if rest, ok := strings.CutPrefix(s, deepSeekHarnessOpening+"\r\n"); ok && rest != "" {
		return strings.TrimPrefix(rest, "\r\n")
	}
	if rest, ok := strings.CutPrefix(s, deepSeekHarnessOpening+"\n"); ok && rest != "" {
		return strings.TrimPrefix(rest, "\n")
	}
	return s
}

func rewriteDeepSeekHarnessGUIQuotes(s string) string {
	if !strings.Contains(s, guiDoubleQuotedTerms) {
		return s
	}
	for start := 0; start < len(s); {
		index := strings.Index(s[start:], deepSeekHarnessGUIOpening)
		if index < 0 {
			return s
		}
		index += start
		if index == 0 || s[index-1] == '\n' {
			return strings.ReplaceAll(s, guiDoubleQuotedTerms, guiSingleQuotedTerms)
		}
		start = index + len(deepSeekHarnessGUIOpening)
	}
	return s
}

func rewriteSystem(s string) string {
	for _, p := range fingerprints {
		s = strings.ReplaceAll(s, p[0], p[1])
	}
	if strings.HasPrefix(s, "x-anthropic-billing-header:") {
		_, s, _ = strings.Cut(s, "\n")
	}
	s = removeDeepSeekHarnessOpening(s)
	s = rewriteDeepSeekHarnessGUIQuotes(s)
	indexes := compatibilityPattern.FindAllStringIndex(s, -1)
	if len(indexes) == 0 {
		return s
	}
	var out strings.Builder
	last := 0
	for _, idx := range indexes {
		if idx[0] > 0 {
			r, _ := utf8.DecodeLastRuneInString(s[:idx[0]])
			if r == '\u200b' {
				continue
			}
		}
		out.WriteString(s[last : idx[0]+1])
		out.WriteRune('\u200b')
		last = idx[0] + 1
	}
	if last == 0 {
		return s
	}
	out.WriteString(s[last:])
	return out.String()
}

func (p *Provider) chatRequest(r *openai.OpenAIChatRequest) *openai.OpenAIChatRequest {
	// Converters own their output; cloning content here also protects caller-owned history.
	if p.compat {
		copied := false
		for i, m := range r.Messages {
			if m.Role != schemas.ChatMessageRoleSystem || m.Content == nil {
				continue
			}
			content := *m.Content
			changed := false
			if content.ContentStr != nil {
				text := rewriteSystem(*content.ContentStr)
				if text != *content.ContentStr {
					content.ContentStr = schemas.Ptr(text)
					changed = true
				}
			} else {
				for j, block := range content.ContentBlocks {
					if block.Type != schemas.ChatContentBlockTypeText || block.Text == nil {
						continue
					}
					text := rewriteSystem(*block.Text)
					if text == *block.Text {
						continue
					}
					if !changed {
						content.ContentBlocks = append([]schemas.ChatContentBlock(nil), content.ContentBlocks...)
						changed = true
					}
					content.ContentBlocks[j].Text = schemas.Ptr(text)
				}
			}
			if changed {
				if !copied {
					r.Messages = append([]openai.OpenAIMessage(nil), r.Messages...)
					copied = true
				}
				r.Messages[i].Content = &schemas.ChatMessageContent{ContentStr: content.ContentStr, ContentBlocks: content.ContentBlocks}
			}
		}
	}
	// Deferred functions are sent eagerly; builtin search tools are unsupported upstream.
	r.Tools = append([]schemas.ChatTool(nil), r.Tools...)
	tools := r.Tools[:0]
	for _, tool := range r.Tools {
		if strings.HasPrefix(string(tool.Type), "tool_search") {
			continue
		}
		if tool.Function != nil && strings.HasPrefix(tool.Function.Name, "tool_search_tool_") {
			continue
		}
		tool.DeferLoading = nil
		tools = append(tools, tool)
	}
	if len(r.Tools) > 0 && len(tools) == 0 {
		r.ToolChoice = nil
	}
	r.Tools = tools
	return r
}
