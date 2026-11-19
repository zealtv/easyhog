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
// terminal can tell not-inspected, read-only, externally owned, failing and
// stale apart. The one action it offers is Open folder, which shows the
// instance directory in the file manager and runs nothing inside it. Use
// FromViewWith to offer Run and Stop.
func FromView(v core.View) Data { return FromViewWith(v, nil) }

// RunnerLookup reports what the supervisor knows about an instance it enrolled
// (core.Supervisor.Runner satisfies it).
type RunnerLookup func(instanceKey string) (core.RunnerInfo, bool)

// FromViewWith is FromView with the supervisor's side of the story, which makes
// Run and Stop appear where they can work: see controls. A nil lookup means
// nothing is supervised and no instance offers either.
func FromViewWith(v core.View, runners RunnerLookup) Data {
	d := Data{Root: v.Root, Note: "No Groundhog instances were found beneath the root. No scheduled work has started."}
	if v.DiscoverErr != nil {
		d.Err = "rescan failed, showing the last list: " + v.DiscoverErr.Error()
	}
	for _, iv := range v.Instances {
		var info *core.RunnerInfo
		if runners != nil {
			if ri, ok := runners(iv.Instance.Path); ok {
				info = &ri
			}
		}
		d.Nodes = append(d.Nodes, instanceNode(iv, info, runners != nil))
	}
	for _, dg := range v.Diagnostics {
		msg := ""
		if dg.Err != nil {
			msg = dg.Err.Error()
		}
		d.Nodes = append(d.Nodes, Node{Key: "diagnostic:" + dg.Path, Label: dg.Display, State: StateFailed,
			Detail: "could not be inspected", Preview: "Could not inspect " + dg.Path + "\n\n" + msg})
	}
	return d
}

// controls says which of Run and Stop an instance offers, and when it offers
// neither, why. The rule is core.Offers, which the supervisor also applies when
// an action runs; this only adds the terminal's own case, that nothing is
// supervised, and the sentence's opening words.
func controls(iv core.InstanceView, info *core.RunnerInfo, supervised bool) (offer []Action, why string) {
	const need = "Run and Stop are not offered: "
	if !supervised {
		return nil, need + "nothing here is supervised."
	}
	actions, reason := core.Offers(iv, info)
	for _, a := range actions {
		offer = append(offer, Action(a))
	}
	if len(offer) == 0 {
		return nil, need + reason
	}
	return offer, ""
}

// healthWord is the one word the preview gives for how far to trust what is
// shown. Whether the newest read failed, or nothing was read, comes from the
// instance (Inspection.Err, Inspection.Last); Health says only how far the
// status that is shown can be trusted.
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

func instanceNode(iv core.InstanceView, info *core.RunnerInfo, supervised bool) Node {
	in, files := iv.Inspection, iv.Files
	offer, why := controls(iv, info, supervised)
	n := Node{Key: iv.Instance.Path, Label: instanceLabel(iv.Instance), Actions: append(offer, ActionOpenFolder)}
	if last := in.Last; last != nil {
		n.ActionNotes = settleNotes(offer, last.Status, "")
	}
	last := in.Last
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
	case iv.Health == core.HealthInconsistent:
		n.State, n.Detail = StateInconsistent, "inconsistent read, as of "+clock(last.TakenAt)
	case iv.Class == core.ClassExternal:
		n.State, n.Detail = StateExternal, ownedByDetail(iv)
	case tickedFromOutside(iv):
		n.State, n.Detail = StateExternal, "ticked from outside Easyhog"
	case iv.Class == core.ClassUnsupported:
		n.State, n.Detail = StateUnsupported, "cannot be controlled, read-only"
	case iv.Class == core.ClassNotInspected && info != nil && info.Adopted:
		// Taken over from a session that ended; nothing says so until the reader
		// selects it, yet Quit counts its runner.
		n.State, n.Detail = StateStopped, "adopted, not inspected"
	case iv.Class == core.ClassNotInspected:
		n.State, n.Detail = StateStopped, "not inspected"
	case last != nil && last.Status.Runner.State == "running" && iv.Class == core.ClassOwnedBySession && last.Status.Paused:
		// The contract keeps the runner alive while paused, so status reads
		// paused: true with runner.state: running. Only this session's runner is
		// shown as paused; an external or unowned instance keeps the generic
		// word for its class, and its badge and preview still say "paused".
		n.State, n.Detail = StatePaused, "paused, runner idle"
	case last != nil && last.Status.Runner.State == "running" && iv.Class == core.ClassOwnedBySession:
		n.State, n.Detail = StateRunning, "running"
	case last != nil && last.Status.Runner.State == "stopping" && iv.Class == core.ClassOwnedBySession:
		n.State, n.Detail = StateStopped, "runner stopping"
	case iv.Class == core.ClassOwnedBySession:
		// This session holds the token and Quit will stop and release it, but no
		// runner ticks it: stopped, failed or unknown.
		n.State, n.Detail = StateStopped, "enrolled, no runner"
	default:
		n.State, n.Detail = StateStopped, "not enrolled"
	}
	if info != nil && info.Phase == core.PhaseExited && iv.Class == core.ClassOwnedBySession {
		n.State, n.Detail = StateFailed, "runner ended on its own, not restarted"
	}
	if iv.Health == core.HealthStale && in.Err == nil {
		n.Detail += " (stale)"
	}
	if last != nil && n.State != StateFailed && iv.Health != core.HealthInconsistent {
		n.Detail += ", inspected " + clock(last.TakenAt)
	}
	// A failed or uncertain delivery makes the instance row read failed, as it
	// makes an item's row, whatever the runner is doing: the runner's own state
	// stays in the badge's verdict, the detail and the preview. An unsupported
	// instance keeps its word, and an inconsistent read is not acted on.
	if last != nil && n.State != StateFailed && n.State != StateUnsupported && iv.Health != core.HealthInconsistent {
		if k := failedCount(last.Status); k > 0 {
			n.State, n.Detail = StateFailed, plural(k, "delivery", "deliveries")+" failed, "+n.Detail
		}
	}

	beat := heartbeatOf(iv, n.State, info)
	n.Heartbeat = beat.level

	var p strings.Builder
	fmt.Fprintf(&p, "%s\n\n", iv.Instance.Path)
	fmt.Fprintf(&p, "Kind: %s · Health: %s\n", iv.Class, healthWord(iv))
	if supervised {
		if len(offer) > 0 {
			fmt.Fprintf(&p, "Palette offers: %s\n", actionNames(offer))
		} else {
			p.WriteString(why + "\n")
		}
		if note := settleExplanation(iv, offer); note != "" {
			p.WriteString(note + "\n")
		}
		if info != nil && info.Notice != "" {
			fmt.Fprintf(&p, "This session's runner: %s\n", short(info.Notice, 300))
		}
		if info != nil && info.Adopted {
			fmt.Fprintf(&p, "Adopted from a session that ended at %s. Its runner, if still running, is not this session's: Stop, Run and Quit work through the token.\n", clock(info.AdoptedFrom))
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
		statusPreview(&p, iv, beat)
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
	if v, ok := core.PendingVerdict(iv); ok && tickedFromOutside(iv) && slices.Contains(offer, ActionRun) {
		n.RunConfirm = fmt.Sprintf("Something outside Easyhog ticked %s %s before the last read. If Easyhog runs it, those ticks will fail until you Stop it here. Run anyway?",
			short(n.Label, 40), ageText(v.Age))
	}
	if slices.Contains(offer, ActionRun) && settlesJournal(iv) {
		n.RunNote = "will settle an interrupted journal write" // replaces the tick note: the read is not coherent
	}
	n.Children = scheduleNodes(iv, info, supervised)
	return n
}

// ownedByDetail is the detail line of an instance another supervisor owns: the
// owner's own label when the status gives one, cut short and made safe to draw.
func ownedByDetail(iv core.InstanceView) string {
	if last := iv.Inspection.Last; last != nil && last.Status.Owner != nil {
		if label := cleanLine(short(last.Status.Owner.Label, 40)); label != "" {
			return "owned by " + label
		}
	}
	return "owned by another supervisor"
}

// tickedFromOutside reports the second kind of external instance: nobody owns it,
// and the one verdict (core.PendingVerdict) says it probably keeps being ticked,
// by a standalone `tick` that Easyhog did not make. It reads the verdict and adds
// no freshness rule of its own. A recent tick whose source is "runner" is not this
// kind: a runner that has just been stopped and released here, or by another
// supervisor, leaves one behind for the same three minutes, and the reader who
// pressed Stop would otherwise see the instance as someone else's.
func tickedFromOutside(iv core.InstanceView) bool { return core.TickedFromOutside(iv) }

// beatReading is the heartbeat level of a row something drives, and what it was
// judged against.
type beatReading struct {
	level    core.Heartbeat
	interval time.Duration
	// own is true for this session's runner, whose interval Easyhog knows (the
	// default when the lookup gave none); false for anything outside Easyhog,
	// where 60 s is assumed.
	own bool
}

// heartbeatOf reads the heartbeat for an instance row, by the row's final state.
// Only the rows something drives get one: external (both kinds) and running. A
// stopped, paused, failed, unsupported or inconsistent row gets none, and neither
// does a row with no recorded tick. This session's runner is judged against the
// interval it keeps (info.Interval), anything else against core.DefaultTickInterval.
func heartbeatOf(iv core.InstanceView, state State, info *core.RunnerInfo) beatReading {
	if state != StateExternal && state != StateRunning {
		return beatReading{}
	}
	v, ok := core.PendingVerdict(iv)
	if !ok {
		return beatReading{}
	}
	b := beatReading{interval: core.DefaultTickInterval}
	if state == StateRunning {
		b.own = true
		if info != nil && info.Interval > 0 {
			b.interval = info.Interval
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
		who = "this session's runner's interval"
	}
	return fmt.Sprintf("Heartbeat: %s, judged against a tick expected every %s (%s): fresh up to %s, late up to %s.",
		b.level, ageText(b.interval), who, ageText(fresh), ageText(late))
}

// settleNotes are the hints beside the settling actions in the palette: how many
// deliveries each would act on (item is "" for the instance, else that item's path).
func settleNotes(offer []Action, st groundhog.Status, item string) map[Action]string {
	var out map[Action]string
	for _, a := range offer {
		if !a.settles() {
			continue
		}
		n := len(core.SettleTargets(st, core.Action(a), item))
		word := "failed"
		if a != ActionRetryFailed {
			word = "uncertain"
		}
		if out == nil {
			out = map[Action]string{}
		}
		out[a] = fmt.Sprintf("%d %s", n, word)
	}
	return out
}

// settleExplanation is the preview's account of the settling actions: what each
// does when it is offered, and for an unowned instance that holds deliveries
// waiting on a decision (a Quit that gave up waiting leaves such an instance
// unowned), that Run comes first. Empty when neither applies.
func settleExplanation(iv core.InstanceView, offer []Action) string {
	last := iv.Inspection.Last
	if last == nil {
		return ""
	}
	counts := last.Status.DeliveryCounts()
	failed, uncertain := counts[groundhog.DeliveryFailed], counts[groundhog.DeliveryUncertain]
	var parts []string
	if slices.Contains(offer, ActionRetryFailed) {
		parts = append(parts, "Retry failed makes the failed deliveries pending, for a new attempt.")
	}
	if slices.Contains(offer, ActionRecoverAccept) {
		parts = append(parts, "Recover as done records the uncertain ones as accepted on your say-so; Recover and retry makes them pending. Nothing is invoked, and Easyhog cannot know what the consumer did, so check first.")
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ") + " Each stops this session's runner for a moment and starts it again if it was running (the pause stays as it is)."
	}
	if iv.Class != core.ClassUnowned || !slices.Contains(offer, ActionRun) || failed+uncertain == 0 {
		return ""
	}
	// Nothing to settle with yet: the instance is not this session's.
	var what []string
	if uncertain > 0 {
		what = append(what, plural(uncertain, "uncertain delivery", "uncertain deliveries")+" (every handoff waits until they are recovered)")
	}
	if failed > 0 {
		what = append(what, plural(failed, "failed delivery", "failed deliveries"))
	}
	text := "Holds " + strings.Join(what, " and ") + ". Settling needs this session's token: Run enrolls it for this session, and then "
	switch {
	case uncertain > 0 && failed > 0:
		text += "Recover as done, Recover and retry and Retry failed are offered."
	case uncertain > 0:
		text += "Recover as done and Recover and retry are offered."
	default:
		text += "Retry failed is offered."
	}
	return text
}

// settlesJournal reports whether Run would settle an interrupted journal write
// on this instance (core.SettlesOnRun).
func settlesJournal(iv core.InstanceView) bool {
	return iv.Class == core.ClassUnowned && iv.Health == core.HealthInconsistent &&
		iv.Inspection.Last != nil && core.SettlesOnRun(iv.Inspection.Last.Status)
}

// failedState reports whether the tree calls a delivery in state s failed, in the
// instance badge and in an item's row alike: the failed and uncertain ones, which
// Retry failed and the Recovers act on. A `missing` delivery is settled for good
// and offers nothing, and an --active read only counts it, so no item could show
// it; it stays out of both, and the preview still counts it.
func failedState(s groundhog.DeliveryState) bool {
	return s == groundhog.DeliveryFailed || s == groundhog.DeliveryUncertain
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

// settled reports an --active read that left settled deliveries out of the
// list: accepted and missing ones are counted in Status.Settled instead.
func settled(st groundhog.Status) bool {
	return st.Settled != nil && st.Settled.Accepted+st.Settled.Missing > 0
}

// statusPreview writes what a script's status said: ownership, pause, runner,
// consumer, schedule and delivery summaries. Everything here is "as of" the
// time of the read, and the caller has already said how far to trust it.
func statusPreview(p *strings.Builder, iv core.InstanceView, beat beatReading) {
	in, last := iv.Inspection, iv.Inspection.Last
	st := last.Status
	if !st.Consistent && settlesJournal(iv) {
		p.WriteString("The script reported an unfinished delivery journal write. Nothing else is wrong with the status; Run will settle it (enrolling takes the lock that does) and then read the status again.\n")
	} else if !st.Consistent {
		fmt.Fprintf(p, "The script reported an inconsistent status after %d reads; it is shown as seen and should not be acted on.\n", in.Attempts)
	}
	p.WriteString("\n")
	if v, ok := core.PendingVerdict(iv); ok {
		p.WriteString(verdictPreview(v) + "\n")
	}
	if beat.level != core.HeartbeatNone {
		p.WriteString(beat.preview() + "\n")
	}
	switch {
	case st.Owner == nil:
		p.WriteString("Ownership: none recorded\n")
	case iv.Class == core.ClassOwnedBySession:
		fmt.Fprintf(p, "Ownership: this session (%s)\n", short(st.Owner.Label, 80))
	default:
		fmt.Fprintf(p, "Ownership: another supervisor, %s (%s). Easyhog only inspects it.\n", short(st.Owner.Label, 80), short(st.Owner.ID, 80))
	}
	if st.Paused {
		p.WriteString("Pause: the instance is paused, so no item fires\n")
	} else {
		p.WriteString("Pause: not paused\n")
	}
	fmt.Fprintf(p, "Runner: %s\n", st.Runner.State)
	if e := problemText(st.Runner.LastError); e != "" {
		fmt.Fprintf(p, "Runner's last error: %s\n", e)
	}
	switch c := st.Consumer; {
	case !c.Configured:
		p.WriteString("Consumer: not configured\n")
	case !c.Valid:
		p.WriteString("Consumer: configured, but not valid\n")
	default:
		p.WriteString("Consumer: configured\n")
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

	counts := st.DeliveryCounts() // listed deliveries plus the settled ones an --active read only counts
	total := 0
	for _, n := range counts {
		total += n
	}
	if total == 0 {
		p.WriteString("Deliveries: none\n")
	} else {
		var parts []string
		for _, s := range []groundhog.DeliveryState{groundhog.DeliveryPending, groundhog.DeliveryInFlight, groundhog.DeliveryFailed,
			groundhog.DeliveryUncertain, groundhog.DeliveryMissing, groundhog.DeliveryAccepted} {
			if counts[s] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", counts[s], strings.ReplaceAll(string(s), "_", " ")))
			}
		}
		fmt.Fprintf(p, "Deliveries: %s\n", strings.Join(parts, ", "))
		listed := 0
		for _, d := range st.Deliveries {
			if d.State == groundhog.DeliveryAccepted {
				continue
			}
			if listed == 5 {
				p.WriteString("  … more not listed\n")
				break
			}
			listed++
			fmt.Fprintf(p, "  %s\n", deliveryLine(d))
		}
		if settled(st) {
			// An --active read leaves these out of the list: say so, so a reader
			// does not take the missing list for the whole story.
			fmt.Fprintf(p, "Settled: %d accepted, %d missing (not listed)\n", st.Settled.Accepted, st.Settled.Missing)
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
	fmt.Fprintf(p, "\nRevision %d", st.Revision)
	if st.JournalGeneration != nil {
		fmt.Fprintf(p, ", journal generation %d", *st.JournalGeneration)
	}
	p.WriteString("\n")
}

// deliveryLine is one delivery as one line: where it is, its state, why, and
// the newest attempt's outcome.
func deliveryLine(d groundhog.Delivery) string {
	s := short(d.Path, 120) + ": " + strings.ReplaceAll(string(d.State), "_", " ")
	if d.Reason != nil {
		s += " (" + short(*d.Reason, 120) + ")"
	}
	if k := len(d.Attempts); k > 0 {
		a := d.Attempts[k-1]
		s += fmt.Sprintf("; attempt %d of %d %s", k, k, strings.ReplaceAll(a.Outcome, "_", " "))
		if a.ExitCode != nil {
			s += fmt.Sprintf(", exit status %d", *a.ExitCode)
		}
		if a.Signal != nil {
			s += ", signal " + short(*a.Signal, 20)
		}
		if e := problemText(a.Error); e != "" {
			s += ", " + e
		}
		if a.OwnerLabel != nil && *a.OwnerLabel != "" {
			// Whose call it was, as the record says (Groundhog keeps it after the owner
			// has gone, so an unresolved attempt still names who admitted it).
			s += ", admitted by " + short(*a.OwnerLabel, 60)
		}
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
	switch {
	case iv.Inspection.Err != nil: // the failure is shown in the tree row; the old status says nothing about trust
	case iv.Health == core.HealthStale:
		parts = append(parts, "stale")
	case iv.Health == core.HealthInconsistent:
		parts = append(parts, "inconsistent")
	}
	if v, ok := core.PendingVerdict(iv); ok {
		if b := verdictBadge(v); b != "" {
			parts = append(parts, b)
		}
	}
	if len(st.Schedule) > 0 { // an empty count says nothing: a blank says it
		parts = append(parts, plural(len(st.Schedule), "item", "items"))
	}
	counts := st.DeliveryCounts()
	pending := counts[groundhog.DeliveryPending] + counts[groundhog.DeliveryInFlight]
	failed := failedCount(st)
	if pending > 0 {
		parts = append(parts, fmt.Sprintf("%d pending", pending))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	return strings.Join(parts, " · ")
}

// verdictBadge is the verdict in a few words for the tree, or "" when nothing
// has been seen ticking the instance: a missing heartbeat mark after the state
// word already says that, and a row says only what matters. It carries no
// tick age either: the mark says how recent the tick looks, and the preview
// keeps the exact age with the read time and the fuller sentence.
func verdictBadge(v core.Verdict) string {
	switch v.Outlook {
	case core.OutlookNo:
		return "paused: won't run"
	case core.OutlookYes:
		if v.HandoffsWait() {
			return "items run, handoffs wait"
		}
		return "will run"
	case core.OutlookProbably:
		if v.HandoffsWait() {
			return "probably, handoffs wait"
		}
		return "probably"
	}
	return ""
}

// verdictPreview is the verdict as a sentence with what it rests on, short
// enough to show whole in the 80x24 preview.
func verdictPreview(v core.Verdict) string {
	tick := ""
	if v.HasTick {
		tick = fmt.Sprintf("%s before the %s read", ageText(v.Age), clock(v.ObservedAt))
	}
	if v.HandoffsWait() {
		return handoffsWait(v, tick)
	}
	switch v.Outlook {
	case core.OutlookNo:
		return "Pending work: no, the instance is paused."
	case core.OutlookYes:
		if tick == "" {
			return "Pending work: yes, this session's runner is running (no tick recorded yet)."
		}
		return "Pending work: yes, this session's runner ticked " + tick + "."
	case core.OutlookProbably:
		return "Pending work: probably, something outside Easyhog ticked it " + tick + "."
	}
	if tick == "" {
		return "Pending work: nothing seen ticking it; no tick recorded."
	}
	return "Pending work: nothing seen ticking it; last tick " + tick + "."
}

// handoffsWait is the verdict when an uncertain delivery has raised Groundhog's
// barrier: items fire, but no handoff is made until each
// uncertain delivery is recovered, so it is not a plain yes.
func handoffsWait(v core.Verdict, tick string) string {
	text := "Pending work: items fire, but handoffs wait until " + plural(v.Uncertain, "uncertain delivery is", "uncertain deliveries are") + " recovered"
	who := "this session's runner"
	if v.Outlook == core.OutlookProbably {
		who = "something outside Easyhog"
	}
	if tick == "" {
		return text + " (" + who + " is running; no tick recorded yet)."
	}
	return text + " (" + who + " ticked " + tick + ")."
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
	"owned":           "an owner is recorded",
	"invalid_state":   "the instance's state is invalid",
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
func scheduleNodes(iv core.InstanceView, info *core.RunnerInfo, supervised bool) []Node {
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
			n := Node{Key: inst + "#" + name, Label: labels[i], State: StateScheduled,
				Payload: &PayloadRef{Instance: inst, Rel: it.Path}}
			own := false
			for _, by := range it.PausedBy {
				own = own || by == it.Path
			}
			var failedDelivery, waiting bool
			var related []groundhog.Delivery
			for _, d := range st.Deliveries {
				if d.SchedulePath != nil && (*d.SchedulePath == it.Path || !clash && core.ItemIdentity(*d.SchedulePath) == name) {
					related = append(related, d)
					failedDelivery = failedDelivery || failedState(d.State)
					waiting = waiting || d.State == groundhog.DeliveryPending || d.State == groundhog.DeliveryInFlight
				}
			}
			word := dueWord(it)
			switch {
			case failedDelivery:
				n.State = StateFailed
			case len(it.PausedBy) > 0:
				n.State = StatePaused
			case waiting:
				// A delivery of this item waits for its hand-over: the one thing
				// "pending" means (the badge counts these too). Otherwise the item
				// is merely scheduled.
				n.State = StatePending
			}
			n.Detail = cadenceText(it.Cadence) + ", " + word
			n.Badge, n.Due = itemBadge(it, word)
			// The settling actions an item offers are its own deliveries' (the
			// same core.OffersOn rule as the instance's, limited to this item).
			var settleOffer, toggleOffer []Action
			var toggleWhy string
			if supervised {
				actions, _ := core.OffersOn(iv, info, name)
				for _, a := range actions {
					if a.Settles() {
						settleOffer = append(settleOffer, Action(a))
					}
				}
				var toggles []core.Action
				toggles, toggleWhy = core.ItemOffers(iv, info, it.Path)
				for _, a := range toggles {
					toggleOffer = append(toggleOffer, Action(a))
				}
				n.Actions = append(append(n.Actions, toggleOffer...), settleOffer...)
				n.ActionNotes = settleNotes(settleOffer, st, name)
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
				p.WriteString("The row counts down to this. The runner fires the item on its first tick at or after it, so with a 60 s tick that can be up to a minute later.\n")
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
			case supervised && toggleWhy != "":
				fmt.Fprintf(&p, "Pausing is not offered: %s\n", short(toggleWhy, 300))
			}
			if len(it.BlockedBy) > 0 {
				fmt.Fprintf(&p, "Firing held: %s\n", blockedText(it.BlockedBy))
			}
			for _, d := range related {
				fmt.Fprintf(&p, "Delivery: %s\n", deliveryLine(d))
			}
			if len(settleOffer) > 0 {
				fmt.Fprintf(&p, "Palette offers: %s (this item's deliveries only)\n", actionNames(settleOffer))
			}
			if settled(st) {
				p.WriteString("Settled deliveries (accepted or missing) are counted, not listed: only unfinished ones appear above.\n")
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
