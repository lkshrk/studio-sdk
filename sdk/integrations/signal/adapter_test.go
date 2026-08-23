package signal

import (
	"testing"

	"github.com/qf-studio/studio-sdk/sdk/core"
)

func TestAdapterName(t *testing.T) {
	a := New(Config{BaseURL: "http://signal.invalid", Account: "+4900000000"}, nil)
	if got := a.Name(); got != "signal" {
		t.Errorf("Name() = %q, want %q", got, "signal")
	}
}

func TestAdapterNewChatBridge(t *testing.T) {
	a := New(Config{
		BaseURL:   "http://signal.invalid",
		Account:   "+4900000000",
		Groups:    []string{fixtureGroupID},
		Approvers: []string{fixtureVoterID},
		SelfUUID:  fixtureSelfUUID,
	}, nil)

	deps := core.ChatDeps{Handler: &recordingHandler{}}
	br := a.NewChatBridge(deps)
	if br == nil {
		t.Fatal("NewChatBridge returned nil")
	}
	b, ok := br.(*bridge)
	if !ok {
		t.Fatalf("NewChatBridge returned %T, want *bridge", br)
	}
	if b.initErr != nil {
		t.Errorf("initErr = %v, want nil for a usable config", b.initErr)
	}
}

func TestAdapterNewChatBridgeWithoutGroupsFailsOnStart(t *testing.T) {
	a := New(Config{BaseURL: "http://signal.invalid", Account: "+4900000000"}, nil)

	br := a.NewChatBridge(core.ChatDeps{Handler: &recordingHandler{}})
	b, ok := br.(*bridge)
	if !ok {
		t.Fatalf("NewChatBridge returned %T, want *bridge", br)
	}
	if b.initErr == nil {
		t.Error("initErr = nil, want an error when the group allowlist is empty")
	}
}

func TestAdapterImplementsInterfaces(t *testing.T) {
	a := New(Config{BaseURL: "http://signal.invalid", Account: "+4900000000"}, nil)
	var _ core.Adapter = a
	var _ core.ChatCapable = a
}
