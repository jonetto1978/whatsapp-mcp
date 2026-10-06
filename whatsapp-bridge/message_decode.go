package main

import (
	"context"
	"errors"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
)

const controlPrefix = "[control: "

// decodeForStorage uses the authenticated client's supported decrypt API.
// Keys stay inside whatsmeow; neither ciphertext nor secrets reach logs/API.
// A copy keeps callers' event/protobuf ownership unchanged.
func (b *Bridge) decodeForStorage(evt *events.Message) (*events.Message, string, string) {
	if evt == nil {
		return evt, "", "system"
	}
	m := unwrapEnvelope(evt.Message)
	if m.GetSecretEncryptedMessage() == nil {
		text, typ := extractContent(evt)
		return evt, text, typ
	}
	copyEvent := *evt
	copyEvent.Message = m
	ctx := b.rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	decrypt := b.secretDecrypt
	if decrypt == nil && b.client != nil {
		decrypt = b.client.DecryptSecretEncryptedMessage
	}
	reason := "client-unavailable"
	if decrypt != nil {
		decoded, err := decrypt(ctx, &copyEvent)
		if err == nil && decoded != nil && decoded.GetSecretEncryptedMessage() == nil {
			copyEvent.Message = decoded
			text, typ := extractContent(&copyEvent)
			return &copyEvent, text, typ
		}
		reason = "decrypt-failed"
		if errors.Is(err, whatsmeow.ErrOriginalMessageSecretNotFound) {
			reason = "missing-original"
		}
	}
	label := "secretEncryptedMessage." + m.GetSecretEncryptedMessage().GetSecretEncType().String() + "." + reason
	return &copyEvent, unsupportedMarker(label), "system"
}

func controlMarker(m *waE2E.Message) string {
	if m == nil {
		return ""
	}
	if p := m.GetProtocolMessage(); p != nil && p.Type != nil && p.GetEditedMessage() == nil && unsupportedMessageType(m) == "protocolMessage" {
		switch p.GetType() {
		case waE2E.ProtocolMessage_REVOKE, waE2E.ProtocolMessage_EPHEMERAL_SETTING,
			waE2E.ProtocolMessage_EPHEMERAL_SYNC_RESPONSE, waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION,
			waE2E.ProtocolMessage_APP_STATE_SYNC_KEY_SHARE, waE2E.ProtocolMessage_APP_STATE_SYNC_KEY_REQUEST,
			waE2E.ProtocolMessage_INITIAL_SECURITY_NOTIFICATION_SETTING_SYNC,
			waE2E.ProtocolMessage_APP_STATE_FATAL_EXCEPTION_NOTIFICATION,
			waE2E.ProtocolMessage_LID_MIGRATION_MAPPING_SYNC:
			return controlPrefix + "protocolMessage." + p.GetType().String() + "]"
		}
	}
	if m.GetPinInChatMessage() != nil && unsupportedMessageType(m) == "pinInChatMessage" {
		return controlPrefix + "pinInChatMessage]"
	}
	// Only positively recognized, textless carrier fields qualify. A nil or
	// empty proto remains unknown, and any unfamiliar content fails loud.
	if unsupportedMessageType(m) == "" {
		if m.GetSenderKeyDistributionMessage() != nil {
			return controlPrefix + "senderKeyDistributionMessage]"
		}
		if m.GetMessageContextInfo() != nil {
			return controlPrefix + "messageContextInfo]"
		}
	}
	return ""
}

func isControlText(text string) bool {
	return strings.HasPrefix(text, controlPrefix) && strings.HasSuffix(text, "]")
}

func contentState(typ, text string) string {
	if typ != "system" {
		if text == "" && typ != "text" {
			return "media_only"
		}
		return "available"
	}
	if isControlText(text) {
		return "control"
	}
	if strings.Contains(undecryptableFailMode(text), "viewonce") {
		return "protected_unavailable"
	}
	if undecryptableFailMode(text) != "" {
		return "undecryptable"
	}
	if raw := unsupportedRawType(text); raw != "" {
		if strings.HasPrefix(raw, "secretEncryptedMessage") {
			return "encrypted_update_unavailable"
		}
		return "unsupported"
	}
	if text == "" {
		return "unknown_legacy_empty"
	}
	return "available"
}

func isProtectedMedia(m *waE2E.Message) bool {
	for depth := 0; m != nil && depth < maxUnwrapDepth; depth++ {
		if m.GetViewOnceMessage() != nil || m.GetViewOnceMessageV2() != nil || m.GetViewOnceMessageV2Extension() != nil {
			return true
		}
		if m.GetImageMessage().GetViewOnce() || m.GetVideoMessage().GetViewOnce() || m.GetAudioMessage().GetViewOnce() {
			return true
		}
		switch {
		case m.GetEphemeralMessage().GetMessage() != nil:
			m = m.GetEphemeralMessage().GetMessage()
		case m.GetDeviceSentMessage().GetMessage() != nil:
			m = m.GetDeviceSentMessage().GetMessage()
		case m.GetDocumentWithCaptionMessage().GetMessage() != nil:
			m = m.GetDocumentWithCaptionMessage().GetMessage()
		case m.GetEditedMessage().GetMessage() != nil:
			m = m.GetEditedMessage().GetMessage()
		case m.GetProtocolMessage().GetEditedMessage() != nil:
			m = m.GetProtocolMessage().GetEditedMessage()
		default:
			return false
		}
	}
	return true // fail closed at the nesting bound
}
