// Package provider builds configured backends. Each backend lives in its own
// subpackage behind core.Provider.
package provider

import (
	"context"
	"fmt"

	"github.com/TheAsianFish/switchboard/internal/config"
	"github.com/TheAsianFish/switchboard/internal/core"
	"github.com/TheAsianFish/switchboard/internal/provider/anthropic"
	"github.com/TheAsianFish/switchboard/internal/provider/fake"
	"github.com/TheAsianFish/switchboard/internal/provider/openaicompat"
)

// Build constructs the backend a provider entry describes. A provider whose
// key variable is named but unset is still built, as one that refuses every
// request with a message naming the variable, so the gateway starts and the
// other providers keep serving.
func Build(p config.Provider) (core.Provider, error) {
	if p.APIKeyEnv != "" && p.APIKey == "" {
		return missingKey{provider: p.Name, env: p.APIKeyEnv}, nil
	}
	switch p.Type {
	case config.TypeAnthropic:
		prov, err := anthropic.New(anthropic.Config{
			BaseURL:          p.BaseURL,
			APIKey:           p.APIKey,
			Timeout:          p.Timeout,
			DefaultMaxTokens: p.DefaultMaxTokens,
		})
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", p.Name, err)
		}
		return prov, nil
	case config.TypeOpenAICompat:
		prov, err := openaicompat.New(openaicompat.Config{BaseURL: p.BaseURL, APIKey: p.APIKey, Timeout: p.Timeout})
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", p.Name, err)
		}
		return prov, nil
	case config.TypeFake:
		cfg := fake.DefaultConfig
		if p.Fake != (config.FakeProvider{}) {
			cfg = fake.Config{Latency: p.Fake.Latency, Tokens: p.Fake.Tokens, TokenInterval: p.Fake.TokenInterval}
		}
		return fake.New(cfg), nil
	default:
		return nil, fmt.Errorf("provider %q: unknown type %q", p.Name, p.Type)
	}
}

// Disabled reports whether prov is a placeholder for a provider without its
// key.
func Disabled(prov core.Provider) bool {
	_, ok := prov.(missingKey)
	return ok
}

type missingKey struct{ provider, env string }

func (m missingKey) err() error {
	// The client learns the provider is unconfigured; the variable name is
	// in the startup warning, not in responses.
	return core.Errorf(core.KindUpstream, "provider %q is not configured on this gateway", m.provider)
}

func (m missingKey) Complete(context.Context, *core.Request) (*core.Response, error) {
	return nil, m.err()
}

func (m missingKey) Stream(context.Context, *core.Request) (core.Stream, error) {
	return nil, m.err()
}
