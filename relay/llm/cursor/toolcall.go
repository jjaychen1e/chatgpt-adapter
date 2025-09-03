package cursor

import (
	"chatgpt-adapter/core/common/toolcall"
	"chatgpt-adapter/core/common/vars"
	"chatgpt-adapter/core/gin/model"
	"chatgpt-adapter/core/gin/response"
	"chatgpt-adapter/core/logger"

	"github.com/gin-gonic/gin"
	"github.com/iocgo/sdk/env"
)

func toolChoice(ctx *gin.Context, env *env.Environment, cookie string, completion model.Completion) bool {
	logger.Info("completeTools ...")
	echo := ctx.GetBool(vars.GinEcho)

	exec, err := toolcall.ToolChoice(ctx, completion, func(message string) (string, error) {
		if echo {
			logger.Infof("toolCall message: \n%s", message)
			return "", nil
		}

		completion.Messages = []model.Keyv[interface{}]{
			{
				"role":    "user",
				"content": message,
			},
		}
		messageBuffer, err := convertRequest(completion)
		if err != nil {
			return "", err
		}

		r, err := fetch(ctx, env, cookie, messageBuffer)
		if err != nil {
			return "", err
		}

		return waitMessage(ctx, r, toolcall.Cancel)
	})

	if err != nil {
		logger.Error(err)
		response.Error(ctx, -1, err)
		return true
	}

	return exec
}
