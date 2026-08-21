// Package signal provides a Studio SDK Signal adapter over signal-cli-rest-api.
// It implements sdk/core.Adapter and sdk/core.ChatCapable.
//
// Group membership is the authorization boundary: anyone in an allowlisted
// group can message pilot and vote in its approval polls, so an empty Groups
// list admits nothing.
//
// Usage:
//
//	cfg := signal.Config{
//		BaseURL: "http://signal-cli-rest-api:8080",
//		Account: "+4912345",
//		Groups:  []string{"group.abc..."},
//	}
//	a := signal.New(cfg, nil)
//	core.Register(a)
package signal

import (
	"errors"
	"log/slog"

	"github.com/qf-studio/studio-sdk/sdk/core"
)

// Compile-time interface assertions.
var (
	_ core.Adapter     = (*Adapter)(nil)
	_ core.ChatCapable = (*Adapter)(nil)
	_ core.ChatBridge  = (*bridge)(nil)
)

// Config holds configuration for the Signal connector.
type Config struct {
	// BaseURL is the signal-cli-rest-api service root (json-rpc mode).
	BaseURL string
	// Account is the registered Signal number this adapter sends and receives as.
	Account string
	// Groups is the allowlist of Signal groups the adapter acts on. Either
	// encoding is accepted — "group.<base64>" from GET /v1/groups or the raw
	// form carried in received envelopes. Empty admits nothing.
	Groups []string
	// SelfUUID is the account's own Signal UUID, used to ignore the adapter's
	// own traffic so a message it sent cannot round-trip into its command path.
	// Optional; without it self-filtering is skipped.
	SelfUUID string
	// StyledText sends messages with text_mode "styled", rendering *italic*,
	// **bold**, `monospace`, ~strikethrough~ and ||spoiler|| markers as styles.
	StyledText bool
	// MaxMessageLength overrides the chunking threshold; zero uses the default.
	MaxMessageLength int
}

// Adapter implements core.Adapter and core.ChatCapable for Signal.
type Adapter struct {
	cfg    Config
	logger *slog.Logger
}

// New creates a new Signal adapter. logger may be nil to use slog.Default().
func New(cfg Config, logger *slog.Logger) *Adapter {
	if logger == nil {
		logger = slog.Default()
	}
	return &Adapter{cfg: cfg, logger: logger}
}

// Name returns the adapter identifier.
func (a *Adapter) Name() string { return "signal" }

// NewChatBridge creates a ChatBridge using the given dependencies.
// Construction errors (bad BaseURL, missing account) surface on Start.
func (a *Adapter) NewChatBridge(deps core.ChatDeps) core.ChatBridge {
	senderOpts := []SenderOption{}
	if a.cfg.StyledText {
		senderOpts = append(senderOpts, WithStyledText())
	}
	if a.cfg.MaxMessageLength > 0 {
		senderOpts = append(senderOpts, WithMaxMessageLength(a.cfg.MaxMessageLength))
	}

	sender, senderErr := NewSender(a.cfg.BaseURL, a.cfg.Account, senderOpts...)
	receiver, receiverErr := NewReceiver(a.cfg.BaseURL, a.cfg.Account, WithLogger(a.logger))

	// An empty allowlist would leave the bridge listening with no authorization
	// boundary, so refuse at Start rather than run permissively.
	var groupsErr error
	if len(a.cfg.Groups) == 0 {
		groupsErr = errors.New("signal: no groups configured; the group allowlist is the authorization boundary")
	}

	return &bridge{
		sender:           sender,
		receiver:         receiver,
		deps:             deps,
		allow:            NewGroupAllowlist(a.cfg.Groups...),
		selfUUID:         a.cfg.SelfUUID,
		logger:           a.logger,
		polls:            make(map[int64]pollMeta),
		seenVoteRevision: make(map[string]int),
		initErr:          firstErr(groupsErr, senderErr, receiverErr),
	}
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
