//go:build 3rd

package scan

import (
	"github.com/iocgo/sdk"

	"chatgpt-adapter/relay/3rd/llm/kilo"
	"chatgpt-adapter/relay/3rd/llm/trae"
	"chatgpt-adapter/relay/3rd/llm/zed"
)

func rejects(container *sdk.Container) (err error) {
	err = trae.Injects(container)
	if err != nil {
		return
	}

	err = kilo.Injects(container)
	if err != nil {
		return
	}

	err = zed.Injects(container)
	if err != nil {
		return
	}

	return
}
