package middle

import (
	"encoding/json"
	"fmt"
	"github.com/bincooo/chatgpt-adapter/v2/internal/agent"
	"github.com/bincooo/chatgpt-adapter/v2/internal/common"
	"github.com/bincooo/chatgpt-adapter/v2/internal/vars"
	"github.com/bincooo/chatgpt-adapter/v2/pkg"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"slices"
	"strings"
	"time"
)

var excludeToolNames = "__EXCLUDE_TOOL_NAMES__"

func buildTemplate(ctx *gin.Context, completion pkg.ChatCompletion, template string, max int) (message string, err error) {
	pMessages := completion.Messages
	messageL := len(pMessages)
	content := "continue"

	if messageL > max {
		pMessages = pMessages[messageL-max:]
		messageL = len(pMessages)
	}

	if messageL > 0 {
		if keyv := pMessages[messageL-1]; keyv.Is("role", "user") {
			content = keyv.GetString("content")
			pMessages = pMessages[:messageL-1]
		}
	}

	if content == "continue" {
		ctx.Set(excludeToolNames, extractToolNames(completion.Messages))
	}

	for _, t := range completion.Tools {
		t.GetKeyv("function").Set("id", common.RandStr(5))
	}

	parser := templateBuilder().
		Vars("toolDef", toolDef(ctx, completion.Tools)).
		Vars("tools", completion.Tools).
		Vars("pMessages", pMessages).
		Vars("content", content).
		//Func("ContainsT", func(value interface{}) bool {
		//	if len(excludeToolNames) == 0 || value == nil || value == "" {
		//		return false
		//	}
		//	return slices.Contains(excludeToolNames, value)
		//}).
		Func("Join", func(slice []interface{}, sep string) string {
			if len(slice) == 0 {
				return ""
			}
			var result []string
			for _, v := range slice {
				result = append(result, fmt.Sprintf("\"%v\"", v))
			}
			return strings.Join(result, sep)
		}).
		Func("ToolDesc", func(value string) string {
			for _, t := range completion.Tools {
				fn := t.GetKeyv("function")
				if !fn.Has("name") {
					continue
				}
				if value == fn.GetString("name") {
					return fn.GetString("description")
				}
			}
			return ""
		}).Do()
	return parser(template)
}

// 执行工具选择器
//
//	return:
//	bool  > 是否执行了工具
//	error > 执行异常
func CompleteToolCalls(ctx *gin.Context, completion pkg.ChatCompletion, callback func(message string) (string, error)) (bool, error) {
	message, err := buildTemplate(ctx, completion, agent.ToolCall, 10)
	if err != nil {
		return false, err
	}

	content, err := callback(message)
	if err != nil {
		return false, err
	}
	logrus.Infof("completeTools response: \n%s", content)

	previousTokens := common.CalcTokens(message)
	ctx.Set(vars.GinCompletionUsage, common.CalcUsageTokens(content, previousTokens))

	// 解析参数
	return parseToToolCall(ctx, content, completion), nil
}

// 工具参数解析
//
//	return:
//	bool  > 是否执行了工具
func parseToToolCall(ctx *gin.Context, content string, completion pkg.ChatCompletion) bool {
	j := ""
	created := time.Now().Unix()
	slice := strings.Split(content, "TOOL_RESPONSE")

	for _, value := range slice {
		left := strings.Index(value, "{")
		right := strings.LastIndex(value, "}")
		if left >= 0 && right > left {
			j = value[left : right+1]
			break
		}
	}

	// 非-1值则为有默认选项
	valueDef := nameWithToolDef(ctx.GetString("tool"), completion.Tools)

	// 没有解析出 JSON
	if j == "" {
		if valueDef != "-1" {
			return toolCallResponse(ctx, completion, valueDef, "{}", created)
		}
		return false
	}

	name := ""
	for _, t := range completion.Tools {
		fn := t.GetKeyv("function")
		n := fn.GetString("name")
		// id 匹配
		if strings.Contains(j, fn.GetString("id")) {
			name = n
			break
		}
		// name 匹配
		if strings.Contains(j, n) {
			name = n
			break
		}
	}

	// 没有匹配到工具
	if name == "" {
		if valueDef != "-1" {
			return toolCallResponse(ctx, completion, valueDef, "{}", created)
		}
		return false
	}

	// 避免AI重复选择相同的工具
	if names, ok := common.GetGinValues[string](ctx, excludeToolNames); ok {
		if slices.Contains(names, name) {
			return valueDef != "-1" && toolCallResponse(ctx, completion, valueDef, "{}", created)
		}
	}

	// 解析参数
	var js map[string]interface{}
	if err := json.Unmarshal([]byte(j), &js); err != nil {
		logrus.Error(err)
		if valueDef != "-1" {
			return toolCallResponse(ctx, completion, valueDef, "{}", created)
		}
		return false
	}

	obj, ok := js["arguments"]
	if !ok {
		delete(js, "toolId")
		obj = js
	}

	bytes, _ := json.Marshal(obj)
	return toolCallResponse(ctx, completion, name, string(bytes), created)
}

func ToolCallCancel(str string) bool {
	if strings.Contains(str, "<|tool|>") {
		return true
	}
	if strings.Contains(str, "<|assistant|>") {
		return true
	}
	if strings.Contains(str, "<|user|>") {
		return true
	}
	if strings.Contains(str, "<|system|>") {
		return true
	}
	if strings.Contains(str, "<|end|>") {
		return true
	}
	return len(str) > 1 && !strings.HasPrefix(str, "1:")
}

func extractToolNames(messages []pkg.Keyv[interface{}]) (slice []string) {
	for pos := range messages {
		message := messages[pos]
		if message.Is("role", "tool") {
			slice = append(slice, message.GetString("name"))
		}
	}
	return
}

func toolCallResponse(ctx *gin.Context, completion pkg.ChatCompletion, name string, value string, created int64) bool {
	if completion.Stream {
		SSEToolCallResponse(ctx, completion.Model, name, value, created)
		return true
	} else {
		ToolCallResponse(ctx, completion.Model, name, value)
		return true
	}
}

func toolDef(ctx *gin.Context, tools []pkg.Keyv[interface{}]) (value string) {
	value = ctx.GetString("tool")
	if value == "" {
		return "-1"
	}

	for _, t := range tools {
		fn := t.GetKeyv("function")
		id := fn.GetString("id")
		if fn.Has("name") {
			if value == fn.GetString("name") {
				value = id
				return
			}
		}
	}
	return "-1"
}

func nameWithToolDef(name string, tools []pkg.Keyv[interface{}]) (value string) {
	value = name
	if value == "" {
		return "-1"
	}

	for _, t := range tools {
		fn := t.GetKeyv("function")
		if fn.Has("name") {
			if value == fn.GetString("name") {
				return
			}
		}
	}

	return "-1"
}
