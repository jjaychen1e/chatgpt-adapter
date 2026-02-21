package cursor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/iocgo/sdk/env"

	"chatgpt-adapter/core/common"
	"chatgpt-adapter/core/common/vars"
	"chatgpt-adapter/core/gin/response"
	"chatgpt-adapter/core/logger"

	"github.com/bincooo/emit.io"
	"github.com/gin-gonic/gin"
	"github.com/golang/protobuf/proto"
)

const ginTokens = "__tokens__"

type chunkError struct {
	E struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details []struct {
			Type  string `json:"type"`
			Debug struct {
				Error   string `json:"error"`
				Details struct {
					Title       string `json:"title"`
					Detail      string `json:"detail"`
					IsRetryable bool   `json:"isRetryable"`
				} `json:"details"`
				IsExpected bool `json:"isExpected"`
			} `json:"debug"`
			Value string `json:"value"`
		} `json:"details"`
	} `json:"error"`
}

func (ce chunkError) Error() string {
	message := ce.E.Message
	if len(ce.E.Details) > 0 {
		message = ce.E.Details[0].Debug.Details.Detail
	}
	return fmt.Sprintf("[%s] %s", ce.E.Code, message)
}

func waitMessage(ctx *gin.Context, r *http.Response, cancel func(str string) bool) (content string, err error) {
	defer r.Body.Close()
	scanner := newScanner(r.Body, ctx.Request.Header)
	for {
		if !scanner.Scan() {
			break
		}

		event := scanner.Text()
		if event == "" {
			continue
		}

		if !scanner.Scan() {
			break
		}

		chunk := scanner.Bytes()
		if len(chunk) == 0 {
			continue
		}

		if event[7:] == "error" {
			var chunkErr chunkError
			err = json.Unmarshal(chunk, &chunkErr)
			if err == nil {
				err = &chunkErr
			}
			return
		}

		if event[7:] == "system" || event[7:] == "thinking" || bytes.Equal(chunk, []byte("{}")) {
			continue
		}

		raw := string(chunk)
		logger.Debug("----- raw -----")
		logger.Debug(raw)
		if len(raw) > 0 {
			content += raw
			if cancel != nil && cancel(content) {
				return content, nil
			}
		}
	}

	return content, nil
}

func waitResponse(ctx *gin.Context, r *http.Response, sse bool) (content string) {
	defer r.Body.Close()
	created := time.Now().Unix()
	logger.Info("waitResponse ...")
	matchers := common.GetGinMatchers(ctx)
	completion := common.GetGinCompletion(ctx)
	tokens := ctx.GetInt(ginTokens)
	thinkReason := env.Env.GetBool("server.think_reason")
	modelName := completion.Model[7:]
	thinkReason = thinkReason && isReasoningModel(modelName)
	logger.Infof("model=%s, thinkReason=%v", modelName, thinkReason)
	reasoningContent := ""
	think := 0

	onceExec := sync.OnceFunc(func() {
		if !sse {
			ctx.Writer.WriteHeader(http.StatusOK)
		}
	})

	scanner := newScanner(r.Body, ctx.Request.Header)
	for {
		if !scanner.Scan() {
			raw := response.ExecMatchers(matchers, "", true)
			if raw != "" && sse {
				response.SSEResponse(ctx, Model, raw, created)
			}
			content += raw
			break
		}
		event := scanner.Text()
		if event == "" {
			continue
		}

		if !scanner.Scan() {
			raw := response.ExecMatchers(matchers, "", true)
			if raw != "" && sse {
				response.SSEResponse(ctx, Model, raw, created)
			}
			content += raw
			break
		}

		chunk := scanner.Bytes()
		if len(chunk) == 0 {
			continue
		}

		if event[7:] == "error" {
			if bytes.Equal(chunk, []byte("{}")) {
				continue
			}
			var chunkErr chunkError
			err := json.Unmarshal(chunk, &chunkErr)
			if err == nil {
				err = &chunkErr
			}

			if response.NotSSEHeader(ctx) {
				logger.Error(err)
				response.Error(ctx, -1, err)
			}
			return
		}

		if event[7:] == "system" || bytes.Equal(chunk, []byte("{}")) {
			continue
		}

		raw := string(chunk)
		reasonContent := ""
		eventType := event[7:]

		if eventType == "thinking" {
			if thinkReason {
				reasonContent = raw
				reasoningContent += raw
				raw = ""
				think = 1
				logger.Debug("----- think raw -----")
				logger.Debug(reasonContent)
				goto label
			}
			// Fallback: wrap in <think> tags in content for clients that don't support reasoning_content
			if think == 0 {
				think = 1
				raw = "<think>\n" + raw
			}
		} else {
			// Transition from thinking to content
			if think == 1 {
				if thinkReason {
					think = 2
				} else {
					think = 2
					raw = "\n</think>\n" + raw
				}
			}
		}

		logger.Debug("----- raw -----")
		logger.Debug(raw)
		onceExec()

		raw = response.ExecMatchers(matchers, raw, false)
		if len(raw) == 0 {
			continue
		}

		if raw == response.EOF {
			break
		}

	label:
		if sse {
			response.ReasonSSEResponse(ctx, Model, raw, reasonContent, created)
		}
		content += raw
	}

	if content == "" && response.NotSSEHeader(ctx) {
		return
	}

	ctx.Set(vars.GinCompletionUsage, response.CalcUsageTokens(reasoningContent+content, tokens))
	if !sse {
		response.ReasonResponse(ctx, Model, content, reasoningContent)
	} else {
		response.SSEResponse(ctx, Model, "[DONE]", created)
	}
	return
}

// isReasoningModel checks if the model name is a reasoning/thinking model.
// Reads from config: cursor.reasoning_models (exact matches) and cursor.reasoning_patterns (substring matches).
// Falls back to built-in defaults if not configured.
func isReasoningModel(modelName string) bool {
	models := env.Env.GetStringSlice("cursor.reasoning_models")
	patterns := env.Env.GetStringSlice("cursor.reasoning_patterns")

	// Apply defaults independently — user can override one without losing the other
	if len(models) == 0 {
		models = []string{"deepseek-r1", "o3", "think3"}
	}
	if len(patterns) == 0 {
		patterns = []string{"thinking", "gemini-2.5-pro", "gemini-2.5-flash", "gemini-3"}
	}

	if slices.Contains(models, modelName) {
		return true
	}
	for _, pattern := range patterns {
		if strings.Contains(modelName, pattern) {
			return true
		}
	}
	return false
}

func newScanner(body io.ReadCloser, _ http.Header) (scanner *bufio.Scanner) {
	// 每个字节占8位
	// 00000011 第一个字节是占位符，应该是用来代表消息类型的 假定 0: 消息体/proto, 1: 系统提示词/gzip, 2、3: 错误标记/gzip
	// 00000000 00000000 00000010 11011000 4个字节描述包体大小
	scanner = bufio.NewScanner(body)
	var (
		magic    byte
		chunkLen = -1
		setup    = 5
	)

	// Queue for buffered tokens when a single protobuf message
	// produces multiple event/data pairs (e.g. thinking + content transition)
	var pendingTokens [][]byte

	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		// Drain pending tokens first (no advancing the input)
		if len(pendingTokens) > 0 {
			token = pendingTokens[0]
			pendingTokens = pendingTokens[1:]
			return 0, token, nil
		}

		if atEOF && len(data) == 0 {
			return
		}

		if atEOF {
			return len(data), data, err
		}

		if chunkLen == -1 && len(data) < setup {
			return
		}

		if chunkLen == -1 {
			magic = data[0]
			chunkLen = bytesToInt32(data[1:setup])

			// 这部分应该是分割标记？或者补位
			if magic == 0 && chunkLen == 0 {
				chunkLen = -1
				return setup, []byte(""), err
			}

			if magic == 3 { // gzip+json 格式的消息
				return setup, []byte("event: message"), err
			}

			if magic == 2 { // json 格式的消息
				return setup, []byte("event: message"), err
			}

			// magic == 0 or magic == 1: defer event type to data phase
			// (need to decode protobuf first to know if it's thinking or content)
			return setup, nil, nil
		}

		if len(data) < chunkLen {
			return
		}

		chunk := data[:chunkLen]
		chunkLen = -1

		i := len(chunk)
		// gzip 解码
		if magic == 1 && emit.IsEncoding(chunk, "gzip") {
			reader, gzErr := emit.DecodeGZip(io.NopCloser(bytes.NewReader(chunk)))
			if gzErr != nil {
				err = gzErr
				return
			}
			chunk, err = io.ReadAll(reader)
		}

		// JSON 解码
		if magic == 2 {
			fmt.Println("magic == 2, chunk:", string(chunk))
		}

		// gzip+json 解码
		if magic == 3 && emit.IsEncoding(chunk, "gzip") {
			reader, gzErr := emit.DecodeGZip(io.NopCloser(bytes.NewReader(chunk)))
			if gzErr != nil {
				err = gzErr
				return
			}
			chunk, err = io.ReadAll(reader)

			fmt.Println("magic == 3, chunk:", string(chunk))
		}

		if magic == 0 || magic == 1 {
			// println(hex.EncodeToString(chunk))
			var message ResMessage
			err = proto.Unmarshal(chunk, &message)
			if err != nil {
				return
			}
			if message.Msg == nil {
				chunk = []byte("")
				advance = i
				return
			}

			hasThinking := message.Msg.Thinking != nil && message.Msg.Thinking.Text != ""
			hasValue := message.Msg.Value != ""

			if hasThinking && hasValue {
				// Transition: thinking ends and content begins in same protobuf message.
				// Return thinking event token now, queue thinking data + content event/data.
				pendingTokens = append(pendingTokens,
					[]byte(message.Msg.Thinking.Text),
					[]byte("event: message"),
					[]byte(message.Msg.Value),
				)
				return i, []byte("event: thinking"), nil
			} else if hasThinking {
				// Pure thinking chunk
				pendingTokens = append(pendingTokens,
					[]byte(message.Msg.Thinking.Text),
				)
				return i, []byte("event: thinking"), nil
			} else if hasValue {
				// Pure content chunk
				pendingTokens = append(pendingTokens,
					[]byte(message.Msg.Value),
				)
				return i, []byte("event: message"), nil
			} else {
				return i, []byte(""), nil
			}
		}
		return i, chunk, err
	})

	return
}
