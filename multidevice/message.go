// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package multidevice

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"

	"google.golang.org/protobuf/proto"

	"github.com/RadicalApp/libsignal-protocol-go/ecc"
	"github.com/RadicalApp/libsignal-protocol-go/groups"
	"github.com/RadicalApp/libsignal-protocol-go/protocol"
	"github.com/RadicalApp/libsignal-protocol-go/serialize"
	"github.com/RadicalApp/libsignal-protocol-go/session"

	whatsapp "go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	waProto "go.mau.fi/whatsmeow/binary/proto"
)

var pbSerializer = serialize.NewProtoBufSerializer()

func (cli *Client) decryptDM(child *waBinary.Node, from waBinary.FullJID, isPreKey bool) ([]byte, error) {
	content, _ := child.Content.([]byte)

	builder := session.NewBuilderFromSignal(cli.Session, from.SignalAddress(), serialize.NewJSONSerializer())
	cipher := session.NewCipher(builder, from.SignalAddress())
	if isPreKey {
		preKeyMsg, err := protocol.NewPreKeySignalMessageFromBytes(content, pbSerializer.PreKeySignalMessage, pbSerializer.SignalMessage)
		if err != nil {
			return nil, fmt.Errorf("failed to parse prekey message: %w", err)
		}
		plaintext, _, err := cipher.DecryptMessageReturnKey(preKeyMsg)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt prekey message: %w", err)
		}
		return unpadMessage(plaintext)
	} else {
		msg, err := protocol.NewSignalMessageFromBytes(content, pbSerializer.SignalMessage)
		if err != nil {
			return nil, fmt.Errorf("failed to parse normal message: %w", err)
		}
		plaintext, err := cipher.Decrypt(msg)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt normal message: %w", err)
		}
		return unpadMessage(plaintext)
	}
}

func (cli *Client) decryptGroupMsg(child *waBinary.Node, from waBinary.FullJID, chat waBinary.FullJID) ([]byte, error) {
	content, _ := child.Content.([]byte)

	senderKeyName := protocol.NewSenderKeyName(chat.String(), from.SignalAddress())
	builder := groups.NewGroupSessionBuilder(cli.Session, pbSerializer)
	cipher := groups.NewGroupCipher(builder, senderKeyName, cli.Session)
	msg, err := protocol.NewSenderKeyMessageFromBytes(content, pbSerializer.SenderKeyMessage)
	if err != nil {
		return nil, fmt.Errorf("failed to parse group message: %w", err)
	}
	plaintext, err := cipher.Decrypt(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt group message: %w", err)
	}
	return unpadMessage(plaintext)
}

var CheckPadding = true

func unpadMessage(plaintext []byte) ([]byte, error) {
	if CheckPadding {
		lastByte := plaintext[len(plaintext)-1]
		expectedPadding := bytes.Repeat([]byte{lastByte}, int(lastByte))
		if !bytes.HasSuffix(plaintext, expectedPadding) {
			return nil, fmt.Errorf("plaintext doesn't have expected padding")
		}
	}
	return plaintext[:len(plaintext)-int(plaintext[len(plaintext)-1])], nil
}

func padMessage(plaintext []byte) []byte {
	var pad [1]byte
	_, err := rand.Read(pad[:])
	if err != nil {
		panic(err)
	}
	plaintext = append(plaintext, bytes.Repeat(pad[:], int(pad[0]&0xf))...)
	return plaintext
}

func (cli *Client) decryptMessages(info *MessageInfo, node *waBinary.Node) {
	if len(node.GetChildrenByTag("unavailable")) == len(node.GetChildren()) {
		cli.sendRetryReceipt(node)
		return
	}
	children := node.GetChildren()
	cli.Log.Debugln("Decrypting", len(children), "messages from", info.FromString())
	handled := false
	for _, child := range children {
		if child.Tag != "enc" {
			continue
		}
		encType, ok := child.Attrs["type"].(string)
		if !ok {
			continue
		}
		var decrypted []byte
		var err error
		if encType == "pkmsg" || encType == "msg" {
			decrypted, err = cli.decryptDM(&child, info.From, encType == "pkmsg")
		} else if info.Chat != nil && encType == "skmsg" {
			decrypted, err = cli.decryptGroupMsg(&child, info.From, *info.Chat)
		} else {
			cli.Log.Warnfln("Unhandled encrypted message (type %s) from %s", encType, info.FromString())
			continue
		}
		if err != nil {
			cli.Log.Warnfln("Error decrypting message from %s: %v", info.FromString(), err)
			cli.sendRetryReceipt(node)
			return
		}

		var msg waProto.Message
		err = proto.Unmarshal(decrypted, &msg)
		if err != nil {
			cli.Log.Warnfln("Error unmarshaling decrypted message from %s: %v", info.FromString(), err)
			continue
		}

		cli.handleDecryptedMessage(info, &msg)
		handled = true
	}
	if handled {
		cli.sendMessageReceipt(info)
		cli.sendAck(node)
	}
}

type MessageInfo struct {
	From waBinary.FullJID
	Chat *waBinary.FullJID
	ID   string
	Type string

	Recipient *waBinary.FullJID

	Notify    string
	Timestamp int64
	Category  string
}

func (mi *MessageInfo) FromString() string {
	if mi.Chat != nil {
		return fmt.Sprintf("%s in %s", mi.From, mi.Chat)
	} else {
		return mi.From.String()
	}
}

func parseMessageInfo(node *waBinary.Node) (*MessageInfo, error) {
	var info MessageInfo

	from, ok := node.Attrs["from"].(waBinary.FullJID)
	if !ok {
		return nil, fmt.Errorf("didn't find valid `from` attribute in message")
	}
	recipient, ok := node.Attrs["recipient"].(waBinary.FullJID)
	if ok {
		info.Recipient = &recipient
	}
	if from.Server == waBinary.GroupServer {
		info.Chat = &from
		info.From, ok = node.Attrs["participant"].(waBinary.FullJID)
		if !ok {
			return nil, fmt.Errorf("didn't find valid `participant` attribute in group message")
		}
	} else {
		info.From = from
	}

	info.ID, ok = node.Attrs["id"].(string)
	if !ok {
		return nil, fmt.Errorf("didn't find valid `id` attribute in message")
	}
	ts, ok := node.Attrs["t"].(string)
	if !ok {
		return nil, fmt.Errorf("didn't find valid `t` (timestamp) attribute in message")
	}
	var err error
	info.Timestamp, err = strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("didn't find valid `t` (timestamp) attribute in message: %w", err)
	}

	info.Notify, _ = node.Attrs["notify"].(string)
	info.Category, _ = node.Attrs["category"].(string)

	return &info, nil
}

func (cli *Client) handleEncryptedMessage(node *waBinary.Node) bool {
	if node.Tag != "message" {
		return false
	}

	info, err := parseMessageInfo(node)
	if err != nil {
		cli.Log.Warnln("Failed to parse message:", err)
		return true
	}

	go cli.decryptMessages(info, node)

	return true
}

func (cli *Client) handleSenderKeyDistributionMessage(chat, from waBinary.FullJID, rawSKDMsg *waProto.SenderKeyDistributionMessage) {
	builder := groups.NewGroupSessionBuilder(cli.Session, pbSerializer)
	senderKeyName := protocol.NewSenderKeyName(chat.String(), from.SignalAddress())
	sdkMsg, err := protocol.NewSenderKeyDistributionMessageFromBytes(rawSKDMsg.AxolotlSenderKeyDistributionMessage, pbSerializer.SenderKeyDistributionMessage)
	if err != nil {
		cli.Log.Errorfln("Failed to parse sender key distribution message from %s for %s: %v", from, chat, err)
		return
	}
	builder.Process(senderKeyName, sdkMsg)
}

func (cli *Client) handleHistorySyncNotification(notif *waProto.HistorySyncNotification) {
	var historySync waProto.HistorySync
	if data, err := cli.downloadMedia(notif.GetDirectPath(), notif.FileEncSha256, notif.MediaKey, int(notif.GetFileLength()), whatsapp.MediaHistory, "md-msg-hist"); err != nil {
		cli.Log.Errorln("Failed to download history sync data:", err)
	} else if reader, err := zlib.NewReader(bytes.NewReader(data)); err != nil {
		cli.Log.Errorln("Failed to create zlib reader for history sync data:", err)
	} else if rawData, err := io.ReadAll(reader); err != nil {
		cli.Log.Errorln("Failed to decompress history sync data:", err)
	} else if err = proto.Unmarshal(rawData, &historySync); err != nil {
		cli.Log.Errorln("Failed to unmarshal history sync data:", err)
	} else {
		cli.Log.Debugln("Received history sync")
		fmt.Printf("%+v\n", &historySync)
		for _, conv := range historySync.GetConversations() {
			fmt.Println("  Conversation:", conv.GetId(), conv.GetName())
			for _, msg := range conv.GetMessages() {
				fmt.Println("    ", msg.Message)
			}
		}
	}
}

func (cli *Client) handleProtocolMessage(info *MessageInfo, msg *waProto.Message) {
	if msg.GetProtocolMessage().GetHistorySyncNotification() != nil {
		cli.handleHistorySyncNotification(msg.GetProtocolMessage().GetHistorySyncNotification())
		cli.sendProtocolMessageReceipt(info.ID, "hist_sync")
	}

	if info.Category == "peer" {
		cli.sendProtocolMessageReceipt(info.ID, "peer_msg")
	}
}

type Message struct {
	Info    *MessageInfo
	Message *waProto.Message
}

func (cli *Client) handleDecryptedMessage(info *MessageInfo, msg *waProto.Message) {
	fmt.Printf("Received message: %+v -- info: %+v\n", msg, info)
	if msg.GetSenderKeyDistributionMessage() != nil {
		cli.handleSenderKeyDistributionMessage(*info.Chat, info.From, msg.SenderKeyDistributionMessage)
	}
	if msg.GetProtocolMessage() != nil {
		cli.handleProtocolMessage(info, msg)
	}
	cli.dispatchEvent(&Message{info, msg})
}

func (cli *Client) sendProtocolMessageReceipt(id, msgType string) {
	if len(id) == 0 {
		return
	}
	err := cli.sendNode(waBinary.Node{
		Tag: "receipt",
		Attrs: map[string]interface{}{
			"id":   id,
			"type": msgType,
			"to":   waBinary.NewJID(cli.Session.ID.User, waBinary.UserServer),
		},
		Content: nil,
	})
	if err != nil {
		cli.Log.Warnfln("Failed to send acknowledgement for protocol message %s: %v", id, err)
	}
}

//func (cli *Client) ackMessage(info *MessageInfo) {
//	attrs := map[string]interface{}{
//		"class": "message",
//		"id":    info.ID,
//	}
//	if info.Chat != nil {
//		attrs["to"] = *info.Chat
//		// TODO is this really supposed to be the user instead of info.Participant?
//		attrs["participant"] = waBinary.NewADJID(cli.Session.ID.User, 0, 0)
//	} else {
//		attrs["to"] = waBinary.NewJID(cli.Session.ID.User, waBinary.UserServer)
//	}
//	err := cli.sendNode(waBinary.Node{
//		Tag:   "ack",
//		Attrs: attrs,
//	})
//	if err != nil {
//		cli.Log.Warnfln("Failed to send acknowledgement for %s: %v", info.ID, err)
//	}
//}

func (cli *Client) sendMessageReceipt(info *MessageInfo) {
	attrs := map[string]interface{}{
		"id": info.ID,
	}
	isFromMe := info.From.User == cli.Session.ID.User
	if isFromMe {
		attrs["type"] = "sender"
	} else {
		attrs["type"] = "inactive"
	}
	if info.Chat != nil {
		attrs["to"] = *info.Chat
		attrs["participant"] = info.From
	} else {
		attrs["to"] = info.From
		if isFromMe && info.Recipient != nil {
			attrs["recipient"] = *info.Recipient
		}
	}
	err := cli.sendNode(waBinary.Node{
		Tag:   "receipt",
		Attrs: attrs,
	})
	if err != nil {
		cli.Log.Warnfln("Failed to send receipt for %s: %v", info.ID, err)
	}
}

func (cli *Client) sendRetryReceipt(node *waBinary.Node) {
	id, _ := node.Attrs["id"].(string)

	cli.messageRetriesLock.Lock()
	cli.messageRetries[id]++
	retryCount := cli.messageRetries[id]
	cli.messageRetriesLock.Unlock()

	var registrationIDBytes [4]byte
	binary.BigEndian.PutUint16(registrationIDBytes[2:], cli.Session.RegistrationID)
	attrs := map[string]interface{}{
		"id":   id,
		"type": "retry",
		"to":   node.Attrs["from"],
	}
	if recipient, ok := node.Attrs["recipient"]; ok {
		attrs["recipient"] = recipient
	}
	if participant, ok := node.Attrs["participant"]; ok {
		attrs["participant"] = participant
	}
	payload := waBinary.Node{
		Tag:   "receipt",
		Attrs: attrs,
		Content: []waBinary.Node{
			{Tag: "retry", Attrs: map[string]interface{}{
				"count": retryCount,
				"id":    id,
				"t":     node.Attrs["t"],
				"v":     1,
			}},
			{Tag: "registration", Content: registrationIDBytes[:]},
		},
	}
	if retryCount > 1 {
		keys := cli.Session.GetOrGenPreKeys(1)
		deviceIdentity, err := proto.Marshal(cli.Session.Account)
		if err != nil {
			cli.Log.Errorln("Failed to marshal account info:", err)
			return
		}
		payload.Content = append(payload.GetChildren(), waBinary.Node{
			Tag: "keys",
			Content: []waBinary.Node{
				{Tag: "type", Content: []byte{ecc.DjbType}},
				{Tag: "identity", Content: cli.Session.IdentityKey.Pub[:]},
				preKeyToNode(keys[0]),
				preKeyToNode(cli.Session.SignedPreKey),
				{Tag: "device-identity", Content: deviceIdentity},
			},
		})
	}
	err := cli.sendNode(payload)
	if err != nil {
		cli.Log.Errorfln("Failed to send retry receipt for %s: %v", id, err)
	}
}