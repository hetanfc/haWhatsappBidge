package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"
)

type WhatsApp struct {
	cfg    Config
	tr     *Tracker
	log    *slog.Logger
	cli    *whatsmeow.Client
	db     *sql.DB
	target types.JID // phone-number JID of the contact
	lid    types.JID // LID alias of the same contact, when resolvable
	kick   chan struct{}
	seen   map[string]bool // other chats already logged, to keep the log quiet
	sent   *sentLog        // our outgoing messages, to give receipts a subject
	pruned time.Time       // last cleanup of the sent log

	pairFailed atomic.Bool // pairing by code failed: fall back to printing QR codes
}

// WhatsApp validates the pairing display name server-side and rejects anything
// that isn't a common "Browser (OS)" pair with a 400, so this is not cosmetic
// and must not be replaced with a nicer name. What shows up in the phone's
// linked-devices list is store.DeviceProps.Os, set below.
const pairDisplayName = "Chrome (Linux)"

func NewWhatsApp(ctx context.Context, cfg Config, tr *Tracker, log *slog.Logger) (*WhatsApp, error) {
	if dir := filepath.Dir(cfg.DBPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}

	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)", cfg.DBPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open session store: %w", err)
	}
	db.SetMaxOpenConns(1)

	waLogger := waLog.Stdout("whatsmeow", waLevel(cfg.LogLevel), true)
	container := sqlstore.NewWithDB(db, "sqlite3", waLogger)
	if err := container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("upgrade session store: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("load device: %w", err)
	}

	// How this shows up in WhatsApp > Linked devices.
	store.DeviceProps.Os = proto.String("Home Assistant")

	w := &WhatsApp{
		cfg:  cfg,
		tr:   tr,
		log:  log,
		db:   db,
		kick: make(chan struct{}, 1),
		seen: map[string]bool{},
	}

	if cfg.JIDOverride != "" {
		jid, err := types.ParseJID(cfg.JIDOverride)
		if err != nil {
			return nil, fmt.Errorf("parse WT_JID: %w", err)
		}
		w.target = jid
	} else {
		w.target = types.NewJID(cfg.Phone, types.DefaultUserServer)
	}

	sent, err := newSentLog(ctx, db, log)
	if err != nil {
		return nil, err
	}
	sent.prune(ctx)
	w.sent = sent
	w.pruned = time.Now()

	w.cli = whatsmeow.NewClient(device, waLogger)
	w.cli.AddEventHandler(w.handleEvent)
	return w, nil
}

func (w *WhatsApp) Start(ctx context.Context) error {
	go w.maintain(ctx)

	needsLogin := w.cli.Store.ID == nil
	firstQR := make(chan struct{}, 1)

	if needsLogin {
		qrChan, err := w.cli.GetQRChannel(ctx)
		if err != nil {
			return fmt.Errorf("qr channel: %w", err)
		}
		go func() {
			for evt := range qrChan {
				switch evt.Event {
				case "code":
					select {
					case firstQR <- struct{}{}:
					default:
					}
					if w.cfg.PairPhone != "" && !w.pairFailed.Load() {
						// Pairing by code: the QR would only add noise.
						continue
					}
					fmt.Println()
					fmt.Println("=== Inquadra questo QR con WhatsApp > Dispositivi collegati ===")
					qrterminal.GenerateWithConfig(evt.Code, qrterminal.Config{
						Level:     qrterminal.L,
						Writer:    os.Stdout,
						BlackChar: qrterminal.BLACK,
						WhiteChar: qrterminal.WHITE,
						QuietZone: 1,
					})
					fmt.Printf("\nSe il QR sopra non e' leggibile, genera l'immagine da questo testo:\n%s\n\n", evt.Code)
				case "success":
					w.log.Info("device linked to whatsapp")
				case "timeout":
					if w.cfg.PairPhone != "" {
						w.log.Warn("pairing code expired, restart the add-on to get a new one")
					} else {
						w.log.Warn("qr code expired, a new one will be generated")
					}
				default:
					w.log.Debug("qr event", "event", evt.Event)
				}
			}
		}()
	}

	if err := w.cli.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	// Pairing by code: no QR to scan, you type the code into the phone.
	if needsLogin && w.cfg.PairPhone != "" {
		// The server rejects the request until the login handshake is done, so
		// wait for the first QR event: that's the signal the socket is ready.
		select {
		case <-firstQR:
		case <-ctx.Done():
			return nil
		case <-time.After(15 * time.Second):
			w.log.Warn("no qr event within 15s, requesting the pairing code anyway")
		}

		code, err := w.cli.PairPhone(ctx, w.cfg.PairPhone, true, whatsmeow.PairClientChrome, pairDisplayName)
		if err != nil {
			// Never fatal: without a fallback you'd be left with no way to log in.
			w.pairFailed.Store(true)
			w.log.Error("pairing by code failed, falling back to the QR code (it shows up here within ~20s)",
				"err", err, "pair_phone", w.cfg.PairPhone)
		} else {
			fmt.Println()
			fmt.Println("=== ACCOPPIAMENTO WHATSAPP ===")
			fmt.Println("Sul telefono: WhatsApp > Dispositivi collegati > Collega un dispositivo >")
			fmt.Println("\"Collega con numero di telefono\", poi digita questo codice:")
			fmt.Printf("\n        %s\n\n", code)
			fmt.Println("Il codice scade dopo pochi minuti: se non fai in tempo, riavvia l'add-on.")
		}
	}

	w.log.Info("whatsapp client started", "target", w.target.String())
	return nil
}

func (w *WhatsApp) Close() {
	w.tr.SetConnected(false)
	w.cli.Disconnect()
	_ = w.db.Close()
}

func (w *WhatsApp) handleEvent(rawEvt any) {
	switch evt := rawEvt.(type) {
	case *events.Connected:
		w.log.Info("connected to whatsapp")
		w.tr.SetConnected(true)
		w.poke()

	case *events.PushNameSetting:
		w.poke()

	case *events.Disconnected:
		w.log.Warn("disconnected from whatsapp, reconnecting")
		w.tr.SetConnected(false)

	case *events.StreamReplaced:
		w.log.Error("stream replaced: another client took over this session")
		w.tr.SetConnected(false)

	case *events.LoggedOut:
		w.log.Error("logged out of whatsapp, delete the session db and scan the QR again", "reason", evt.Reason.String())
		w.tr.SetConnected(false)

	case *events.ChatPresence:
		w.onChatPresence(evt)

	case *events.Receipt:
		w.onReceipt(evt)

	case *events.Message:
		if !w.matches(evt.Info.Chat) {
			return
		}
		if evt.Info.IsFromMe {
			// Our own message, synced from the phone: remembered only so the
			// receipts coming back can say what they refer to.
			w.sent.add(context.Background(), evt.Info.ID, evt.Info.Timestamp, messageKind(evt.Message))
			return
		}
		// She hit send: whatever typing session was open is over now.
		kind := messageKind(evt.Message)
		w.log.Debug("message received", "kind", kind)
		w.tr.Send(Event{Kind: EvMessage, Media: kind, At: time.Now()})
	}
}

// messageKind labels an incoming message by type. Only the type is ever looked
// at: the content is never read, logged or published.
func messageKind(msg *waE2E.Message) string {
	switch {
	case msg == nil:
		return "altro"
	case msg.GetPtvMessage() != nil:
		return "videomessaggio"
	case msg.GetAudioMessage() != nil:
		if msg.GetAudioMessage().GetPTT() {
			return "vocale"
		}
		return "audio"
	case msg.GetImageMessage() != nil:
		return "foto"
	case msg.GetVideoMessage() != nil:
		if msg.GetVideoMessage().GetGifPlayback() {
			return "gif"
		}
		return "video"
	case msg.GetStickerMessage() != nil:
		return "sticker"
	case msg.GetDocumentMessage() != nil:
		return "documento"
	case msg.GetLocationMessage() != nil:
		return "posizione"
	case msg.GetContactMessage() != nil:
		return "contatto"
	case msg.GetConversation() != "", msg.GetExtendedTextMessage() != nil:
		return "testo"
	default:
		return "altro"
	}
}

func (w *WhatsApp) onChatPresence(evt *events.ChatPresence) {
	chat := evt.MessageSource.Chat
	if !w.matches(chat) {
		// Logged once per chat: this is how you find the right JID when the
		// contact is addressed over @lid instead of the phone number.
		if !w.seen[chat.String()] {
			w.seen[chat.String()] = true
			w.log.Info("chat presence from another chat (ignored)", "jid", chat.String(), "state", string(evt.State))
		}
		return
	}

	media := "text"
	if evt.Media == types.ChatPresenceMediaAudio {
		media = "audio"
	}
	w.log.Debug("chat presence", "state", string(evt.State), "media", media)

	switch evt.State {
	case types.ChatPresenceComposing:
		w.tr.Send(Event{Kind: EvComposing, Media: media, At: time.Now()})
	case types.ChatPresencePaused:
		w.tr.Send(Event{Kind: EvPaused, At: time.Now()})
	}
}

// onReceipt turns the ticks on our own outgoing messages into events: delivered
// (her phone was reachable), read (she opened the chat), played (she listened to
// a voice note or watched a video message).
//
// The "self" variants are our own devices reporting what we read, not her, so
// they are ignored. Regular video and image files never produce a played
// receipt: for those the blue ticks only say the chat was opened.
func (w *WhatsApp) onReceipt(evt *events.Receipt) {
	if !receiptFromContact(evt) {
		if evt != nil {
			w.log.Debug("receipt from own device ignored",
				"type", string(evt.Type), "chat", evt.Chat.String())
		}
		return
	}
	if !w.matches(evt.MessageSource.Chat) {
		return
	}

	at := evt.Timestamp
	if at.IsZero() {
		at = time.Now()
	}

	var kind EventKind
	switch evt.Type {
	case types.ReceiptTypeDelivered:
		kind = EvDelivered
	case types.ReceiptTypeRead:
		kind = EvRead
	case types.ReceiptTypePlayed:
		kind = EvPlayed
	default:
		w.log.Debug("receipt ignored", "type", string(evt.Type))
		return
	}

	ctx := context.Background()
	ids := make([]string, 0, len(evt.MessageIDs))
	for _, id := range evt.MessageIDs {
		ids = append(ids, string(id))
	}

	known := w.sent.lookup(ctx, ids)
	ev := Event{
		Kind:   kind,
		At:     at,
		Target: describeTargets(known, len(ids), time.Now()),
	}
	if len(known) > 0 {
		ev.Media = known[0].Kind
	}
	if kind == EvPlayed {
		// Only played receipts repeat in practice, and that repetition is the
		// interesting part: it means the same clip was opened again.
		ev.Repeat = w.sent.bump(ctx, ids, string(evt.Type))
	}

	w.log.Debug("receipt", "type", string(evt.Type), "messages", len(ids),
		"target", ev.Target, "repeat", ev.Repeat, "at", at.Format(time.RFC3339))
	w.tr.Send(ev)
}

// receiptFromContact rejects receipts generated by one of our own devices.
// Those can use the regular "read" type too, not only "read-self", so
// checking the receipt type alone causes false "the contact read it" events.
func receiptFromContact(evt *events.Receipt) bool {
	return evt != nil && !evt.IsFromMe && evt.Type != types.ReceiptTypeReadSelf
}

func (w *WhatsApp) matches(jid types.JID) bool {
	if jid.IsEmpty() {
		return false
	}
	if jid.User == w.target.User {
		return true
	}
	if !w.lid.IsEmpty() && jid.User == w.lid.User {
		return true
	}
	return false
}

func (w *WhatsApp) poke() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// maintain keeps the presence subscription alive. WhatsApp only pushes presence
// (including typing) to clients that declared themselves available, and the
// subscription itself expires, so both are refreshed periodically.
func (w *WhatsApp) maintain(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.kick:
			// Give the connection a moment to finish the initial handshake.
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		case <-ticker.C:
		}

		if !w.cli.IsConnected() || !w.cli.IsLoggedIn() {
			continue
		}
		if time.Since(w.pruned) >= 24*time.Hour {
			w.sent.prune(ctx)
			w.pruned = time.Now()
		}
		w.ensurePushName(ctx)
		if w.cfg.MarkOnline {
			if err := w.cli.SendPresence(ctx, types.PresenceAvailable); err != nil {
				w.log.Warn("send presence failed", "err", err)
			}
			// Being available is required to receive typing events, but it also
			// makes whatsmeow send active delivery receipts. Switch those back
			// to inactive so this linked device behaves like WhatsApp Web in the
			// background and doesn't suppress notifications on the phone.
			w.cli.SetForceActiveDeliveryReceipts(false)
		}
		w.resolveLID(ctx)
		if err := w.cli.SubscribePresence(ctx, w.target); err != nil {
			w.log.Warn("subscribe presence failed", "jid", w.target.String(), "err", err)
		} else {
			w.log.Debug("presence subscription refreshed", "jid", w.target.String())
		}
	}
}

// ensurePushName sets a push name if WhatsApp hasn't synced ours yet: whatsmeow
// refuses to send presence without one. We never send messages, so this name is
// not shown to anyone.
func (w *WhatsApp) ensurePushName(ctx context.Context) {
	if w.cli.Store.PushName != "" {
		return
	}
	w.cli.Store.PushName = w.cfg.PushName
	if err := w.cli.Store.Save(ctx); err != nil {
		w.log.Warn("could not save push name", "err", err)
		return
	}
	w.log.Info("push name was empty, defaulted", "push_name", w.cfg.PushName)
}

// resolveLID maps the phone-number JID to its LID alias, since newer WhatsApp
// versions deliver presence addressed to @lid.
func (w *WhatsApp) resolveLID(ctx context.Context) {
	if !w.lid.IsEmpty() || w.cli.Store.LIDs == nil || w.target.Server != types.DefaultUserServer {
		return
	}
	lid, err := w.cli.Store.LIDs.GetLIDForPN(ctx, w.target)
	if err != nil {
		w.log.Debug("lid lookup failed", "err", err)
		return
	}
	if !lid.IsEmpty() {
		w.lid = lid
		w.log.Info("resolved contact lid", "pn", w.target.String(), "lid", lid.String())
	}
}

func waLevel(l slog.Level) string {
	switch {
	case l <= slog.LevelDebug:
		return "DEBUG"
	case l <= slog.LevelInfo:
		return "INFO"
	case l <= slog.LevelWarn:
		return "WARN"
	default:
		return "ERROR"
	}
}
