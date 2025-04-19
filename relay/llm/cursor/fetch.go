package cursor

import (
	"chatgpt-adapter/core/cache"
	"chatgpt-adapter/core/common"
	"chatgpt-adapter/core/gin/model"
	"chatgpt-adapter/core/logger"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bincooo/emit.io"
	"github.com/gin-gonic/gin"
	"github.com/golang/protobuf/proto"
	"github.com/google/uuid"
	"github.com/iocgo/sdk/env"
	"github.com/iocgo/sdk/stream"
)

func fetch(ctx *gin.Context, env *env.Environment, cookie string, buffer []byte) (response *http.Response, err error) {
	// count, err := checkUsage(ctx, env, 150)
	// if err != nil {
	// 	return
	// }
	// if count <= 0 {
	// 	err = fmt.Errorf("invalid usage")
	// 	return
	// }

	// Prepare headers map for easier logging
	headers := map[string]string{
		"authorization":            "Bearer " + cookie,
		"content-type":             "application/connect+proto",
		"connect-accept-encoding":  "gzip",
		"connect-content-encoding": "gzip",
		"connect-protocol-version": "1",
		"traceparent":              "00-" + strings.ReplaceAll(uuid.NewString(), "-", "") + "-" + common.Hex(16) + "-00",
		"user-agent":               "connect-es/1.6.1",
		"x-amzn-trace-id":          "Root=" + uuid.NewString(),
		"x-client-key":             genClientKey(ctx.GetString("token")),
		"x-cursor-checksum":        genChecksum(ctx, env),
		"x-cursor-client-version":  "0.46.2",
		"x-cursor-timezone":        "Asia/Shanghai",
		"x-ghost-mode":             "false",
		"x-request-id":             uuid.NewString(),
		"x-session-id":             uuid.NewString(),
		"host":                     "api2.cursor.sh",
		"Connection":               "close",
		"Transfer-Encoding":        "chunked",
	}

	// Log headers if debug is enabled
	if true {
		logger.Debug("Cursor API Request Headers:")
		for key, value := range headers {
			// Mask sensitive information
			if key == "authorization" {
				logger.Debugf("  %s: Bearer ***", key)
			} else {
				logger.Debugf("  %s: %s", key, value)
			}
		}
	}

	// Build and execute request with headers
	client := emit.ClientBuilder(common.HTTPClient).
		Context(ctx.Request.Context()).
		Proxies(env.GetString("server.proxied")).
		POST("https://api2.cursor.sh/aiserver.v1.AiService/StreamChat")

	// Apply headers from map
	for key, value := range headers {
		client = client.Header(key, value)
	}

	response, err = client.Bytes(buffer).DoC(emit.Status(http.StatusOK), emit.IsPROTO)

	// Log response headers if debug is enabled and response is successful
	if env.GetBool("server.debug") && err == nil && response != nil {
		logger.Debug("Cursor API Response Headers:")
		for key, values := range response.Header {
			for _, value := range values {
				logger.Debugf("  %s: %s", key, value)
			}
		}
		logger.Debugf("  Status: %s", response.Status)
	}

	return
}

func convertRequest(completion model.Completion) (buffer []byte, err error) {
	messages := stream.Map(stream.OfSlice(completion.Messages), func(message model.Keyv[interface{}]) *ChatMessage_UserMessage {
		return &ChatMessage_UserMessage{
			MessageId: uuid.NewString(),
			Role:      elseOf[int32](message.Is("role", "user"), 1, 2),
			Content:   message.GetString("content"),
		}
	}).ToSlice()
	message := &ChatMessage{
		Messages:      messages,
		UnknownField4: "",
		Model: &ChatMessage_Model{
			Name:  completion.Model[7:],
			Empty: "",
		},
		UnknownField13: 1,
		ConversationId: uuid.NewString(),
		UnknownField16: 1,
		UnknownField29: 1,
		UnknownField30: 0,
	}

	protoBytes, err := proto.Marshal(message)
	if err != nil {
		return
	}

	header := int32ToBytes(0, len(protoBytes))
	buffer = append(header, protoBytes...)
	return
}

func checkUsage(ctx *gin.Context, env *env.Environment, max int) (count int, err error) {
	var (
		cookie = ctx.GetString("token")
	)
	cookie, err = url.QueryUnescape(cookie)
	if err != nil {
		return
	}

	user := ""
	if strings.Contains(cookie, "::") {
		user = strings.Split(cookie, "::")[0]
	}
	response, err := emit.ClientBuilder(common.HTTPClient).
		Context(ctx.Request.Context()).
		Proxies(env.GetString("server.proxied")).
		GET("https://www.cursor.com/api/usage").
		Query("user", user).
		Header("cookie", "WorkosCursorSessionToken="+cookie).
		Header("referer", "https://www.cursor.com/settings").
		Header("user-agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.3 Safari/605.1.15").
		DoC(emit.Status(http.StatusOK), emit.IsJSON)
	if err != nil {
		return
	}
	defer response.Body.Close()
	obj, err := emit.ToMap(response)
	if err != nil {
		return
	}

	if som, ok := obj["startOfMonth"]; ok {
		t, e := time.Parse("2006-01-02T15:04:05.000Z", som.(string))
		if e != nil {
			logger.Error(e)
		} else {
			if t.Before(time.Now().Add(-(14 * 24 * time.Hour))) { // 超14天
				return
			}
		}
	}

	for k, v := range obj {
		if !strings.Contains(k, "gpt-") {
			continue
		}
		value, ok := v.(map[string]interface{})
		if !ok {
			continue
		}

		i := value["numRequests"].(float64)
		count += int(i)
	}

	count = max - count
	return
}

func genClientKey(token string) string {
	hex1 := sha256.Sum256([]byte(token + "--client-key"))
	return hex.EncodeToString(hex1[:])
}

func genChecksum(ctx *gin.Context, env *env.Environment) string {
	token := ctx.GetString("token")
	checksum := ctx.GetHeader("x-cursor-checksum")
	logger.Debug("Generating checksum - Input parameters:")
	logger.Debugf("  Initial x-cursor-checksum header: %s", checksum)

	if checksum == "" {
		checksum = env.GetString("cursor.checksum")
		logger.Debugf("  Using cursor.checksum from env: %s", checksum)

		if strings.HasPrefix(checksum, "http") {
			logger.Debug("HTTP checksum URL detected, fetching from cache or remote")
			cacheManager := cache.CursorCacheManager()
			tokenHash := common.CalcHex(token)
			logger.Debugf("  Token hash for cache: %s", tokenHash)

			value, err := cacheManager.GetValue(tokenHash)
			if err != nil {
				logger.Error(err)
				return ""
			}
			if value != "" {
				logger.Debug("  Found cached checksum value")
				return value
			}

			logger.Debugf("  Fetching checksum from URL: %s", checksum)
			response, err := emit.ClientBuilder(common.HTTPClient).GET(checksum).
				DoC(emit.Status(http.StatusOK), emit.IsTEXT)
			if err != nil {
				logger.Error(err)
				return ""
			}
			checksum = emit.TextResponse(response)
			response.Body.Close()
			logger.Debugf("  Received checksum from HTTP: %s", checksum)

			logger.Debug("  Caching checksum value for 30 minutes")
			_ = cacheManager.SetWithExpiration(tokenHash, checksum, 30*time.Minute)
			return checksum
		}
	}

	if checksum == "" {
		logger.Debug("Generating dynamic checksum")
		// 不采用全局设备码方式，而是用cookie产生。更换时仅需要重新抓取新的WorkosCursorSessionToken即可
		salt := strings.Split(token, ".")
		logger.Debugf("  Salt length: %d", len(salt))

		calc := func(data []byte) {
			var t byte = 165
			for i := range data {
				data[i] = (data[i] ^ t) + byte(i)
				t = data[i]
			}
		}

		// 对时间检验了
		t := time.Now()
		t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 30*(t.Minute()/30), 0, 0, t.Location()) // 每个半小时轮换一次
		timestamp := int64(math.Floor(float64(t.UnixMilli()) / 1e6))
		logger.Debugf("  Using timestamp for checksum: %d", timestamp)

		data := []byte{
			byte((timestamp >> 8) & 0xff),
			byte(timestamp & 0xff),
			byte((timestamp >> 24) & 0xff),
			byte((timestamp >> 16) & 0xff),
			byte((timestamp >> 8) & 0xff),
			byte(timestamp & 0xff),
		}
		logger.Debugf("  Initial data bytes: %v", data)

		calc(data)
		logger.Debugf("  Calculated data bytes: %v", data)

		hex1 := sha256.Sum256([]byte(salt[1]))
		hex2 := sha256.Sum256([]byte(token))
		logger.Debug("  Generated SHA256 hashes")

		// 前面的字符生成存在问题，先硬编码
		// woc , 粗心大意呀
		checksum = fmt.Sprintf("%s%s/%s", base64.RawStdEncoding.EncodeToString(data), hex.EncodeToString(hex1[:]), hex.EncodeToString(hex2[:]))
		logger.Debug("  Generated final dynamic checksum")
	}

	logger.Debugf("Returning checksum: %s", checksum)
	return checksum
}

func int32ToBytes(magic byte, num int) []byte {
	hex := make([]byte, 4)
	binary.BigEndian.PutUint32(hex, uint32(num))
	return append([]byte{magic}, hex...)
}

func bytesToInt32(hex []byte) int {
	return int(binary.BigEndian.Uint32(hex))
}

func elseOf[T any](condition bool, a1, a2 T) T {
	if condition {
		return a1
	}
	return a2
}
