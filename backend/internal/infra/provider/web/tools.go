package web

import (
	"encoding/json"
	"fmt"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/toolemu"
)

// parseToolConfiguration 只接受函数工具（提示词模拟）与 Grok Web 原生执行的 web_search。
func parseToolConfiguration(rawTools, rawChoice json.RawMessage) (toolemu.Config, error) {
	configuration, err := toolemu.ParseConfig(rawTools, rawChoice)
	if err == nil && len(configuration.Unsupported) > 0 {
		return toolemu.Config{}, fmt.Errorf("Grok Web 暂不支持 tools.type=%q", configuration.Unsupported[0])
	}
	return configuration, err
}

// injectToolPrompt 将工具调用约定放在拼接后的会话文本之前。
func injectToolPrompt(prompt string, configuration toolemu.Config) string {
	system := toolemu.Prompt(configuration)
	if system == "" {
		return prompt
	}
	return "[system]\n" + system + "\n\n" + prompt
}

// toolCallsToXML 将 Chat Completions 历史 tool_calls 还原为调用约定。
func toolCallsToXML(raw json.RawMessage) string {
	var values []struct {
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &values) != nil || len(values) == 0 {
		return ""
	}
	calls := make([]toolemu.Call, 0, len(values))
	for _, value := range values {
		if toolemu.ValidName(value.Function.Name) {
			calls = append(calls, toolemu.Call{Name: value.Function.Name, Arguments: value.Function.Arguments})
		}
	}
	return toolemu.FormatCalls(calls)
}
