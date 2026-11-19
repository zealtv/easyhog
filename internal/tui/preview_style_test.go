package tui

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/discovery"
	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/payload"
)

// Every shape of line the classifier knows, and the lines it must leave alone.
func TestClassifyPreviewLine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		line  string
		first bool
		kind  previewKind
		at    int
	}{
		{"instance path", "/work/projects/alpha/.groundhog", true, lineTitle, 0},
		{"item title", "Item schedule/daily/09/report", true, lineTitle, 5},
		{"could not inspect", "Could not inspect /work/x/.groundhog", true, lineTitle, 18},
		{"a path that is not first", "/work/projects/alpha/.groundhog", false, lineOther, 0},
		{"Item title that is not first", "Item schedule/daily/09/report", false, lineOther, 0},
		{"an unknown first line", "Something else entirely", true, lineOther, 0},

		{"Kind", "Kind: owned_by_session · Health: ok", false, lineField, 6},
		{"Palette offers", "Palette offers: Run, Open folder", false, lineField, 16},
		{"This session's runner", "This session's runner: started", false, lineField, 23},
		{"Support", "Support: protocol 1", false, lineField, 9},
		{"Error at", "Error at 09:30:15: exit 3", false, lineField, 19},
		{"Ownership", "Ownership: this session (alpha)", false, lineField, 11},
		{"Pause", "Pause: not paused", false, lineField, 7},
		{"Runner", "Runner: running", false, lineField, 8},
		{"Runner's last error", "Runner's last error: exit: boom", false, lineField, 21},
		{"Consumer", "Consumer: configured", false, lineField, 10},
		{"Schedule", "Schedule: 3 items (1 due now, 0 already fired, 1 paused)", false, lineField, 10},
		{"Deliveries", "Deliveries: 1 pending, 1 failed", false, lineField, 12},
		{"Settled", "Settled: 4 accepted, 0 missing (not listed)", false, lineField, 9},
		{"Diagnostic", "Diagnostic: code: message", false, lineField, 12},
		{"Last inspected", "Last inspected: 09:30:15", false, lineField, 16},
		{"directory listings", "From directory listings (nothing run): 3 schedule items, 2 entries in out/", false, lineField, 39},
		{"Listing problem", "Listing problem: too many", false, lineField, 17},
		{"Pending work", "Pending work: yes, this session's runner ticked 5s before the 09:30:15 read.", false, lineField, 14},
		{"Heartbeat", "Heartbeat: fresh, judged against a tick expected every 1m00s (this session's runner's interval): fresh up to 1m30s, late up to 3m00s.", false, lineField, 11},
		{"Item id", "Item id: report", false, lineField, 9},
		{"Cadence", "Cadence: daily at 09:00", false, lineField, 9},
		{"Due", "Due: due now", false, lineField, 5},
		{"Firing held", "Firing held: not due", false, lineField, 13},
		{"Delivery", "Delivery: out/x: failed (reason)", false, lineField, 10},
		{"not offered", "Run and Stop are not offered: the script lacks the runtime commands (read-only).", false, lineField, 30},

		{"a listed delivery", "  out/report-2026-11-15: failed (consumer exited); attempt 1 of 1 failed", false, lineDelivery, 25},
		{"more not listed", "  … more not listed", false, lineMore, 0},

		{"an unknown label", "Mystery: something", false, lineOther, 0},
		{"a label with no colon", "Pause", false, lineOther, 0},
		{"a label with no space after the colon", "Pause:not paused", false, lineOther, 0},
		{"a lowercase label", "pause: not paused", false, lineOther, 0},
		{"a sentence", "Its script was not run. Missing or unusable:", false, lineOther, 0},
		{"a list item", "- script", false, lineOther, 0},
		{"Settled deliveries sentence", "Settled deliveries (accepted or missing) are counted, not listed: only unfinished ones appear above.", false, lineOther, 0},
		{"Revision", "Revision 41, journal generation 8", false, lineOther, 0},
		{"As of", "As of 09:30:15", false, lineOther, 0},
		{"a blank line", "", false, lineOther, 0},
		{"a blank first line", "", true, lineOther, 0},
		{"three-space indent", "   out/x: failed", false, lineOther, 0},
		{"two-space indent without a colon", "  out/x", false, lineOther, 0},
		{"a payload-style line", "    key: value", false, lineOther, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, at := classifyPreviewLine(tc.line, tc.first)
			if kind != tc.kind || at != tc.at {
				t.Errorf("classifyPreviewLine(%q, %v) = %v, %d; want %v, %d", tc.line, tc.first, kind, at, tc.kind, tc.at)
			}
		})
	}
}

// A line the classifier does not know is not styled at all.
func TestUnrecognisedLinesComeBackUnstyled(t *testing.T) {
	for _, line := range []string{"Mystery: 09:30:15 running", "- failed", "Revision 3 at 09:30:15", ""} {
		if got := stylePreviewLine(line, false, 80, newStyles()); got != nil {
			t.Errorf("stylePreviewLine(%q) = %q, want nil", line, got)
		}
	}
}

// The adapter's real previews fall into the shapes the classifier knows.
func TestTheAdaptersPreviewsUseKnownShapes(t *testing.T) {
	n := instanceNode(inspected(), nil, false)
	lines := strings.Split(n.Preview, "\n")
	if k, _ := classifyPreviewLine(lines[0], true); k != lineTitle {
		t.Errorf("first line %q is not a title", lines[0])
	}
	want := map[string]previewKind{"Kind: ": lineField, "Ownership: ": lineField, "Pause: ": lineField, "Runner: ": lineField,
		"Consumer: ": lineField, "Schedule: ": lineField, "Deliveries: ": lineField, "Last inspected: ": lineField,
		"  out/report-2026-11-15": lineDelivery, "  out/waiting": lineDelivery}
	for prefix, kind := range want {
		found := false
		for _, line := range lines {
			if strings.HasPrefix(line, prefix) {
				found = true
				if k, _ := classifyPreviewLine(line, false); k != kind {
					t.Errorf("%q classified %v, want %v", line, k, kind)
				}
			}
		}
		if !found {
			t.Errorf("no preview line starts %q:\n%s", prefix, n.Preview)
		}
	}
	item := instanceNode(inspected(), nil, false).Children[0]
	for _, line := range strings.Split(item.Preview, "\n")[1:] {
		for _, label := range []string{"Item id: ", "Cadence: ", "Due: ", "Pause: ", "Delivery: "} {
			if strings.HasPrefix(line, label) {
				if k, _ := classifyPreviewLine(line, false); k != lineField {
					t.Errorf("%q classified %v, want a field", line, k)
				}
			}
		}
	}
}

// Times go faint and state words take their state's colour, only where the
// field says states; the text is never altered.
func TestValueSpansColourTimesAndStateWords(t *testing.T) {
	st := newStyles()
	spans := valueSpans("1 pending, 2 failed, 1 uncertain, 1 paused, at 09:30:15", true, st)
	byText := map[string]lipgloss.Style{}
	var joined string
	for _, s := range spans {
		byText[s.text] = s.style
		joined += s.text
	}
	if joined != "1 pending, 2 failed, 1 uncertain, 1 paused, at 09:30:15" {
		t.Errorf("text changed: %q", joined)
	}
	for word, state := range map[string]State{"pending": StatePending, "failed": StateFailed, "paused": StatePaused} {
		if byText[word].GetForeground() != st.state[state].GetForeground() {
			t.Errorf("%q is not in the %s colour", word, state.label())
		}
	}
	if byText["uncertain"].GetForeground() != st.warn.GetForeground() || byText["09:30:15"].GetForeground() != st.faint.GetForeground() {
		t.Error("uncertain is not warn, or the time is not faint")
	}
	for _, s := range valueSpans("not paused at 09:30:15", false, st) {
		if s.text == "paused" {
			t.Error("a state word was coloured in a field that does not carry states")
		}
	}
}

// --- colour off is today's preview ----------------------------------------

// previewLinesBeforeStyling is Model.previewLines exactly as it stood before
// the preview was styled (as of 254fc0c), kept to prove that colour off changes
// nothing: not a character, not a line break.
func previewLinesBeforeStyling(m Model, w int) []string {
	r := m.selectedRow()
	if r == nil {
		return nil
	}
	n := r.node
	var lines []string
	s := m.styles
	head := s.state[n.State].Render(n.State.glyph()) + " " + lipgloss.NewStyle().Bold(true).Render(cleanLine(n.Label))
	lines = append(lines, ansi.Truncate(head, w, "…"))
	sub := n.State.label()
	if r.depth > 0 {
		sub += " · in " + cleanLine(r.instance.Label)
	}
	if n.Detail != "" {
		sub += " · " + cleanLine(n.Detail)
	}
	lines = append(lines, ansi.Truncate(s.soft.Render(sub), w, "…"), "")
	if l := m.last; l != nil && l.instance == r.instance.Key {
		style := s.soft
		if l.failure {
			style = s.warn
		}
		for _, line := range wrapPlain("Last action: "+cleanLine(l.text), w) {
			lines = append(lines, style.Render(line))
		}
		lines = append(lines, "")
	}
	if strings.TrimSpace(n.Preview) == "" {
		return append(lines, s.faint.Render("Nothing to preview."))
	}
	body := clean(n.Preview)
	if n.Payload != nil && m.readPayload != nil {
		body = strings.TrimRight(body, "\n")
	}
	for _, line := range strings.Split(body, "\n") {
		lines = append(lines, wrapPlain(line, w)...)
	}
	if n.Payload != nil && m.readPayload != nil {
		lines = append(lines, "", s.soft.Render("Payload, read as text (nothing is run)"))
		text := "reading…"
		if m.payload.key == n.Key && m.payload.loaded {
			text = m.payload.text
		}
		for _, line := range strings.Split(strings.TrimRight(clean(text), "\n"), "\n") {
			lines = append(lines, wrapPlain(line, w)...)
		}
	}
	return lines
}

const richPayload = "notes.md  120 bytes\n" +
	"    # Notes\n    \n    Some **bold** and `code` and [[a-link]].\n    \n    - one\n    - two\n    \n    > quoted\n    \n    ```sh\n    echo '**not bold**'\n    ```\n" +
	"data.json  60 bytes\n" +
	"    {\n      \"name\": \"x\",\n      \"n\": 3\n    }\n" +
	"plain.txt  30 bytes\n" +
	"    # not a heading, **not bold**\n" +
	"big.md  9.0 KiB\n" +
	"    # Start\n    … (shows the start only; the file has 9.0 KiB)\n" +
	"empty.md  0 bytes\n    (empty)\n" +
	"sub/\n" +
	"sub/deep.md  8 bytes\n    # deep\n" +
	"… 3 more not shown"

func richModel(t *testing.T) Model {
	t.Helper()
	text := map[string]string{}
	for _, rel := range []string{"schedule/daily/09/report", "schedule/weekly.paused/mon/review", "schedule/every/15m/beat"} {
		text[rel] = richPayload
	}
	m := payloadModel(t, &reads{text: text}, 100, 40)
	m.last = &outcome{instance: m.selectedRow().instance.Key, text: "Run: started", failure: false}
	return m
}

// With colour off, every row of the tree (the instance and its items, with
// their payloads) previews exactly as it did before styling, at every width.
func TestColourOffPreviewIsByteIdenticalToBeforeStyling(t *testing.T) {
	m := richModel(t)
	m = pump(m, key(tea.KeyRight))
	for row := 0; row < 4; row++ {
		m.colour = false
		for _, w := range []int{200, 100, 60, 38, 20, 5, 1} {
			got, want := m.previewLines(w), previewLinesBeforeStyling(m, w)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("row %d width %d: colour-off preview changed:\n--- got\n%s\n--- want\n%s", row, w, strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		}
		// The test would pass vacuously if the styled path never differed.
		m.colour = true
		if row > 0 && strings.Join(m.previewLines(100), "\n") == strings.Join(previewLinesBeforeStyling(m, 100), "\n") {
			t.Fatalf("row %d: colour on changed nothing, so the identity check proves nothing", row)
		}
		m = pump(m, key(tea.KeyDown))
	}
}

// The same for the whole frame, at the sizes the shell is checked at.
func TestColourOffFrameShowsMarkdownAsWritten(t *testing.T) {
	m := richModel(t)
	m = pump(m, key(tea.KeyRight), key(tea.KeyDown))
	out := ansi.Strip(m.View())
	for _, want := range []string{"# Notes", "**bold**", "`code`", "[[a-link]]", "- one", "> quoted", "```sh", "\"name\": \"x\""} {
		if !strings.Contains(out, want) {
			t.Errorf("colour-off frame lacks %q:\n%s", want, out)
		}
	}
}

// --- colour on: text the file says survives --------------------------------

// Styling never changes the words of the program's own preview: with the
// styles stripped, a wide pane reads the same as the plain preview.
func TestStyledAdapterPreviewReadsTheSameAsPlain(t *testing.T) {
	m := payloadModel(t, &reads{}, 100, 40)
	m.readPayload = nil
	plainLines := previewLinesBeforeStyling(m, 200)
	m.colour = true
	got := m.previewLines(200)
	if len(got) < 10 {
		t.Fatalf("a thin preview: %q", got)
	}
	if strings.Join(got, "\n") != strings.Join(plainLines, "\n") {
		t.Errorf("styled text differs from plain once styles are stripped (no colour profile in tests):\n--- got\n%s\n--- plain\n%s",
			strings.Join(got, "\n"), strings.Join(plainLines, "\n"))
	}
}

func TestStyledLinesFitAndKeepEveryWord(t *testing.T) {
	m := payloadModel(t, &reads{}, 100, 40)
	m.readPayload = nil
	// The head's two lines are cut with an ellipsis by code this test does not cover.
	plainText := strings.Join(strings.Fields(strings.Join(previewLinesBeforeStyling(m, 300)[3:], " ")), " ")
	m.colour = true
	for _, w := range []int{80, 40, 24, 12} {
		lines := m.previewLines(w)
		for _, line := range lines {
			if lipgloss.Width(line) > w {
				t.Errorf("width %d: %q is %d cells wide", w, line, lipgloss.Width(line))
			}
		}
		// Whitespace aside, wrapping at spaces loses nothing but long words split.
		got := strings.ReplaceAll(strings.Join(lines[3:], ""), " ", "")
		if want := strings.ReplaceAll(plainText, " ", ""); got != want {
			t.Errorf("width %d: words lost or changed\n got %q\nwant %q", w, got, want)
		}
	}
}

// Only the body of a *.md or *.markdown file is read as markdown. The header
// line, other files, the payload reader's notices and directory lines stay as
// the reader wrote them.
func TestPayloadMarkdownAppliesOnlyToTheBodyOfMarkdownFiles(t *testing.T) {
	out := ansi.Strip(strings.Join(stylePayload(richPayload, 100, newStyles()), "\n"))
	got := strings.Split(out, "\n")
	has := func(line string) bool {
		for _, g := range got {
			if g == line {
				return true
			}
		}
		return false
	}
	for _, want := range []string{
		"notes.md  120 bytes", // the header, untouched
		"    Notes",           // `# Notes`: the marker dropped, the indent kept
		"    Some bold and code and a-link.",
		"    - one", "    > quoted", // list and quote markers stay
		"    echo '**not bold**'",                            // a fence is not restyled
		"    # not a heading, **not bold**",                  // .txt stays as written
		"    … (shows the start only; the file has 9.0 KiB)", // the reader's notice
		"    (empty)", "sub/", "    deep", "… 3 more not shown",
		"    {", "      \"name\": \"x\",", "      \"n\": 3", "    }", // JSON text unchanged
	} {
		if !has(want) {
			t.Errorf("no line %q in:\n%s", want, out)
		}
	}
	if has("    # Start") == false && !has("    Start") {
		t.Errorf("big.md's text vanished:\n%s", out)
	}
}

// A file name that looks like markup is a header, and a header is never rendered.
func TestAHeaderIsNeverRenderedAsMarkdown(t *testing.T) {
	out := ansi.Strip(strings.Join(stylePayload("# **odd** `name`.md  12 bytes\n    # body", 100, newStyles()), "\n"))
	if !strings.HasPrefix(out, "# **odd** `name`.md  12 bytes\n") {
		t.Errorf("the header was changed:\n%s", out)
	}
	if !strings.HasSuffix(out, "    body") {
		t.Errorf("the markdown body was not rendered:\n%s", out)
	}
}

func TestMarkdownExtensionsAreCaseInsensitiveAndExact(t *testing.T) {
	for name, rendered := range map[string]bool{
		"a.md": true, "A.MD": true, "a.markdown": true, "dir/a.Markdown": true,
		"a.mdx": false, "a.txt": false, "md": false, "a.md.txt": false, "a.rmd": false, "README": false,
	} {
		lines := styleBody(name, []string{"    # Title"}, 40, newStyles())
		got := ansi.Strip(strings.Join(lines, "\n"))
		if rendered && got != "    Title" || !rendered && got != "    # Title" {
			t.Errorf("%s: %q (rendered=%v)", name, got, rendered)
		}
	}
}

func TestPayloadNoticesAreNeverRendered(t *testing.T) {
	for _, body := range [][]string{
		{"    (empty)"},
		{"    (binary file, not shown)"},
		{"    (cannot be read: permission denied)"},
		{"    # Title", "    … (shows the start only; the file has 5.0 KiB)"},
	} {
		got := ansi.Strip(strings.Join(styleBody("n.md", body, 60, newStyles()), "\n"))
		if strings.Contains(strings.Join(body, "\n"), "# Title") {
			if !strings.HasPrefix(got, "    Title\n    … (shows the start only; the file has 5.0 KiB)") {
				t.Errorf("truncation: %q", got)
			}
			continue
		}
		if got != strings.Join(body, "\n") {
			t.Errorf("a notice changed: %q -> %q", body, got)
		}
	}
}

func TestJSONKeysAndStringsAreToldApart(t *testing.T) {
	st := newStyles()
	line := `{"name": "x y", "n": 3, "ok": true, "q": "a \"quoted\" b"}`
	var keys, strs []string
	var joined string
	for _, s := range jsonSpans(line, st) {
		joined += s.text
		switch s.id {
		case "json:key":
			keys = append(keys, s.text)
		case "json:string":
			strs = append(strs, s.text)
		}
	}
	if joined != line {
		t.Errorf("text changed: %q", joined)
	}
	if strings.Join(keys, ",") != `"name","n","ok","q"` || strings.Join(strs, ",") != `"x y","a \"quoted\" b"` {
		t.Errorf("keys %q strings %q", keys, strs)
	}
	if st.jsonKey.GetForeground() == st.jsonString.GetForeground() || st.jsonKey.GetForeground() == (lipgloss.NoColor{}) {
		t.Error("keys and strings are not coloured differently")
	}
	for _, l := range jsonLine("  "+line, 200, st) {
		if ansi.Strip(l) != "      "+line {
			t.Errorf("rendered text changed: %q", ansi.Strip(l))
		}
	}
	// Minified or cut JSON, and a line that is not JSON at all, keep their text.
	for _, odd := range []string{`{"a":1,"b":"c"}`, `"unterminated`, `not json: "x"`, `]`} {
		if got := ansi.Strip(strings.Join(jsonLine(odd, 80, st), "")); got != "    "+odd {
			t.Errorf("%q came back as %q", odd, got)
		}
	}
}

// A real payload read from a temporary directory, end to end: the reader's
// output goes through the styling with its headers, notices and truncation intact.
func TestStyledPayloadFromARealItemDirectory(t *testing.T) {
	dir := t.TempDir()
	item := filepath.Join(dir, "schedule", "daily", "x")
	if err := os.MkdirAll(item, 0o755); err != nil {
		t.Fatal(err)
	}
	long := "# Long\n\n" + strings.Repeat("A line of **text** that keeps going.\n", 200)
	for name, data := range map[string]string{
		"a.md": "# Title\n\nSome *words*.\n", "b.json": "{\"k\": \"v\"}\n", "c.txt": "# plain\n",
		"d.md": "", "long.md": long,
	} {
		if err := os.WriteFile(filepath.Join(item, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	text := strings.TrimRight(clean(payload.Preview(payload.OS{}, dir, "schedule/daily/x")), "\n")
	out := ansi.Strip(strings.Join(stylePayload(text, 120, newStyles()), "\n"))
	for _, want := range []string{"a.md  ", "    Title", "    Some words.", "b.json  ", "    {\"k\": \"v\"}", "c.txt  ", "    # plain",
		"d.md  0 bytes", "    (empty)", "long.md  ", "    Long", "    … (shows the start only; the file has "} {
		if !strings.Contains(out, want) {
			t.Errorf("styled payload lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "# Title") || strings.Contains(out, "# Long") {
		t.Errorf("a markdown heading kept its marker with colour on:\n%s", out)
	}
	// The same text with colour off is the reader's own, line for line.
	m := NewWithData(FromView(core.View{Instances: []core.InstanceView{inspected()}}), WithPayloads(func(PayloadRef) string { return text }))
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 60})
	m = pump(next.(Model), key(tea.KeyRight), key(tea.KeyDown))
	if got := ansi.Strip(m.View()); !strings.Contains(got, "# Title") || !strings.Contains(got, "# plain") {
		t.Errorf("colour-off frame changed the payload:\n%s", got)
	}
}

// Rendering reads nothing: stylePayload and stylePreviewLine take text, and a
// payload that names a path does not make them open it.
func TestStylingTakesTextAndOpensNothing(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.md")
	if err := os.WriteFile(secret, []byte("# SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := strings.Join(stylePayload("link.md  "+secret+"\n    ["+secret+"](file://"+secret+")\n    "+secret, 200, newStyles()), "\n")
	if strings.Contains(out, "SECRET") {
		t.Error("styling read a file")
	}
}

// --- fieldLabels must not drift from adapt.go --------------------------------

// previewFixtures are instance views that between them reach the branches of
// adapt.go that write preview text: every class and health, errors, notices,
// adoption, deliveries in every state, held firing, settled counts, diagnostics,
// a malformed and a legacy script, and a listing-only instance.
func previewFixtures() []struct {
	v    core.InstanceView
	info *core.RunnerInfo
} {
	type fx = struct {
		v    core.InstanceView
		info *core.RunnerInfo
	}
	var out []fx
	add := func(v core.InstanceView, info *core.RunnerInfo) { out = append(out, fx{v, info}) }

	add(inspected(), nil)
	add(ours("run", groundhog.RunnerRunning, false), nil)
	add(withTick(ours("paused", groundhog.RunnerRunning, true), 12*time.Second, "runner"), nil)
	add(withTick(unowned("other"), 45*time.Second, "standalone"), nil)
	add(unowned("never"), nil)

	// Deliveries in every state, more than the preview lists, with settled counts.
	many := ours("many", groundhog.RunnerRunning, false)
	var dels []groundhog.Delivery
	for _, id := range []string{"d1", "d2", "d3", "d4", "d5", "d6", "d7"} {
		dels = append(dels, delivery(id, groundhog.DeliveryFailed, "schedule/daily/a", "easyhog-1"))
	}
	dels = append(dels, delivery("u", groundhog.DeliveryUncertain, "schedule/daily/b", "easyhog-1"),
		delivery("p", groundhog.DeliveryPending, "schedule/daily/b", ""), delivery("f", groundhog.DeliveryInFlight, "", ""))
	many = withDeliveries(many, dels...)
	st := many.Inspection.Last.Status
	st.Settled = &groundhog.Settled{Accepted: 4, Missing: 1}
	st.Diagnostics = []groundhog.Problem{{Code: "slow", Message: "tick took long"}}
	st.Runner.LastError = &groundhog.Problem{Code: "exit", Message: "boom"}
	st.Consumer = groundhog.Consumer{Configured: true}
	st.Schedule[0].FiredMarker = sp("2026-11-15")
	st.Schedule[0].NextDue = tp(at.Add(26 * time.Hour))
	st.Schedule[1].PausedBy = []string{st.Schedule[1].Path}
	st.Schedule[1].BlockedBy = []string{"subtree_paused"}
	many.Inspection.Last = obs(st)
	many.Files.Runtime, many.Files.OutTruncated = true, true
	many.Files.Problems = []string{"schedule/x: unreadable"}
	add(many, &core.RunnerInfo{Notice: "the runner ended on its own", Adopted: true, AdoptedFrom: at})

	for _, class := range []core.Class{core.ClassExternal, core.ClassUnsupported, core.ClassNotInspected, core.ClassLegacy, core.ClassFailing} {
		add(iv("c-"+class.String(), class, core.HealthOK, core.Inspection{Last: obs(okStatus())}), nil)
	}
	add(iv("stale", core.ClassUnowned, core.HealthStale, core.Inspection{Last: obs(okStatus())}), nil)
	bad := okStatus()
	bad.Consistent = false
	add(iv("inconsistent", core.ClassUnowned, core.HealthInconsistent, core.Inspection{Last: obs(bad), Attempts: 3}), nil)
	add(iv("failing", core.ClassUnowned, core.HealthOK, core.Inspection{Last: obs(okStatus()), Err: errors.New("deadline"), ErrAt: at}), nil)
	add(iv("neverread", core.ClassFailing, core.HealthOK, core.Inspection{Err: errors.New("exit 6"), ErrAt: at}), nil)
	mal := iv("malformed", core.ClassMalformed, core.HealthOK, core.Inspection{})
	mal.Instance.Malformed = []string{"groundhog.sh is missing"}
	add(mal, nil)
	add(iv("legacy", core.ClassLegacy, core.HealthOK, core.Inspection{Support: &groundhog.Support{Reason: "lacks required capabilities: x"}}), nil)
	listed := iv("listed", core.ClassNotInspected, core.HealthOK, core.Inspection{})
	listed.Files.Schedule = []discovery.ScheduleDir{{Path: "daily/x", Leaf: true}, {Path: "daily/y.paused", Leaf: true, Paused: true}}
	add(listed, nil)
	return out
}

// labelShaped is "Capitalised words: something": what a field looks like. Lines
// with digits or punctuation before the colon (a time, a sentence) are not it.
var labelShaped = regexp.MustCompile(`^([A-Z][A-Za-z' ()]*): `)

// Sentences the adapter writes that happen to open with a short phrase and a
// colon. They are prose, not fields, and stay unstyled; a phrase that is not here
// and not in fieldLabels fails the test below.
var proseOpeners = map[string]bool{
	"Not inspected": true, // "Not inspected: nothing was run for this instance. …"
}

// A label added to adapt.go but not to fieldLabels shows up unstyled, quietly.
// This walks the previews adapt.go writes for the fixtures above and fails when
// a line shaped like a field starts with a label the classifier does not know.
func TestFieldLabelsCoverEveryLabelAdaptWrites(t *testing.T) {
	seen := map[string]int{}
	for _, f := range previewFixtures() {
		node := instanceNode(f.v, f.info, true)
		nodes := append([]Node{node}, node.Children...)
		for _, n := range nodes {
			for i, line := range strings.Split(n.Preview, "\n") {
				m := labelShaped.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				seen[m[1]]++
				if kind, _ := classifyPreviewLine(line, i == 0); kind == lineField || proseOpeners[m[1]] {
					continue
				}
				t.Errorf("preview line %q opens with %q, which is not in fieldLabels (add it there, or to proseOpeners if the line is prose)", line, m[1])
			}
		}
	}
	// The walk must have reached the labels it is meant to guard.
	for _, label := range fieldLabels {
		if seen[label] == 0 {
			t.Errorf("no fixture produces a %q line, so the walk cannot see it drift", label)
		}
	}
}
