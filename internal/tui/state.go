package tui

import (
	"errors"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/zealtv/easyhog/internal/core"
)

// State says what an instance or schedule item is doing, in words and a glyph
// that stay distinguishable with colour off.
type State int

const (
	StateStopped State = iota
	StateRunning
	StatePaused
	StateExternal
	StateUnsupported
	StatePending
	StateFailed
	// StateScheduled is a schedule item with nothing waiting and nothing wrong: it
	// is on the schedule. StatePending is left to what is waiting for a hand-over
	// (a delivery), so the two words never mean each other.
	StateScheduled
	// StateInconsistent is an instance whose latest status read the script itself
	// called incoherent: shown as seen, not acted on. It once read "pending", a word
	// now left to a delivery waiting.
	StateInconsistent
)

func (s State) label() string {
	switch s {
	case StateRunning:
		return "running"
	case StatePaused:
		return "paused"
	case StateExternal:
		return "external"
	case StateUnsupported:
		return "unsupported"
	case StatePending:
		return "pending"
	case StateFailed:
		return "failed"
	case StateScheduled:
		return "scheduled"
	case StateInconsistent:
		return "inconsistent"
	}
	return "stopped"
}

func (s State) glyph() string {
	switch s {
	case StateRunning:
		return "●"
	case StatePaused:
		return "◐"
	case StateExternal:
		return "◇"
	case StateUnsupported:
		return "⊘"
	case StatePending:
		return "◌"
	case StateFailed:
		return "✗"
	case StateScheduled:
		// A circle with one quadrant lit, like a clock face at three: it says "at a
		// time", and differs from pending's dotted ◌ and from paused's half-filled ◐.
		return "◷"
	case StateInconsistent:
		// Two waves: the read wavers. Unlike any other state's glyph, and the badge
		// already says "inconsistent" in words.
		return "≈"
	}
	return "○"
}

// heartbeatGlyph is the mark after a driven row's state word, one cell wide and
// unlike every state glyph, so the level reads with colour off: a bullet for
// fresh, a ring for late, a small dot for quiet, falling in size as the tick
// ages. All three sit in the WGL4 set (U+2022, U+25E6, U+2219), which common
// terminal fonts carry, and none is the middle dot that separates badge parts.
// None draws nothing.
func heartbeatGlyph(h core.Heartbeat) string {
	switch h {
	case core.HeartbeatFresh:
		return "•"
	case core.HeartbeatLate:
		return "◦"
	case core.HeartbeatQuiet:
		return "∙"
	}
	return ""
}

// Action names something the palette can ask for. Quit is the only action the
// shell performs itself; the others go to an ActionHandler.
type Action string

const (
	ActionRun  Action = "run"
	ActionStop Action = "stop"
	// The three settling actions are Retry failed, Recover as done and Recover and
	// retry (core's ActionRetryFailed, ActionRecoverAccept, ActionRecoverRetry).
	ActionRetryFailed   Action = "retry-failed"
	ActionRecoverAccept Action = "recover-accept"
	ActionRecoverRetry  Action = "recover-retry"
	// Pause item and Unpause item rename one schedule item's folder (core.RenameItem).
	// They are an item's own offers, like the settling actions, and run on the
	// instance's one slot.
	ActionPauseItem   Action = "pause-item"
	ActionUnpauseItem Action = "unpause-item"
	ActionRefresh     Action = "refresh"
	ActionOpenFolder  Action = "open-folder"
	ActionQuit        Action = "quit"
	// ActionToggleHidden shows or hides the instances Node.Hidden marks. It is a
	// view preference the shell keeps itself: it starts no work and reaches no handler.
	ActionToggleHidden Action = "toggle-hidden"
)

// settles reports whether a is one of the three settling actions.
func (a Action) settles() bool {
	return a == ActionRetryFailed || a == ActionRecoverAccept || a == ActionRecoverRetry
}

// togglesItem reports whether a is Pause item or Unpause item.
func (a Action) togglesItem() bool { return a == ActionPauseItem || a == ActionUnpauseItem }

// onNode reports whether a is an offer of the selected node itself (an item's
// settling actions, Pause item, Unpause item) rather than of its instance.
func (a Action) onNode() bool { return a.settles() || a.togglesItem() }

// acts reports whether a asks the supervisor to do something to the instance's
// runner, deliveries or schedule: Run, Stop, a settling action, Pause item or
// Unpause item. They run one at a time per instance, off the update loop, and
// show a transitional state.
func (a Action) acts() bool { return a == ActionRun || a == ActionStop || a.onNode() }

// Node is one row of the tree: an instance (top level) or one of its schedule
// items.
type Node struct {
	// Key identifies the node across snapshots (for example a canonical
	// instance path, plus the item id). Selection and expansion follow it,
	// never a row index. Keys must be unique within a Data.
	Key   string
	Label string
	State State
	// Detail is one quiet line under the label in the preview.
	Detail string
	// Preview is the text shown for this node. It is inert text: the shell
	// strips control characters and never runs anything it contains.
	Preview string
	// Actions lists the actions currently valid for an instance. The palette
	// offers only these (plus Refresh and Quit). Items take their instance's
	// Run and Stop, which act on the instance; the settling actions an item
	// lists are its own (the instance's, limited to that item's deliveries), so a
	// settling action is read from the selected node and the rest from its instance.
	Actions []Action
	// Badge is a short quiet summary drawn beside the label in the tree when the
	// pane is wide enough ("2 items · 1 failed"). Plain text, no layout.
	Badge string
	// Due, when set on an item, is when it next becomes due (the script's
	// next_due). While it lies ahead of the shell's clock the row's badge is the
	// countdown to it; at and after it, and whenever Due is nil, the row shows
	// Badge. The countdown is worked out at render time, never stored.
	Due *time.Time
	// Heartbeat is how recent the tick behind an instance row looks: set only on
	// the rows something drives (external, and running here) that record a tick,
	// and drawn as a small mark after the state word. Zero draws nothing.
	Heartbeat core.Heartbeat
	// RunNote is a light, non-blocking hint shown beside Run in the palette
	// ("will settle an interrupted journal write"). It asks nothing and changes
	// no key.
	RunNote string
	// RunConfirm, when set, is the question the palette puts to the reader before
	// it runs Run on this instance: something outside Easyhog ticks it, and
	// enrolling makes those ticks fail until Stop releases it. Empty for every
	// instance Run can take without asking.
	RunConfirm string
	// ActionNotes are light hints beside the settling actions in the palette
	// ("2 failed", "1 uncertain"), for the node the palette is bound to.
	ActionNotes map[Action]string
	// Hidden marks a top-level instance the tree leaves out unless the reader
	// asked to see hidden instances (the palette's toggle). Hiding is only a view
	// choice: the instance is still discovered, still in Data, and hiding it
	// starts or stops nothing. Ignored below the top level.
	Hidden bool
	// Payload, when set, names the files the preview reads as text (an item's
	// directory). Nothing in them is ever run.
	Payload  *PayloadRef
	Children []Node
}

// PayloadRef names a schedule item's directory: Rel is slash-separated and
// relative to Instance, the instance directory.
type PayloadRef struct {
	Instance string
	Rel      string
}

// PayloadMsg delivers the text read for one node. It applies only while that
// node is still selected.
type PayloadMsg struct {
	Key  string
	Text string
}

// Data is one snapshot for the shell to show. It carries no I/O handles;
// whatever builds it does so outside the render loop and delivers it as a
// SnapshotMsg.
type Data struct {
	Root  string
	Nodes []Node
	// Err, when set, reports that the latest load failed. With no nodes the
	// body shows an error state; with nodes the last good tree stays visible.
	Err string
	// Note explains an empty tree when there is no error.
	Note string
}

// SnapshotMsg replaces the shown data. Selection, expansion and scroll follow
// node keys across it.
type SnapshotMsg struct{ Data Data }

// ActionRequest is what the palette asks an ActionHandler to do.
type ActionRequest struct {
	Action Action
	// InstanceKey is the key of the selected node's instance, empty when
	// nothing is selected. NodeKey is the selected node itself.
	InstanceKey string
	NodeKey     string
	// ItemPath is the schedule item's path ("schedule/daily/x") when a settling
	// action was chosen on an item, so it settles that item's deliveries only, and
	// for Pause item and Unpause item, the folder to rename (as the status lists it,
	// `.paused` suffix and all); empty on an instance (all of its deliveries) and
	// for every other action. A settling action's path has no `.paused` suffix on
	// the item's own component, as the delivery records carry it.
	ItemPath string
	// Label is the bound instance's relative path as the tree shows it, cleaned
	// for display: what a notice about the action names.
	Label string
}

// ResultMsg reports the outcome of an action in the status line. InstanceKey,
// when set, says which instance's action ended, which lets the shell offer
// Run and Stop for it again.
type ResultMsg struct {
	Action      Action
	InstanceKey string
	Message     string
	Err         error
}

// ActionError is an action's failure with the words to show for it. Err keeps
// the typed cause (a *core.RefusedError, say) for callers and tests.
type ActionError struct {
	Text string
	Err  error
}

func (e *ActionError) Error() string { return e.Text }
func (e *ActionError) Unwrap() error { return e.Err }

// ErrNotAvailable is the result of actions nothing implements yet.
var ErrNotAvailable = errors.New("not available yet")

// ActionHandler starts an action and returns the command that performs it.
// The shell calls it only after the user ran the matching palette entry.
type ActionHandler func(ActionRequest) tea.Cmd

// clean makes untrusted text safe to draw: tabs become spaces and every other
// control character, escape included, becomes a visible middle dot.
func clean(s string) string {
	var b strings.Builder
	for _, r := range strings.ReplaceAll(s, "\r\n", "\n") {
		switch {
		case r == '\n':
			b.WriteRune(r)
		case r == '\t':
			b.WriteString("    ")
		case unicode.IsControl(r) || r == ' ' || r == ' ':
			b.WriteRune('·')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// cleanLine is clean for a single-line label.
func cleanLine(s string) string {
	return strings.ReplaceAll(clean(s), "\n", " ")
}
