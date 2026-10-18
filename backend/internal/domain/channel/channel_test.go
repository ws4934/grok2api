package channel

import (
	"context"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

type lifecycleModule struct {
	AccountLifecycle
	descriptor ChannelDescriptor
}

func (m lifecycleModule) Descriptor() ChannelDescriptor { return m.descriptor }

type chatModule struct{ lifecycleModule }

func (chatModule) Chat(context.Context, Credentials, ChatRequest, func(ChatEvent) error) error {
	return nil
}
func TestRegistryIsExplicitAndValidatesCapabilities(t *testing.T) {
	descriptor := ChannelDescriptor{ID: account.Provider("new_channel"), Name: "Future channel", Version: ContractVersion, Capabilities: Capabilities{Chat: true}}
	onlyLife := lifecycleModule{descriptor: descriptor}
	if _, e := NewRegistry(onlyLife); e == nil {
		t.Fatal("declared chat without implementation accepted")
	}
	module := chatModule{onlyLife}
	r, e := NewRegistry(module)
	if e != nil {
		t.Fatal(e)
	}
	if _, ok := r.Get("new_channel"); !ok {
		t.Fatal("new ID requires central enum")
	}
	if _, ok := r.Get("unknown_channel"); ok {
		t.Fatal("unknown module registered")
	}
	if _, e := NewRegistry(module, module); e == nil {
		t.Fatal("duplicate channel accepted")
	}
	descriptor.Capabilities.OAuth = true
	if _, e := NewRegistry(chatModule{lifecycleModule{descriptor: descriptor}}); e == nil {
		t.Fatal("OAuth without adapter accepted")
	}
	for _, id := range []account.Provider{"grok_web", "UPPER", "bad/id", ""} {
		descriptor.ID = id
		if _, e := NewRegistry(chatModule{lifecycleModule{descriptor: descriptor}}); e == nil {
			t.Fatalf("invalid descriptor %q accepted", id)
		}
	}
	if DefaultSettings().Enabled {
		t.Fatal("extension enabled by default")
	}
}
