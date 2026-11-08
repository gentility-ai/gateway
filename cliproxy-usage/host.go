package main

import (
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// hostCall sends one call to the proxy through the callback it handed over at
// init. Tests replace it with a fake host.
var hostCall = nativeHostCall

// callHost calls a host method and decodes its result envelope.
func callHost(method string, request, result any) error {
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	answer, err := hostCall(method, raw)
	if err != nil {
		return err
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(answer, &envelope); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if !envelope.OK {
		if envelope.Error != nil {
			return fmt.Errorf("%s: %s", method, envelope.Error.Message)
		}
		return fmt.Errorf("%s failed", method)
	}
	return json.Unmarshal(envelope.Result, result)
}
