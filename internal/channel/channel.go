package channel

import "context"

// KeyboardButton is one button in an inline keyboard row.
type KeyboardButton struct {
	Text string // label shown to the user
	Data string // opaque callback data sent back when pressed
}

// InboundEvent represents a message, command, or inline keyboard button press
// arriving from a chat platform.
type InboundEvent struct {
	UserKey        string // platform-scoped user identifier, e.g. "tg:123456"
	ConversationID string // chat/channel identifier passed back to Send* methods
	Text           string // message text (for voice: the transcription)
	IsCommand      bool
	Command        string // without the leading slash, e.g. "reset"
	CommandArgs    string // text after the command

	// IsVoice is true when the text was transcribed from a voice message.
	IsVoice bool

	// IsCallback is true when the event originates from an inline keyboard button press.
	IsCallback   bool
	CallbackID   string // opaque ID used with AnswerCallback
	CallbackData string // data string attached to the pressed button
}

// MessageHandle is an opaque reference to a sent message that can be edited.
// Seal must be called when no further edits will be made.
type MessageHandle interface {
	Seal()
}

// Channel abstracts the chat platform (Telegram, Slack, …).
type Channel interface {
	// Events returns a channel that emits inbound events until ctx is cancelled.
	Events(ctx context.Context) <-chan InboundEvent

	// SendText sends a new message and returns an editable handle.
	SendText(ctx context.Context, conversationID string, text string) (MessageHandle, error)

	// UpdateText replaces the content of a previously sent message.
	// A no-op if the handle has been Seal()ed.
	UpdateText(ctx context.Context, handle MessageHandle, text string) error

	// SendTyping sends a transient "typing…" indicator.
	SendTyping(ctx context.Context, conversationID string) error

	// SendDocument uploads a local file to the conversation.
	// caption may be empty.
	SendDocument(ctx context.Context, conversationID string, filePath string, caption string) error

	// SendKeyboard sends a message with an inline keyboard.
	// buttons is a 2-D slice: outer index = row, inner index = button in that row.
	SendKeyboard(ctx context.Context, conversationID string, text string, buttons [][]KeyboardButton) (MessageHandle, error)

	// AnswerCallback acknowledges an inline keyboard button press so Telegram
	// removes the loading spinner. notification is shown briefly (may be empty).
	AnswerCallback(ctx context.Context, callbackID string, notification string) error
}
