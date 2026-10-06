package main

import (
	"context"
	"encoding/json"
	"errors"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"
)

func TestRepairUnsupportedHistoryContent(t *testing.T) {
	b, db := backfillDB(t)
	seedRow(t, db, "recover", "system", "[unsupported: ephemeralMessage]")
	n, err := b.backfillDecodedContent("recover", &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{Conversation: proto.String("synthetic recovered text")}}})
	if err != nil || n != 1 {
		t.Fatalf("recovery affected %d rows, error %v; want 1", n, err)
	}
	typ, text := rowOf(t, db, "recover")
	if typ != "text" || text != "synthetic recovered text" {
		t.Fatal("readable content was not restored")
	}
}

func TestRepairClassifiesCarrierWithoutCallingItMissingText(t *testing.T) {
	text, typ := extractContentFromProto(&waE2E.Message{SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{}})
	if typ != "system" || text != "[control: senderKeyDistributionMessage]" {
		t.Fatalf("got %q/%q; want explicit control classification", typ, text)
	}
}

func TestRepairWrappedMediaKeysAreAvailable(t *testing.T) {
	wrapped := &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("synthetic caption"), MediaKey: []byte("synthetic media key")}}}}
	fields, ok := extractFromMessage(wrapped)
	if !ok || string(fields.MediaKey) != "synthetic media key" {
		t.Fatal("ordinary wrapped media loses its download metadata")
	}
}

func TestRepairMissingContentNeverDowngradesExistingRows(t *testing.T) {
	b, db := backfillDB(t)
	for _, row := range []struct{ id, typ, body string }{
		{"good", "text", "keep original"}, {"caption", "image", "keep caption"}, {"unsupported", "system", "[unsupported: secretEncryptedMessage]"},
	} {
		seedRow(t, db, row.id, row.typ, row.body)
		n, err := b.backfillStoredContent(row.id, "[unsupported: anotherType]", "system")
		if err != nil || n != 0 {
			t.Fatalf("downgrade %s: %d %v", row.id, n, err)
		}
		typ, body := rowOf(t, db, row.id)
		if typ != row.typ || body != row.body {
			t.Fatal("row downgraded")
		}
	}
}

func TestRepairProtectedMediaNeverGetsPersistentKeys(t *testing.T) {
	for _, m := range []*waE2E.Message{
		{ViewOnceMessageV2: &waE2E.FutureProofMessage{Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{MediaKey: []byte("synthetic")}}}},
		{EphemeralMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{ViewOnce: proto.Bool(true), MediaKey: []byte("synthetic")}}}},
	} {
		if f, ok := extractFromMessage(m); ok || len(f.MediaKey) > 0 {
			t.Fatal("protected media acquired persistent download metadata")
		}
	}
}

func repairSecretProto() *waE2E.Message {
	return &waE2E.Message{SecretEncryptedMessage: &waE2E.SecretEncryptedMessage{
		SecretEncType:    waE2E.SecretEncryptedMessage_MESSAGE_EDIT.Enum(),
		TargetMessageKey: &waCommon.MessageKey{ID: proto.String("ORIGINAL"), Participant: proto.String("111@lid")},
		EncPayload:       []byte("synthetic ciphertext"),
	}}
}

func TestRepairSecretEditLiveAndHistoryUseSupportedDecrypt(t *testing.T) {
	b, db := backfillDB(t)
	b.client = &whatsmeow.Client{Store: &store.Device{}}
	calls := 0
	b.secretDecrypt = func(_ context.Context, evt *events.Message) (*waE2E.Message, error) {
		calls++
		if evt.Info.Chat.String() != "c@g.us" || evt.Info.Sender.String() != "111@lid" || !evt.Info.IsGroup {
			t.Fatal("decrypt lost chat/sender context")
		}
		if evt.Message.GetSecretEncryptedMessage() == nil {
			t.Fatal("decrypt did not receive encrypted update")
		}
		return &waE2E.Message{Conversation: proto.String("synthetic edited body")}, nil
	}
	seedRow(t, db, "LIVE", "system", "[unsupported: secretEncryptedMessage]")
	original := repairSecretProto()
	evt := &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: types.NewJID("c", types.GroupServer), Sender: types.NewJID("111", types.HiddenUserServer), IsGroup: true}, ID: "LIVE", Timestamp: time.Unix(100, 0)}, Message: original}
	b.onMessage(evt)
	if evt.Message != original || original.GetSecretEncryptedMessage() == nil {
		t.Fatal("decoder mutated caller event")
	}
	seedRow(t, db, "HISTORY", "system", "[unsupported: secretEncryptedMessage]")
	b.processHistorySyncEvent(&events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{ID: proto.String("c@g.us"), Messages: []*waHistorySync.HistorySyncMsg{{Message: &waWeb.WebMessageInfo{Key: &waCommon.MessageKey{ID: proto.String("HISTORY"), Participant: proto.String("111@lid")}, Message: repairSecretProto(), MessageTimestamp: proto.Uint64(101)}}}}}}})
	for _, id := range []string{"LIVE", "HISTORY"} {
		typ, body := rowOf(t, db, id)
		if typ != "text" || body != "synthetic edited body" {
			t.Fatalf("%s was not restored: %q %q", id, typ, body)
		}
	}
	if calls != 2 {
		t.Fatalf("decrypt calls=%d, want 2", calls)
	}
}

func TestRepairSecretFailureIsExplicitAndDoesNotExposeError(t *testing.T) {
	b := &Bridge{secretDecrypt: func(context.Context, *events.Message) (*waE2E.Message, error) {
		return nil, errors.New("sensitive diagnostic must not leak")
	}}
	_, text, typ := b.decodeForStorage(&events.Message{Message: repairSecretProto()})
	if typ != "system" || text != "[unsupported: secretEncryptedMessage.MESSAGE_EDIT.decrypt-failed]" {
		t.Fatalf("unavailable outcome %q %q", typ, text)
	}
	if contentState(typ, text) != "encrypted_update_unavailable" {
		t.Fatal("encrypted update reported as recovered")
	}
	b.secretDecrypt = func(context.Context, *events.Message) (*waE2E.Message, error) {
		return nil, whatsmeow.ErrOriginalMessageSecretNotFound
	}
	_, text, _ = b.decodeForStorage(&events.Message{Message: repairSecretProto()})
	if text != "[unsupported: secretEncryptedMessage.MESSAGE_EDIT.missing-original]" {
		t.Fatal("missing original not distinguished")
	}
}

func TestRepairControlUnknownAndEditClassifications(t *testing.T) {
	cases := []struct {
		m     *waE2E.Message
		state string
	}{
		{&waE2E.Message{}, "unknown_legacy_empty"},
		{&waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum()}}, "control"},
		{&waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), EditedMessage: &waE2E.Message{Conversation: proto.String("edited text")}}}, "available"},
		{&waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum()}}, "unsupported"},
	}
	for _, tc := range cases {
		text, typ := extractContentFromProto(tc.m)
		if got := contentState(typ, text); got != tc.state {
			t.Fatalf("state=%q, want %q", got, tc.state)
		}
	}
	bytes, err := json.Marshal(messageRow{Type: "system", ContentText: "[control: messageContextInfo]"})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	json.Unmarshal(bytes, &decoded)
	if decoded["content_state"] != "control" {
		t.Fatal("API classification absent")
	}
	if rawTypeForStorage("system", "[control: messageContextInfo]") != "" {
		t.Fatal("control inflates unreadable counters")
	}
}

func TestRepairControlsStayOutOfVaultWhileMissingContentStaysVisible(t *testing.T) {
	if renderMessageText("system", "[control: messageContextInfo]", "") != "" {
		t.Fatal("control noise exported")
	}
	if renderMessageText("system", "[unsupported: secretEncryptedMessage]", "") == "" {
		t.Fatal("missing update hidden")
	}
}
