package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridge"
	"maunium.net/go/mautrix/bridge/bridgeconfig"
	"maunium.net/go/mautrix/bridge/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/util"

	"go.mau.fi/mautrix-signal/database"
	"go.mau.fi/mautrix-signal/pkg/signalmeow"
)

type portalSignalMessage struct {
	msg    string
	user   *User
	sender *Puppet
}

type portalMatrixMessage struct {
	evt  *event.Event
	user *User
}

type Portal struct {
	*database.Portal

	bridge *SignalBridge
	log    zerolog.Logger

	roomCreateLock sync.Mutex
	encryptLock    sync.Mutex

	signalMessages chan portalSignalMessage
	matrixMessages chan portalMatrixMessage

	recentMessages *util.RingBuffer[string, *signalmeow.Message]

	currentlyTyping     []id.UserID
	currentlyTypingLock sync.Mutex
}

const recentMessageBufferSize = 32

func init() {
	event.TypeMap[event.StateBridge] = reflect.TypeOf(CustomBridgeInfoContent{})
	event.TypeMap[event.StateHalfShotBridge] = reflect.TypeOf(CustomBridgeInfoContent{})
}

//** Interfaces that Portal implements **

var _ bridge.Portal = (*Portal)(nil)

//var _ bridge.ReadReceiptHandlingPortal = (*Portal)(nil)
//var _ bridge.MembershipHandlingPortal = (*Portal)(nil)
//var _ bridge.TypingPortal = (*Portal)(nil)
//var _ bridge.MetaHandlingPortal = (*Portal)(nil)
//var _ bridge.DisappearingPortal = (*Portal)(nil)

// ** bridge.Portal Interface **

func (portal *Portal) IsEncrypted() bool {
	return portal.Encrypted
}

func (portal *Portal) MarkEncrypted() {
	portal.Encrypted = true
	portal.Update()
}

func (portal *Portal) ReceiveMatrixEvent(user bridge.User, evt *event.Event) {
	if user.GetPermissionLevel() >= bridgeconfig.PermissionLevelUser {
		portal.matrixMessages <- portalMatrixMessage{user: user.(*User), evt: evt}
	}
}

func (portal *Portal) IsPrivateChat() bool {
	// Assuming that if the receiver is set, it's a private chat
	return portal.Receiver != ""
}

func (portal *Portal) MainIntent() *appservice.IntentAPI {
	if portal.IsPrivateChat() {
		return portal.bridge.GetPuppetBySignalID(portal.ChatID).DefaultIntent()
	}

	return portal.bridge.Bot
}

type CustomBridgeInfoContent struct {
	event.BridgeEventContent
	RoomType string `json:"com.beeper.room_type,omitempty"`
}

func (portal *Portal) getBridgeInfo() (string, CustomBridgeInfoContent) {
	bridgeInfo := event.BridgeEventContent{
		BridgeBot: portal.bridge.Bot.UserID,
		Creator:   portal.MainIntent().UserID,
		Protocol: event.BridgeInfoSection{
			ID:          "signal",
			DisplayName: "Signal",
			AvatarURL:   portal.bridge.Config.AppService.Bot.ParsedAvatar.CUString(),
			ExternalURL: "https://signal.org/",
		},
		Channel: event.BridgeInfoSection{
			ID:          portal.Key().ChatID,
			DisplayName: portal.Name,
		},
	}
	var bridgeInfoStateKey string
	bridgeInfoStateKey = fmt.Sprintf("fi.mau.signal://signal/%s", portal.Key().ChatID)
	bridgeInfo.Channel.ExternalURL = fmt.Sprintf("https://signal.me/#p/%s", portal.Key().ChatID)
	var roomType string
	if portal.IsPrivateChat() {
		roomType = "dm"
	}
	return bridgeInfoStateKey, CustomBridgeInfoContent{bridgeInfo, roomType}
}

func (portal *Portal) UpdateBridgeInfo() {
	if len(portal.MXID) == 0 {
		portal.log.Debug().Msg("Not updating bridge info: no Matrix room created")
		return
	}
	portal.log.Debug().Msg("Updating bridge info...")
	stateKey, content := portal.getBridgeInfo()
	_, err := portal.MainIntent().SendStateEvent(portal.MXID, event.StateBridge, stateKey, content)
	if err != nil {
		portal.log.Warn().Msgf("Failed to update m.bridge: %v", err)
	}
	// TODO remove this once https://github.com/matrix-org/matrix-doc/pull/2346 is in spec
	_, err = portal.MainIntent().SendStateEvent(portal.MXID, event.StateHalfShotBridge, stateKey, content)
	if err != nil {
		portal.log.Warn().Msgf("Failed to update uk.half-shot.bridge: %v", err)
	}
}

// ** bridge.ChildOverride methods (for SignalBridge in main.go) **

func (br *SignalBridge) GetAllIPortals() (iportals []bridge.Portal) {
	portals := br.getAllPortals()
	iportals = make([]bridge.Portal, len(portals))
	for i, portal := range portals {
		iportals[i] = portal
	}
	return iportals
}

func (br *SignalBridge) getAllPortals() []*Portal {
	return br.dbPortalsToPortals(br.DB.Portal.GetAll())
}

func (br *SignalBridge) dbPortalsToPortals(dbPortals []*database.Portal) []*Portal {
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()

	output := make([]*Portal, len(dbPortals))
	for index, dbPortal := range dbPortals {
		if dbPortal == nil {
			continue
		}

		portal, ok := br.portalsByID[dbPortal.Key()]
		if !ok {
			portal = br.loadPortal(dbPortal, nil)
		}

		output[index] = portal
	}

	return output
}

// ** Portal Creation and Message Handling **

func (br *SignalBridge) NewPortal(dbPortal *database.Portal) *Portal {
	portal := &Portal{
		Portal: dbPortal,
		bridge: br,
		log:    br.ZLog.With().Str("chat_id", dbPortal.Key().ChatID).Logger(),

		signalMessages: make(chan portalSignalMessage, br.Config.Bridge.PortalMessageBuffer),
		matrixMessages: make(chan portalMatrixMessage, br.Config.Bridge.PortalMessageBuffer),

		//recentMessages: util.NewRingBuffer[string, *discordgo.Message](recentMessageBufferSize),
		//commands: make(map[string]*discordgo.ApplicationCommand),
	}

	go portal.messageLoop()

	return portal
}

func (portal *Portal) messageLoop() {
	for {
		portal.log.Debug().Msg("Waiting for message")
		select {
		case msg := <-portal.matrixMessages:
			portal.log.Debug().Msg("Got message from matrix")
			portal.handleMatrixMessages(msg)
		case msg := <-portal.signalMessages:
			portal.log.Debug().Msg("Got message from signal")
			portal.handleSignalMessages(msg)
		}
	}
}

func (portal *Portal) handleMatrixMessages(msg portalMatrixMessage) {
	switch msg.evt.Type {
	case event.EventMessage: //, event.EventSticker:
		portal.handleMatrixMessage(msg.user, msg.evt)
	case event.EventRedaction:
		//portal.handleMatrixRedaction(msg.user, msg.evt)
	case event.EventReaction:
		//portal.handleMatrixReaction(msg.user, msg.evt)
	default:
		portal.log.Warn().Str("type", msg.evt.Type.String()).Msg("Unhandled matrix message type")
	}
}

type messageTimings struct {
	initReceive  time.Duration
	decrypt      time.Duration
	implicitRR   time.Duration
	totalReceive time.Duration

	preproc   time.Duration
	convert   time.Duration
	totalSend time.Duration
}

func (portal *Portal) handleMatrixMessage(sender *User, evt *event.Event) {
	evtTS := time.UnixMilli(evt.Timestamp)
	timings := messageTimings{
		initReceive:  evt.Mautrix.ReceivedAt.Sub(evtTS),
		decrypt:      evt.Mautrix.DecryptionDuration,
		totalReceive: time.Since(evtTS),
	}
	implicitRRStart := time.Now()
	//portal.handleMatrixReadReceipt(msg.user, "", evtTS, false)
	timings.implicitRR = time.Since(implicitRRStart)
	start := time.Now()

	messageAge := timings.totalReceive
	//origEvtID := evt.ID
	//var dbMsg *database.Message
	//if retryMeta := evt.Content.AsMessage().MessageSendRetry; retryMeta != nil {
	//	origEvtID = retryMeta.OriginalEventID
	//	dbMsg = portal.bridge.DB.Message.GetByMXID(origEvtID)
	//	if dbMsg != nil && dbMsg.Sent {
	//		portal.log.Debugfln("Ignoring retry request %s (#%d, age: %s) for %s/%s from %s as message was already sent", evt.ID, retryMeta.RetryCount, messageAge, origEvtID, dbMsg.JID, evt.Sender)
	//		go ms.sendMessageMetrics(evt, nil, "", true)
	//		return
	//	} else if dbMsg != nil {
	//		portal.log.Debugfln("Got retry request %s (#%d, age: %s) for %s/%s from %s", evt.ID, retryMeta.RetryCount, messageAge, origEvtID, dbMsg.JID, evt.Sender)
	//	} else {
	//		portal.log.Debugfln("Got retry request %s (#%d, age: %s) for %s from %s (original message not known)", evt.ID, retryMeta.RetryCount, messageAge, origEvtID, evt.Sender)
	//	}
	//} else {
	//	portal.log.Debugfln("Received message %s from %s (age: %s)", evt.ID, evt.Sender, messageAge)
	//}
	portal.log.Debug().Msgf("Received message %s from %s (age: %s)", evt.ID, evt.Sender, messageAge)

	errorAfter := portal.bridge.Config.Bridge.MessageHandlingTimeout.ErrorAfter
	deadline := portal.bridge.Config.Bridge.MessageHandlingTimeout.Deadline
	isScheduled, _ := evt.Content.Raw["com.beeper.scheduled"].(bool)
	if isScheduled {
		portal.log.Debug().Msgf("%s is a scheduled message, extending handling timeouts", evt.ID)
		errorAfter *= 10
		deadline *= 10
	}

	if errorAfter > 0 {
		remainingTime := errorAfter - messageAge
		if remainingTime < 0 {
			//go ms.sendMessageMetrics(evt, errTimeoutBeforeHandling, "Timeout handling", true)
			return
		} else if remainingTime < 1*time.Second {
			portal.log.Warn().Msgf("Message %s was delayed before reaching the bridge, only have %s (of %s timeout) until delay warning", evt.ID, remainingTime, errorAfter)
		}
		go func() {
			time.Sleep(remainingTime)
			//ms.sendMessageMetrics(evt, errMessageTakingLong, "Timeout handling", false)
		}()
	}

	ctx := context.Background()
	if deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}

	timings.preproc = time.Since(start)
	start = time.Now()
	//msg, sender, extraMeta, err := portal.convertMatrixMessage(ctx, sender, evt)
	msg := evt.Content.AsMessage().Body
	recipientSignalID := portal.ChatID
	timings.convert = time.Since(start)
	//if msg == nil {
	//	go ms.sendMessageMetrics(evt, err, "Error converting", true)
	//	return
	//}
	//dbMsgType := database.MsgNormal
	//if msg.PollCreationMessage != nil || msg.PollCreationMessageV2 != nil || msg.PollCreationMessageV3 != nil {
	//	dbMsgType = database.MsgMatrixPoll
	//} else if msg.EditedMessage == nil {
	//	portal.MarkDisappearing(nil, origEvtID, time.Duration(portal.ExpirationTime)*time.Second, time.Now())
	//} else {
	//	dbMsgType = database.MsgEdit
	//}
	//info := portal.generateMessageInfo(sender)
	//if dbMsg == nil {
	//	dbMsg = portal.markHandled(nil, nil, info, evt.ID, evt.Sender, false, true, dbMsgType, database.MsgNoError)
	//} else {
	//	info.ID = dbMsg.JID
	//}
	//if dbMsgType == database.MsgMatrixPoll && extraMeta != nil && extraMeta.PollOptions != nil {
	//	dbMsg.PutPollOptions(extraMeta.PollOptions)
	//}
	portal.log.Debug().Msgf("Sending event %s to Signal %s", evt.ID, recipientSignalID)
	start = time.Now()

	// Check to see if recipientSignalID is a standard UUID (with dashes)
	var err error
	if _, uuidErr := uuid.Parse(recipientSignalID); uuidErr == nil {
		// this is a 1:1 chat
		result := signalmeow.SendMessage(ctx, sender.SignalDevice, recipientSignalID, msg)
		if !result.WasSuccessful {
			err = result.FailedSendResult.Error
			portal.log.Error().Msgf("Error sending event %s to Signal %s: %s", evt.ID, recipientSignalID, err)
		}
	} else {
		// this is a group chat
		groupID := signalmeow.GroupID(recipientSignalID)
		result, err := signalmeow.SendGroupMessage(ctx, sender.SignalDevice, groupID, msg)
		if err != nil {
			portal.log.Error().Msgf("Error sending event %s to Signal group %s: %s", evt.ID, recipientSignalID, err)
		}
		totalRecipients := len(result.FailedToSendTo) + len(result.SuccessfullySentTo)
		if len(result.FailedToSendTo) > 0 {
			portal.log.Error().Msgf("Failed to send event %s to %d of %d members of Signal group %s", evt.ID, len(result.FailedToSendTo), totalRecipients, recipientSignalID)
		}
		if len(result.SuccessfullySentTo) == 0 {
			portal.log.Error().Msgf("Failed to send event %s to all %d members of Signal group %s", evt.ID, totalRecipients, recipientSignalID)
			err = errors.New("failed to send to any members of Signal group")
		} else if len(result.SuccessfullySentTo) < totalRecipients {
			portal.log.Warn().Msgf("Only sent event %s to %d of %d members of Signal group %s", evt.ID, len(result.SuccessfullySentTo), totalRecipients, recipientSignalID)
		} else {
			portal.log.Debug().Msgf("Sent event %s to all %d members of Signal group %s", evt.ID, totalRecipients, recipientSignalID)
		}
	}
	timings.totalSend = time.Since(start)
	//go ms.sendMessageMetrics(evt, err, "Error sending", true)
	if err == nil {
		//dbMsg.MarkSent(resp.Timestamp)
		dbMessage := portal.bridge.DB.Message.New()
		dbMessage.MXID = evt.ID
		dbMessage.MXRoom = portal.MXID
		dbMessage.Sender = sender.SignalID
		dbMessage.Timestamp = start // TODO: get timestamp from signal
		dbMessage.SignalChatID = portal.ChatID
		dbMessage.SignalReceiver = portal.Receiver
		dbMessage.Insert(nil)
	}
}

func (portal *Portal) sendMessageMetrics(evt *event.Event, err error, part string) {
	//var msgType string
	//switch evt.Type {
	//case event.EventMessage, event.EventSticker:
	//	msgType = "message"
	//case event.EventReaction:
	//	msgType = "reaction"
	//case event.EventRedaction:
	//	msgType = "redaction"
	//default:
	//	msgType = "unknown event"
	//}
	level := zerolog.DebugLevel
	if err != nil && part != "Ignoring" {
		level = zerolog.ErrorLevel
	}
	logEvt := portal.bridge.ZLog.WithLevel(level).
		Str("action", "send matrix message metrics").
		Str("event_type", evt.Type.Type).
		Str("event_id", evt.ID.String()).
		Str("sender", evt.Sender.String())
	if evt.Type == event.EventRedaction {
		logEvt.Str("redacts", evt.Redacts.String())
	}
	if err != nil {
		logEvt.Err(err).
			Str("result", fmt.Sprintf("%s event", part)).
			Msg("Matrix event not handled")
		//reason, statusCode, isCertain, sendNotice, humanMessage, checkpointErr := errorToStatusReason(err)
		//if checkpointErr == nil {
		//	checkpointErr = err
		//}
		//checkpointStatus := status.ReasonToCheckpointStatus(reason, statusCode)
		//portal.bridge.SendMessageCheckpoint(evt, status.MsgStepRemote, checkpointErr, checkpointStatus, 0)
		//if sendNotice {
		//	if humanMessage == "" {
		//		humanMessage = err.Error()
		//	}
		//	portal.sendErrorMessage(msgType, humanMessage, isCertain)
		//}
		//portal.sendStatusEvent(evt.ID, err)
	} else {
		logEvt.Err(err).Msg("Matrix event handled successfully")
		portal.sendDeliveryReceipt(evt.ID)
		portal.bridge.SendMessageSuccessCheckpoint(evt, status.MsgStepRemote, 0)
		portal.sendStatusEvent(evt.ID, nil)
	}
}

func (portal *Portal) sendDeliveryReceipt(eventID id.EventID) {
	if portal.bridge.Config.Bridge.DeliveryReceipts {
		err := portal.bridge.Bot.MarkRead(portal.MXID, eventID)
		if err != nil {
			portal.log.Warn().Err(err).
				Str("event_id", eventID.String()).
				Msg("Failed to send delivery receipt")
		}
	}
}

func (portal *Portal) sendStatusEvent(evtID id.EventID, err error) {
	if !portal.bridge.Config.Bridge.MessageStatusEvents {
		return
	}
	intent := portal.bridge.Bot
	if !portal.Encrypted {
		// Bridge bot isn't present in unencrypted DMs
		intent = portal.MainIntent()
	}
	stateKey, _ := portal.getBridgeInfo()
	content := event.BeeperMessageStatusEventContent{
		Network: stateKey,
		RelatesTo: event.RelatesTo{
			Type:    event.RelReference,
			EventID: evtID,
		},
		Status: event.MessageStatusSuccess,
	}
	if err == nil {
		content.Status = event.MessageStatusSuccess
	} else {
		var checkpointErr error
		//content.Reason, content.Status, _, _, content.Message, checkpointErr = errorToStatusReason(err)
		if checkpointErr != nil {
			content.Error = checkpointErr.Error()
		} else {
			content.Error = err.Error()
		}
	}
	_, err = intent.SendMessageEvent(portal.MXID, event.BeeperMessageStatus, &content)
	if err != nil {
		portal.log.Err(err).Str("event_id", evtID.String()).Msg("Failed to send message status event")
	}
}

func (portal *Portal) handleSignalMessages(msg portalSignalMessage) {
	if portal.MXID == "" {
		portal.log.Debug().Msg("Creating Matrix room from incoming message")
		if err := portal.CreateMatrixRoom(msg.user, nil); err != nil {
			portal.log.Error().Err(err).Msg("Failed to create portal room")
			return
		} else {
			portal.log.Info().Msgf("Created matrix room: %s", portal.MXID)
		}
	}

	//intent := portal.getMessageIntent(msg.user, msg.sender)
	intent := msg.sender.IntentFor(portal)
	if intent == nil {
		portal.log.Error().Msg("Failed to get message intent")
		return
	}

	timestamp := time.Now() //TODO get this from signal message
	content := &event.MessageEventContent{
		Body:    msg.msg,
		MsgType: event.MsgText,
	}
	resp, err := portal.sendMessage(
		intent,
		event.EventMessage,
		content,
		nil,
		timestamp.UnixMilli(), // TODO: message timestamp from Signal
	)
	if err != nil {
		portal.log.Error().Err(err).Msg("Failed to send message")
		return
	}
	eventID := resp.EventID
	if eventID == "" {
		portal.log.Error().Err(err).Msg("Failed to send message, no event ID")
		return
	}

	portal.log.Info().Msgf("**** sender: %v", msg.sender)
	portal.log.Info().Msgf("**** sender.signalid: %v", msg.sender.SignalID)
	dbMessage := portal.bridge.DB.Message.New()
	dbMessage.MXID = eventID
	dbMessage.MXRoom = portal.MXID
	dbMessage.Sender = msg.sender.SignalID
	dbMessage.Timestamp = timestamp
	dbMessage.SignalChatID = portal.ChatID
	dbMessage.SignalReceiver = portal.Receiver
	dbMessage.Insert(nil)

	// TODO: send receipt
	// TODO: expire if it's an expiring message

	//switch convertedMsg := msg.msg.(type) {
	//case *discordgo.MessageCreate:
	//		portal.handleDiscordMessageCreate(msg.user, convertedMsg.Message, msg.thread)
	//case *discordgo.MessageUpdate:
	//		portal.handleDiscordMessageUpdate(msg.user, convertedMsg.Message)
	//case *discordgo.MessageDelete:
	//		portal.handleDiscordMessageDelete(msg.user, convertedMsg.Message)
	//case *discordgo.MessageReactionAdd:
	//		portal.handleDiscordReaction(msg.user, convertedMsg.MessageReaction, true, msg.thread, convertedMsg.Member)
	//case *discordgo.MessageReactionRemove:
	//		portal.handleDiscordReaction(msg.user, convertedMsg.MessageReaction, false, msg.thread, nil)
	//default:
	//		portal.log.Warnln("unknown message type")
	//}
}

func (portal *Portal) sendMainIntentMessage(content *event.MessageEventContent) (*mautrix.RespSendEvent, error) {
	return portal.sendMessage(portal.MainIntent(), event.EventMessage, content, nil, 0)
}

func (portal *Portal) encrypt(intent *appservice.IntentAPI, content *event.Content, eventType event.Type) (event.Type, error) {
	if !portal.Encrypted || portal.bridge.Crypto == nil {
		return eventType, nil
	}
	intent.AddDoublePuppetValue(content)
	// TODO maybe the locking should be inside mautrix-go?
	portal.encryptLock.Lock()
	defer portal.encryptLock.Unlock()
	err := portal.bridge.Crypto.Encrypt(portal.MXID, eventType, content)
	if err != nil {
		return eventType, fmt.Errorf("failed to encrypt event: %w", err)
	}
	return event.EventEncrypted, nil
}

func (portal *Portal) sendMessage(intent *appservice.IntentAPI, eventType event.Type, content *event.MessageEventContent, extraContent map[string]interface{}, timestamp int64) (*mautrix.RespSendEvent, error) {
	wrappedContent := event.Content{Parsed: content, Raw: extraContent}
	var err error
	eventType, err = portal.encrypt(intent, &wrappedContent, eventType)
	if err != nil {
		return nil, err
	}

	_, _ = intent.UserTyping(portal.MXID, false, 0)
	if timestamp == 0 {
		return intent.SendMessageEvent(portal.MXID, eventType, &wrappedContent)
	} else {
		return intent.SendMassagedMessageEvent(portal.MXID, eventType, &wrappedContent, timestamp)
	}
}

func (portal *Portal) getMessagePuppet(user *User, senderUUID string) (puppet *Puppet) {
	//if info.IsFromMe {
	//return portal.bridge.GetPuppetBySignalID(user.SignalID)
	if portal.IsPrivateChat() {
		puppet = portal.bridge.GetPuppetBySignalID(portal.ChatID)
	} else if senderUUID != "" {
		puppet = portal.bridge.GetPuppetBySignalID(senderUUID)
	}
	if puppet == nil {
		//	portal.log.Warnfln("Message %+v doesn't seem to have a valid sender (%s): puppet is nil", *info, info.Sender)
		return nil
	}
	//user.EnqueuePortalResync(portal)
	//puppet.SyncContact(user, true, true, "handling message")
	return puppet
}

func (portal *Portal) getMessageIntent(user *User, senderUUID string) *appservice.IntentAPI {
	puppet := portal.getMessagePuppet(user, senderUUID)
	if puppet == nil {
		portal.log.Debug().Msg("Not handling: puppet is nil")
		return nil
	}
	intent := puppet.IntentFor(portal)
	//if !intent.IsCustomPuppet && portal.IsPrivateChat() { //&& info.Sender.User == portal.Key.Receiver.User && portal.Key.Receiver != portal.Key.JID {
	//	portal.log.Debugfln("Not handling: user doesn't have double puppeting enabled")
	//	return nil
	//}
	return intent
}

func (portal *Portal) getEncryptionEventContent() (evt *event.EncryptionEventContent) {
	evt = &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}
	if rot := portal.bridge.Config.Bridge.Encryption.Rotation; rot.EnableCustom {
		evt.RotationPeriodMillis = rot.Milliseconds
		evt.RotationPeriodMessages = rot.Messages
	}
	return
}

func (portal *Portal) shouldSetDMRoomMetadata() bool {
	return !portal.IsPrivateChat() || portal.bridge.Config.Bridge.PrivateChatPortalMeta
}

func (portal *Portal) ensureUserInvited(user *User) bool {
	return user.ensureInvited(portal.MainIntent(), portal.MXID, portal.IsPrivateChat())
}

func (portal *Portal) CreateMatrixRoom(user *User, meta *any) error {
	portal.roomCreateLock.Lock()
	defer portal.roomCreateLock.Unlock()
	if portal.MXID != "" {
		portal.log.Debug().Msg("Not creating room: already exists")
		return nil
	}
	portal.log.Debug().Msg("Creating room")

	//meta = portal.UpdateInfo(user, meta)
	//if meta == nil {
	//	return fmt.Errorf("didn't find metadata")
	//}

	intent := portal.MainIntent()

	if err := intent.EnsureRegistered(); err != nil {
		portal.log.Error().Err(err).Msg("failed to ensure registered")
		return err
	}

	bridgeInfoStateKey, bridgeInfo := portal.getBridgeInfo()
	initialState := []*event.Event{{
		Type:     event.StateBridge,
		Content:  event.Content{Parsed: bridgeInfo},
		StateKey: &bridgeInfoStateKey,
	}, {
		// TODO remove this once https://github.com/matrix-org/matrix-doc/pull/2346 is in spec
		Type:     event.StateHalfShotBridge,
		Content:  event.Content{Parsed: bridgeInfo},
		StateKey: &bridgeInfoStateKey,
	}}

	if !portal.AvatarURL.IsEmpty() {
		initialState = append(initialState, &event.Event{
			Type: event.StateRoomAvatar,
			Content: event.Content{Parsed: &event.RoomAvatarEventContent{
				URL: portal.AvatarURL,
			}},
		})
	}

	creationContent := make(map[string]interface{})
	if !portal.bridge.Config.Bridge.FederateRooms {
		creationContent["m.federate"] = false
	}

	var invite []id.UserID

	if portal.bridge.Config.Bridge.Encryption.Default {
		initialState = append(initialState, &event.Event{
			Type: event.StateEncryption,
			Content: event.Content{
				Parsed: portal.getEncryptionEventContent(),
			},
		})
		portal.Encrypted = true

		if portal.IsPrivateChat() {
			invite = append(invite, portal.bridge.Bot.UserID)
		}
	}

	resp, err := intent.CreateRoom(&mautrix.ReqCreateRoom{
		Visibility:      "private",
		Name:            portal.Name,
		Topic:           portal.Topic,
		Invite:          invite,
		Preset:          "private_chat",
		IsDirect:        portal.IsPrivateChat(),
		InitialState:    initialState,
		CreationContent: creationContent,
	})
	if err != nil {
		portal.log.Warn().Err(err).Msg("failed to create room")
		return err
	}

	portal.NameSet = true
	//portal.TopicSet = true
	portal.AvatarSet = !portal.AvatarURL.IsEmpty()
	portal.MXID = resp.RoomID
	portal.bridge.portalsLock.Lock()
	portal.bridge.portalsByMXID[portal.MXID] = portal
	portal.bridge.portalsLock.Unlock()
	portal.Update()
	portal.log.Info().Msgf("Created matrix room %s", portal.MXID)

	if portal.Encrypted && portal.IsPrivateChat() {
		err = portal.bridge.Bot.EnsureJoined(portal.MXID, appservice.EnsureJoinedParams{BotOverride: portal.MainIntent().Client})
		if err != nil {
			portal.log.Error().Err(err).Msg("Failed to ensure bridge bot is joined to private chat portal")
		}
	}

	user.ensureInvited(portal.MainIntent(), portal.MXID, portal.IsPrivateChat())
	user.syncChatDoublePuppetDetails(portal, true)

	//portal.syncParticipants(user, channel.Recipients)

	if portal.IsPrivateChat() {
		portal.log.Debug().Msgf("Portal is private chat, updating direct chats: %s", portal.MXID)
		puppet := user.bridge.GetPuppetBySignalID(portal.Receiver)
		if puppet == nil {
			portal.log.Error().Msgf("Failed to find puppet for portal receiver %s", portal.Receiver)
			return nil
		}

		chats := map[id.UserID][]id.RoomID{puppet.MXID: {portal.MXID}}
		user.UpdateDirectChats(chats)
	}

	_, err = portal.MainIntent().SendMessageEvent(portal.MXID, portalCreationDummyEvent, struct{}{})
	if err != nil {
		portal.log.Error().Err(err).Msg("Failed to send dummy event to mark portal creation")
	} else {
		portal.log.Debug().Msg("Sent dummy event to mark portal creation")
		portal.Update()
	}

	return nil
}

func (portal *Portal) UpdateInfo(user *User, meta *any) *any {
	return nil
}

// ** Portal loading and fetching **
var (
	portalCreationDummyEvent = event.Type{Type: "fi.mau.dummy.portal_created", Class: event.MessageEventType}
)

func (br *SignalBridge) loadPortal(dbPortal *database.Portal, key *database.PortalKey) *Portal {
	if dbPortal == nil {
		if key == nil {
			br.Log.Errorln("loadPortal called with nil dbPortal and nil key")
			return nil
		}

		dbPortal = br.DB.Portal.New()
		dbPortal.SetPortalKey(*key)
		err := dbPortal.Insert()
		if err != nil {
			br.Log.Errorln("Failed to insert new portal:", err)
			return nil
		}
	}

	portal := br.NewPortal(dbPortal)

	br.portalsByID[portal.Key()] = portal
	if portal.MXID != "" {
		br.portalsByMXID[portal.MXID] = portal
	}

	return portal
}

func (br *SignalBridge) GetPortalByMXID(mxid id.RoomID) *Portal {
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()

	portal, ok := br.portalsByMXID[mxid]
	if !ok {
		return br.loadPortal(br.DB.Portal.GetByMXID(mxid), nil)
	}

	return portal
}

func (br *SignalBridge) GetPortalByChatID(key database.PortalKey) *Portal {
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()
	portal, ok := br.portalsByID[key]
	if !ok {
		return br.loadPortal(br.DB.Portal.GetByChatID(key), &key)
	}
	return portal
}
