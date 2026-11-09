package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func TestRegistrationFailsWithoutMetering(t *testing.T) {
	t.Setenv("GENTILITY_USAGE_URL", "")
	t.Setenv("GENTILITY_USAGE_SPOOL", t.TempDir())

	raw, err := handleMethod(pluginabi.MethodPluginRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "metering_unconfigured" {
		t.Fatalf("registered without metering: %s", raw)
	}
	if posted != nil {
		t.Fatal("poster started without a URL")
	}
}

func TestARollbackTestBuildRefusesToRegister(t *testing.T) {
	refuse = "rollback-test"
	t.Cleanup(func() { refuse = "" })
	t.Setenv("GENTILITY_USAGE_URL", "http://127.0.0.1:1/usage")
	t.Setenv("GENTILITY_USAGE_SPOOL", t.TempDir())

	raw, err := handleMethod(pluginabi.MethodPluginRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "rollback_test" {
		t.Fatalf("a rollback-test build registered: %s", raw)
	}
}
