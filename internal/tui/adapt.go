package tui

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/discovery"
	"github.com/zealtv/easyhog/internal/groundhog"
)

// FromView adapts the refresher's view to the view model. The refresher decides
// what is known and how fresh it is; this only chooses words and states, so the
// terminal can tell not-inspected, read-only, ticked, failing and stale apart.
// The one action it offers is Open folder, which shows the instance directory
// in the file manager and runs nothing inside it. Use FromViewWith to offer the
// actions that change an instance.
func FromView(v core.View) Data { return FromViewWith(v, nil) }

// TickLookup reports what this session's ticker knows about an instance
// (core.Ticker.Info satisfies it).
type TickLookup func(instanceKey string) (core.TickInfo, bool)

// FromViewWith is FromView with the ticker's side of the story, which makes
// Run, Stop, the pause and Retry appear where they can work: see controls. A
// nil lookup means nothing here acts, and no instance offers them.
func FromViewWith(v core.View, ticks TickLookup) Data {
	d := Data{Root: v.Root, Note: "No Groundhog instances were found beneath the root. No scheduled work has started."}
	if v.DiscoverErr != nil {
		d.Err = "rescan failed, showing the last list: " + v.DiscoverErr.Error()
	}
	var ticked []glanceRow
	for _, iv := range v.Instances {
		var info *core.TickInfo
		if ticks != nil {
			if ti, ok := ticks(iv.Instance.Path); ok {
				info = &ti
			}
		}
		n := instanceNode(iv, info, ticks != nil)
		d.Nodes = append(d.Nodes, n)
		if by := tickerOf(iv, info); by != tickedByNone {
			ticked = append(ticked, glanceRow{by: by, tickFailed: info != nil && info.Ticking && info.LastErr != nil, node: n})
		}
	}
	for _, dg := range v.Diagnostics {
		msg := ""
		if dg.Err != nil {
			msg = dg.Err.Error()
		}
		d.Nodes = append(d.Nodes, Node{Key: "diagnostic:" + dg.Path, Label: dg.Display, State: StateFailed,
			Detail: "could not be inspected", Preview: "Could not inspect " + dg.Path + "\n\n" + msg})
	}
	d.Pane = paneReport(v, ticks)
	d.Glance = glanceOf(ticked)
	return d
}

// controls says what an instance offers, and when it offers nothing, why. The
// rule is core.Offers; this only adds the terminal's own case, that nothing
// here acts, and the sentence's opening words.
func controls(iv core.InstanceView, tick *core.TickInfo, acting bool, item string) (offer []Action, why string) {
	const need = "Nothing is offered: "
	if !acting {
		return nil, need + "this view acts on nothing."
	}
	actions, reason := core.OffersOn(iv, tick, item)
	for _, a := range actions {
		offer = append(offer, Action(a))
	}
	if len(offer) == 0 {
		return nil, need + reason
	}
	return offer, ""
}

// healthWord is the one word the preview gives for how far to trust what is
// shown.
func healthWord(iv core.InstanceView) string {
	switch {
	case iv.Inspection.Err != nil:
		return "failing"
	case iv.Inspection.Last == nil:
		return "not inspected"
	}
	return iv.Health.String()
}

func actionNames(as []Action) string {
	var out []string
	for _, a := range as {
		out = append(out, commandName(a))
	}
	return strings.Join(out, ", ")
}

func clock(t time.Time) string { return t.Format("15:04:05") }

// short bounds untrusted text to one modest line.
func short(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func problemText(p *groundhog.Problem) string {
	if p == nil {
		return ""
	}
	return short(p.Code+": "+p.Message, 200)
}

// instanceLabel is the name the terminal gives an instance everywhere: the
// project folder's path relative to the search root, without the trailing
// ".groundhog" every instance directory carries. Display stays as discovery
// spells it (tests, sorting and refresh comparison use it). When the root is
// itself a project, Display reads ".groundhog", and when the root is the
// instance, ".": both show the project folder's own name.
func instanceLabel(in discovery.Instance) string {
	switch in.Display {
	case ".", ".groundhog":
		if name := filepath.Base(filepath.Dir(in.Path)); name != "." && name != string(filepath.Separator) {
			return name
		}
		return in.Display
	}
	return strings.TrimSuffix(in.Display, "/.groundhog")
}

func instanceNode(iv core.InstanceView, tick *core.TickInfo, acting bool) Node {
	in, files := iv.Inspection, iv.Files
	offer, why := controls(iv, tick, acting, "")
	n := Node{Key: iv.Instance.Path, Label: instanceLabel(iv.Instance), Actions: append(offer, ActionOpenFolder)}
	last := in.Last
	if last != nil {
		n.ActionNotes = retryNotes(offer, last.Status, "")
	}
	ticking := tick != nil && tick.Ticking
	// Hidden by class only, never by name: what the listing or the negotiated
	// script showed is what the reader may not want to see by default.
	n.Hidden = iv.Class == core.ClassMalformed || iv.Class == core.ClassLegacy || iv.Class == core.ClassUnsupported
	switch {
	case iv.Class == core.ClassMalformed:
		n.State, n.Detail = StateUnsupported, "malformed, not run"
	case iv.Class == core.ClassLegacy:
		n.State, n.Detail = StateUnsupported, "legacy script, read-only"
	case in.Err != nil && last != nil:
		n.State, n.Detail = StateFailed, "failing, last good "+clock(last.TakenAt)
	case in.Err != nil:
		n.State, n.Detail = StateFailed, "inspection failed"
	case iv.Class == core.ClassUnsupported:
		n.State, n.Detail = StateUnsupported, "cannot be controlled, read-only"
	case iv.Class == core.ClassNotInspected || last == nil:
		n.State, n.Detail = StateStopped, "not inspected"
	case last.Status.Paused:
		// The pause holds every ticker back, so it outranks who ticks it.
		n.State, n.Detail = StatePaused, "nothing fires or starts"
		if ticking {
			n.Detail += "; this session still ticks it"
		}
	case ticking:
		// The state word already says "running"; the detail adds only a problem.
		n.State = StateRunning
		if tick.LastErr != nil {
			n.Detail = "the last tick failed"
		}
	case core.TickedFromOutside(iv, tick): // a fresh tick that was not this session's
		n.State, n.Detail = StateExternal, "ticked from outside Easyhog"
	default:
		n.State, n.Detail = StateStopped, "not ticked"
	}
	if iv.Health == core.HealthStale && in.Err == nil {
		n.Detail = joinDetail(n.Detail, "stale")
	}
	if last != nil && n.State != StateFailed {
		n.Detail = joinDetail(n.Detail, "inspected "+clock(last.TakenAt))
	}
	// A failed or interrupted delivery makes the instance row read failed, as it
	// makes an item's row, whatever ticks it: who ticks it moves into the detail
	// and stays in the preview. An unsupported instance keeps its word.
	if last != nil && n.State != StateFailed && n.State != StateUnsupported {
		if k := failedCount(last.Status); k > 0 {
			rest := n.Detail
			if n.State == StateRunning {
				rest = joinDetail("running", rest)
			}
			n.State, n.Detail = StateFailed, joinDetail(plural(k, "delivery", "deliveries")+" failed", rest)
		}
	}

	beat := heartbeatOf(iv, n.State, tick)
	n.Heartbeat = beat.level

	var p strings.Builder
	fmt.Fprintf(&p, "%s\n\n", iv.Instance.Path)
	fmt.Fprintf(&p, "Kind: %s · Health: %s\n", iv.Class, healthWord(iv))
	if acting {
		if len(offer) > 0 {
			fmt.Fprintf(&p, "Palette offers: %s\n", actionNames(offer))
		} else {
			p.WriteString(why + "\n")
		}
		if note := retryExplanation(iv, offer, ""); note != "" {
			p.WriteString(note + "\n")
		}
		if line := tickLine(tick); line != "" {
			p.WriteString(line + "\n")
		}
	}
	switch {
	case len(iv.Instance.Malformed) > 0:
		p.WriteString("Its script was not run. Missing or unusable:\n- " + strings.Join(iv.Instance.Malformed, "\n- ") + "\n")
	case in.Support != nil && in.Support.Reason != "":
		p.WriteString("Support: " + short(in.Support.String(), 300) + "\n")
	}
	if in.Err != nil {
		fmt.Fprintf(&p, "Error at %s: %s\n", clock(in.ErrAt), short(in.Err.Error(), 300))
	}
	switch {
	case last != nil:
		statusPreview(&p, iv, tick, beat)
	case iv.Class != core.ClassMalformed && iv.Class != core.ClassLegacy:
		p.WriteString("Not inspected: nothing was run for this instance. Select it, or choose Refresh in the palette, to read its status.\n")
	}
	fmt.Fprintf(&p, "\nFrom directory listings (nothing run): %d schedule items, %d entries in out/", len(files.Items()), len(files.Out))
	if files.Runtime {
		p.WriteString(", runtime/ present")
	}
	if files.ScheduleTruncated || files.OutTruncated {
		p.WriteString(" (listing cut short)")
	}
	p.WriteString("\n")
	for _, pr := range files.Problems {
		fmt.Fprintf(&p, "Listing problem: %s\n", short(pr, 200))
	}
	n.Preview = p.String()

	n.Badge = instanceBadge(iv)
	n.Children = scheduleNodes(iv, tick, acting)
	return n
}

// tickLine is the preview's line on what this session's ticker does with the
// instance, "" when it never ticked it.
func tickLine(tick *core.TickInfo) string {
	if tick == nil {
		return ""
	}
	var b strings.Builder
	if tick.Ticking {
		fmt.Fprintf(&b, "This session ticks it every %s", ageText(tick.Interval))
	} else {
		b.WriteString("This session stopped ticking it")
	}
	if !tick.LastBegan.IsZero() {
		fmt.Fprintf(&b, "; its last tick began at %s", clock(tick.LastBegan))
		if tick.LastErr != nil {
			b.WriteString(" and failed: " + short(tick.LastErr.Error(), 200))
		}
	}
	b.WriteString(". Runs start in sessions of their own and outlive Easyhog.")
	return b.String()
}

// joinDetail joins two parts of a detail with ", ", leaving out an empty one so
// no stray comma is left.
func joinDetail(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + ", " + b
}

// beatReading is the heartbeat level of a row something drives, and what it was
// judged against.
type beatReading struct {
	level    core.Heartbeat
	interval time.Duration
	// own is true for this session's ticking, whose interval Easyhog knows; false
	// for anything outside Easyhog, where 60 s is assumed.
	own bool
}

// heartbeatOf reads the heartbeat for an instance row, by the row's final state.
// Only the rows something drives get one: external and running. A stopped,
// paused, failed or unsupported row gets none, and neither does a row with no
// recorded tick. This session's ticking is judged against the interval it keeps
// (tick.Interval), anything else against core.DefaultTickInterval.
func heartbeatOf(iv core.InstanceView, state State, tick *core.TickInfo) beatReading {
	if state != StateExternal && state != StateRunning {
		return beatReading{}
	}
	v, ok := core.PendingVerdict(iv, tick)
	if !ok {
		return beatReading{}
	}
	b := beatReading{interval: core.DefaultTickInterval}
	if state == StateRunning {
		b.own = true
		if tick != nil && tick.Interval > 0 {
			b.interval = tick.Interval
		}
	}
	b.level = core.HeartbeatOf(v, b.interval)
	return b
}

// preview is the one preview line that names the level and the interval it was
// judged against, with the two bounds that divide fresh, late and quiet.
func (b beatReading) preview() string {
	fresh := b.interval * 3 / 2
	late := max(core.TickFreshness, fresh)
	who := "assumed for something outside Easyhog, since Groundhog reports no interval"
	if b.own {
		who = "the interval this session ticks it at"
	}
	return fmt.Sprintf("Heartbeat: %s, judged against a tick expected every %s (%s): fresh up to %s, late up to %s.",
		b.level, ageText(b.interval), who, ageText(fresh), ageText(late))
}

// retryNotes is the hint beside Retry in the palette: how many runs it would
// start again (item is "" for the instance, else that item's path).
func retryNotes(offer []Action, st groundhog.Status, item string) map[Action]string {
	if !slices.Contains(offer, ActionRetry) {
		return nil
	}
	return map[Action]string{ActionRetry: fmt.Sprintf("%d to retry", len(core.RetryTargets(st, item)))}
}

// retryExplanation is the preview's account of Retry: what it does where it is
// offered, and why it is not where failed or interrupted runs wait behind the
// pause. Empty when neither applies.
func retryExplanation(iv core.InstanceView, offer []Action, item string) string {
	last := iv.Inspection.Last
	if last == nil {
		return ""
	}
	targets := core.RetryTargets(last.Status, item)
	switch {
	case slices.Contains(offer, ActionRetry):
		text := "Retry starts the failed and interrupted runs again, as new attempts."
		if hasInterrupted(last.Status, item) {
			text += " An interrupted run lost its wrapper, not necessarily its process: check it has ended before retrying."
		}
		return text
	case len(targets) > 0 && last.Status.Paused:
		return "Retry waits for Unpause instance, since a paused instance starts nothing."
	}
	return ""
}

func hasInterrupted(st groundhog.Status, item string) bool {
	for _, d := range st.Deliveries {
		if d.State == groundhog.DeliveryInterrupted && (item == "" || d.SchedulePath != nil && *d.SchedulePath == item) {
			return true
		}
	}
	return false
}

// failedState reports whether the tree calls a delivery in state s failed, in the
// instance badge and in an item's row alike: the failed and interrupted ones,
// which Retry acts on.
func failedState(s groundhog.DeliveryState) bool {
	return s == groundhog.DeliveryFailed || s == groundhog.DeliveryInterrupted
}

// failedCount is how many deliveries in st the tree calls failed (failedState).
func failedCount(st groundhog.Status) int {
	n := 0
	for s, k := range st.DeliveryCounts() {
		if failedState(s) {
			n += k
		}
	}
	return n
}

// deliveryOrder is the order the preview counts delivery states in: what needs
// attention first.
var deliveryOrder = []groundhog.DeliveryState{groundhog.DeliveryFailed, groundhog.DeliveryInterrupted,
	groundhog.DeliveryRunning, groundhog.DeliveryPending, groundhog.DeliveryAccepted, groundhog.DeliveryDelivered,
	groundhog.DeliveryUnrecorded}

// statusPreview writes what a script's status said: pause, ticks, schedule and
// delivery summaries. Everything here is "as of" the time of the read, and the
// caller has already said how far to trust it.
func statusPreview(p *strings.Builder, iv core.InstanceView, tick *core.TickInfo, beat beatReading) {
	last := iv.Inspection.Last
	st := last.Status
	p.WriteString("\n")
	if v, ok := core.PendingVerdict(iv, tick); ok {
		p.WriteString(verdictPreview(v, last.TakenAt.Location()) + "\n")
	}
	if beat.level != core.HeartbeatNone {
		p.WriteString(beat.preview() + "\n")
	}
	if st.Paused {
		p.WriteString("Pause: the instance is paused, so no tick fires an item or starts a run, and Retry waits\n")
	} else {
		p.WriteString("Pause: not paused\n")
	}

	paused, due, fired := 0, 0, 0
	for _, it := range st.Schedule {
		switch {
		case len(it.PausedBy) > 0:
			paused++
		case it.Fired:
			fired++
		case it.EligibleNow:
			due++
		}
	}
	fmt.Fprintf(p, "\nSchedule: %s", plural(len(st.Schedule), "item", "items"))
	if len(st.Schedule) > 0 {
		fmt.Fprintf(p, " (%d due now, %d already fired, %d paused)", due, fired, paused)
	}
	p.WriteString("\n")

	counts := st.DeliveryCounts()
	if len(st.Deliveries) == 0 {
		p.WriteString("Deliveries: none\n")
	} else {
		var parts []string
		for _, s := range deliveryOrder {
			if counts[s] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", counts[s], s))
			}
		}
		for s, k := range counts {
			if !s.Known() {
				parts = append(parts, fmt.Sprintf("%d %s", k, short(string(s), 20)))
			}
		}
		fmt.Fprintf(p, "Deliveries: %s\n", strings.Join(parts, ", "))
		listed := 0
		for _, d := range st.Deliveries {
			if d.State == groundhog.DeliveryAccepted || d.State == groundhog.DeliveryDelivered {
				continue
			}
			if listed == 5 {
				p.WriteString("  … more not listed\n")
				break
			}
			listed++
			fmt.Fprintf(p, "  %s\n", deliveryLine(d))
		}
	}
	for _, d := range st.Diagnostics {
		fmt.Fprintf(p, "Diagnostic: %s\n", problemText(&d))
	}
	fmt.Fprintf(p, "\nLast inspected: %s", clock(last.TakenAt))
	switch {
	case iv.Inspection.Err != nil:
		p.WriteString(" (the last status that could be read)")
	case iv.Health == core.HealthStale:
		p.WriteString(" (stale: not re-read although it should have been)")
	}
	p.WriteString("\n")
}

// deliveryLine is one delivery as one line: where it is, its state's glyph
// and word, its newest attempt, and its run's log.
func deliveryLine(d groundhog.Delivery) string {
	s := short(d.Path, 120) + ": " + deliveryGlyph(d.State) + " " + short(string(d.State), 20)
	if k := len(d.Attempts); k > 0 {
		a := d.Attempts[k-1]
		s += fmt.Sprintf("; attempt %d", k)
		if a.By != "" {
			s += " by " + short(a.By, 10)
		}
		if a.StartedAt != nil {
			s += ", started " + clock(a.StartedAt.Local())
		}
		if a.ExitStatus != nil {
			s += fmt.Sprintf(", exit status %d", *a.ExitStatus)
		}
	}
	if d.Log != nil {
		// A fired one-shot has no row of its own, so this names where its output is.
		s += "; log " + short(*d.Log, 120)
	}
	return s
}

// instanceBadge is the short tree summary. Without a status it says only what
// the listing showed, so an uninspected instance does not look inspected.
func instanceBadge(iv core.InstanceView) string {
	last := iv.Inspection.Last
	if last == nil || iv.Class == core.ClassLegacy || iv.Class == core.ClassMalformed {
		if k := len(iv.Files.Items()); k > 0 && iv.Class != core.ClassMalformed {
			return plural(k, "item", "items") + " listed"
		}
		return ""
	}
	st := last.Status
	var parts []string
	if iv.Inspection.Err == nil && iv.Health == core.HealthStale {
		parts = append(parts, "stale")
	}
	if len(st.Schedule) > 0 { // an empty count says nothing: a blank says it
		parts = append(parts, plural(len(st.Schedule), "item", "items"))
	}
	counts := st.DeliveryCounts()
	if k := counts[groundhog.DeliveryRunning]; k > 0 {
		parts = append(parts, fmt.Sprintf("%d running", k))
	}
	if k := counts[groundhog.DeliveryPending]; k > 0 {
		parts = append(parts, fmt.Sprintf("%d pending", k))
	}
	if k := failedCount(st); k > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", k))
	}
	return strings.Join(parts, " · ")
}

// verdictPreview is the verdict as a sentence with what it rests on, short
// enough to show whole in the 80x24 preview. The read's time is given in loc,
// the zone the preview's other times use (the script reports UTC).
func verdictPreview(v core.Verdict, loc *time.Location) string {
	tick := ""
	if v.HasTick {
		tick = fmt.Sprintf("%s before the %s read", ageText(v.Age), clock(v.ObservedAt.In(loc)))
	}
	switch v.Outlook {
	case core.OutlookNo:
		return "Pending work: no, the instance is paused."
	case core.OutlookYes:
		if tick == "" {
			return "Pending work: yes, this session ticks it (no tick recorded yet)."
		}
		return "Pending work: yes, this session ticks it; last tick " + tick + "."
	case core.OutlookProbably:
		return "Pending work: probably, something outside Easyhog ticked it " + tick + "."
	}
	switch {
	case tick == "":
		return "Pending work: nothing seen ticking it; no tick recorded."
	case v.Ours:
		return "Pending work: nothing seen ticking it since this session stopped; its last tick " + tick + "."
	}
	return "Pending work: nothing seen ticking it; last tick " + tick + "."
}

// ageText is a duration in the fewest readable units.
func ageText(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	case s < 86400:
		return fmt.Sprintf("%dh%02dm", s/3600, s%3600/60)
	}
	return fmt.Sprintf("%dd%02dh", s/86400, s%86400/3600)
}

// cadenceText says when an item recurs, from the script's cadence.
func cadenceText(c *groundhog.Cadence) string {
	if c == nil {
		return "no cadence reported"
	}
	var parts []string
	if c.Axis == "every" {
		parts = append(parts, "every")
		if c.Interval != nil {
			parts = []string{"every " + short(*c.Interval, 20)}
		}
	} else {
		parts = append(parts, short(c.Axis, 20))
	}
	for _, v := range []*string{c.Day, c.Date} {
		if v != nil {
			parts = append(parts, short(*v, 20))
		}
	}
	if c.Time != nil {
		parts = append(parts, "at "+short(*c.Time, 20))
	}
	text := strings.Join(parts, " ")
	if c.Defaulted {
		text += " (default slot)"
	}
	return text
}

var blockedWords = map[string]string{
	"subtree_paused":  "a folder or item on its path is paused",
	"not_due":         "not due yet",
	"already_fired":   "already fired for this slot",
	"instance_paused": "the instance is paused",
}

func blockedText(codes []string) string {
	var out []string
	for _, c := range codes {
		if w, ok := blockedWords[c]; ok {
			out = append(out, w)
		} else {
			out = append(out, short(c, 40))
		}
	}
	return strings.Join(out, "; ")
}

// dueWord is the one-word due state of an item.
func dueWord(it groundhog.ScheduleItem) string {
	switch {
	case len(it.PausedBy) > 0:
		return "paused"
	case it.Fired:
		return "fired"
	case it.EligibleNow && it.FiringAllowed:
		return "due now"
	case it.EligibleNow:
		return "due, held"
	}
	return "not due"
}

// itemBadge is the timing part of an item's row, and the time its countdown
// runs to. A paused item reads "paused" and a fired one "fired" (a recurring
// item that has fired reads its countdown to the next occurrence instead, when
// the script gave one). An item with a next_due that is not yet reached shows a
// countdown, worked out when the row is drawn, so the text returned is what it
// reads at and after that time: "due now", or "due, held" when something other
// than the clock stops it. With no next_due, whether from an older script or
// because there is none, it falls back to the cadence and the due word.
func itemBadge(it groundhog.ScheduleItem, word string) (string, *time.Time) {
	switch {
	case len(it.PausedBy) > 0:
		return "paused", nil
	case it.NextDue != nil:
		if word != "due now" && word != "due, held" { // not due yet, or fired for the slot just past
			word = "due now"
			for _, code := range it.BlockedBy {
				if code != "not_due" && code != "already_fired" {
					word = "due, held"
				}
			}
		}
		return word, it.NextDue
	case it.Fired:
		return "fired", nil
	}
	badge := short(cadenceText(it.Cadence), 30)
	if word != "not due" {
		badge += " · " + word
	}
	return badge, nil
}

// itemLabels names each item by its folder's own name, since the cadence and
// the date and time that the path above it spells out belong to the badge. The
// status carries no title (Groundhog 8a63131), and payload files are the
// refresher's business, so the base name is all there is. rels are the items'
// paths under schedule/. Items that share a base name (daily/report and
// weekly/mon/report) each add parent folders, the fewest that make that group
// distinct, so only the clashing rows grow longer. The preview and the row key
// keep the full path.
func itemLabels(rels []string) []string {
	parts := make([][]string, len(rels))
	byName := map[string][]int{}
	for i, r := range rels {
		parts[i] = strings.Split(r, "/")
		name := parts[i][len(parts[i])-1]
		byName[name] = append(byName[name], i)
	}
	out := make([]string, len(rels))
	for _, group := range byName {
		depth := 1
		for ; ; depth++ {
			seen := map[string]bool{}
			for _, i := range group {
				p := parts[i]
				seen[strings.Join(p[max(0, len(p)-depth):], "/")] = true
			}
			if len(seen) == len(group) || depth >= maxDepth(parts, group) {
				break
			}
		}
		for _, i := range group {
			p := parts[i]
			out[i] = strings.Join(p[max(0, len(p)-depth):], "/")
		}
	}
	return out
}

// maxDepth is the length of the longest path in the group.
func maxDepth(parts [][]string, group []int) int {
	m := 0
	for _, i := range group {
		m = max(m, len(parts[i]))
	}
	return m
}

// itemNames gives each item the path its row goes by: the path with `.paused`
// dropped from the item's own folder (core.ItemIdentity), so the row keeps its
// key, and its selection, when the folder is renamed, and its label does not
// say what the state word already says. Where `foo` and `foo.paused` both
// exist, each keeps its own folder name: the two rows need two keys and two
// labels. paths may be relative to the instance or to schedule/.
func itemNames(paths []string) []string {
	count := map[string]int{}
	for _, p := range paths {
		count[core.ItemIdentity(p)]++
	}
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = core.ItemIdentity(p)
		if count[out[i]] > 1 {
			out[i] = p
		}
	}
	return out
}

// scheduleNodes lists items. Keys come from the item's name (itemNames: its path
// under the instance, "schedule/daily/x", without a `.paused` on its own folder)
// in both the status and the listing form, so a row keeps its selection when the
// status arrives after the listing and when the item is paused or unpaused.
func scheduleNodes(iv core.InstanceView, tick *core.TickInfo, acting bool) []Node {
	var out []Node
	inst := iv.Instance.Path
	if last := iv.Inspection.Last; last != nil && iv.Class != core.ClassLegacy {
		st := last.Status
		paths := make([]string, len(st.Schedule))
		for i, it := range st.Schedule {
			paths[i] = it.Path
		}
		names := itemNames(paths)
		rels := make([]string, len(names))
		for i, nm := range names {
			rels[i] = strings.TrimPrefix(nm, "schedule/")
		}
		labels := itemLabels(rels)
		for i, it := range st.Schedule {
			// An item's deliveries carry the path it fired under, which has no
			// `.paused` on its own folder: they belong to the item by its name.
			// With a clash, a row owns only the deliveries stamped with its own path.
			name := names[i]
			_, clash := core.ItemClash(st, it.Path)
			ref := &PayloadRef{Instance: inst, Rel: it.Path}
			n := Node{Key: inst + "#" + name, Label: labels[i], State: StateScheduled, Payload: ref}
			own := false
			for _, by := range it.PausedBy {
				own = own || by == it.Path
			}
			var failedDelivery, running, waiting bool
			var related []groundhog.Delivery
			for _, d := range st.Deliveries {
				if d.SchedulePath != nil && (*d.SchedulePath == it.Path || !clash && core.ItemIdentity(*d.SchedulePath) == name) {
					related = append(related, d)
					failedDelivery = failedDelivery || failedState(d.State)
					running = running || d.State == groundhog.DeliveryRunning
					waiting = waiting || d.State == groundhog.DeliveryPending
				}
			}
			// The newest deliveries' logs go to the payload reader, newest first.
			slices.SortStableFunc(related, func(a, b groundhog.Delivery) int { return compareFired(b, a) })
			for _, d := range related {
				if d.Log != nil {
					ref.Logs = append(ref.Logs, *d.Log)
				}
			}
			word := dueWord(it)
			switch {
			case failedDelivery:
				n.State = StateFailed
			case running:
				n.State = StateRunning // its run is going now
			case len(it.PausedBy) > 0:
				n.State = StatePaused
			case waiting:
				// A delivery of this item waits for its run to start: the one thing
				// "pending" means. Otherwise the item is merely scheduled.
				n.State = StatePending
			}
			n.Detail = cadenceText(it.Cadence) + ", " + word
			n.Badge, n.Due = itemBadge(it, word)
			// Retry on an item is its own deliveries' (the same core.OffersOn rule as
			// the instance's, limited to this item).
			var retryOffer, toggleOffer []Action
			var toggleWhy string
			if acting {
				actions, _ := controls(iv, tick, acting, name)
				if slices.Contains(actions, ActionRetry) {
					retryOffer = []Action{ActionRetry}
				}
				var toggles []core.Action
				toggles, toggleWhy = core.ItemOffers(iv, it.Path)
				for _, a := range toggles {
					toggleOffer = append(toggleOffer, Action(a))
				}
				n.Actions = append(append(n.Actions, toggleOffer...), retryOffer...)
				n.ActionNotes = retryNotes(retryOffer, st, name)
				for _, a := range toggleOffer {
					if n.ActionNotes == nil {
						n.ActionNotes = map[Action]string{}
					}
					n.ActionNotes[a] = "→ " + short(path.Base(core.ItemTarget(it.Path, a == ActionPauseItem)), 30)
				}
			}

			var p strings.Builder
			fmt.Fprintf(&p, "Item %s\nItem id: %s\n\n", it.Path, short(it.ItemID, 100))
			fmt.Fprintf(&p, "Cadence: %s\n", cadenceText(it.Cadence))
			fmt.Fprintf(&p, "Due: %s", word)
			if it.FiredMarker != nil {
				fmt.Fprintf(&p, " (marker %s)", short(*it.FiredMarker, 100))
			}
			p.WriteString("\n")
			if it.NextDue != nil {
				fmt.Fprintf(&p, "Next due: %s\n", it.NextDue.Local().Format("Mon 2 Jan 2006 15:04:05 MST"))
				p.WriteString("The row counts down to this. Groundhog fires the item on the first tick at or after it, so with a 60 s tick that can be up to a minute later.\n")
			}
			switch {
			case own:
				p.WriteString("Pause: this item's own name is paused\n")
			case len(it.PausedBy) > 0:
				p.WriteString("Pause: inherited from " + short(strings.Join(it.PausedBy, ", "), 200) + "\n")
			case st.Paused:
				p.WriteString("Pause: inherited from the instance, which is paused\n")
			default:
				p.WriteString("Pause: not paused\n")
			}
			// Right under the pause line, where an 80x24 preview still shows it.
			switch {
			case len(toggleOffer) > 0:
				fmt.Fprintf(&p, "Palette offers: %s (renames its folder to %s)\n",
					actionNames(toggleOffer), short(path.Base(core.ItemTarget(it.Path, toggleOffer[0] == ActionPauseItem)), 60))
			case acting && toggleWhy != "":
				fmt.Fprintf(&p, "Pausing is not offered: %s\n", short(toggleWhy, 300))
			}
			if len(it.BlockedBy) > 0 {
				fmt.Fprintf(&p, "Firing held: %s\n", blockedText(it.BlockedBy))
			}
			for _, d := range related {
				fmt.Fprintf(&p, "Delivery: %s\n", deliveryLine(d))
			}
			if len(retryOffer) > 0 {
				fmt.Fprintf(&p, "Palette offers: %s (this item's deliveries only)\n", actionNames(retryOffer))
			}
			if note := retryExplanation(iv, retryOffer, name); note != "" {
				p.WriteString(note + "\n")
			}
			fmt.Fprintf(&p, "\nAs of %s", clock(last.TakenAt))
			if w := healthWord(iv); w != core.HealthOK.String() {
				fmt.Fprintf(&p, " (%s)", w)
			}
			p.WriteString("\n")
			n.Preview = p.String()
			out = append(out, n)
		}
		return out
	}
	listed := iv.Files.Items()
	rels := make([]string, len(listed))
	for i, it := range listed {
		rels[i] = it.Path
	}
	names := itemNames(rels)
	labels := itemLabels(names)
	for i, it := range listed {
		n := Node{Key: inst + "#schedule/" + names[i], Label: labels[i], State: StateScheduled, Detail: "not inspected",
			Payload: &PayloadRef{Instance: inst, Rel: "schedule/" + it.Path}}
		if it.Paused {
			n.State, n.Detail = StatePaused, "paused by name, not inspected"
		}
		n.Badge = "listed only"
		n.Preview = "Item schedule/" + it.Path + "\nSeen in a directory listing; no script was run, so cadence and due state are not known."
		out = append(out, n)
	}
	return out
}

// compareFired orders deliveries by when they fired (an unrecorded one first),
// then by path.
func compareFired(a, b groundhog.Delivery) int {
	switch {
	case a.FiredAt == nil && b.FiredAt != nil:
		return -1
	case a.FiredAt != nil && b.FiredAt == nil:
		return 1
	case a.FiredAt != nil && !a.FiredAt.Equal(*b.FiredAt):
		return a.FiredAt.Compare(*b.FiredAt)
	}
	return strings.Compare(a.Path, b.Path)
}
