package tui

import (
	"path"
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/zealtv/easyhog/internal/payload"
)

// Styling for the preview pane, applied at render time to text the adapter and
// the payload reader have already produced: nothing here reads a file, runs
// anything or changes what the text says. It runs only when the terminal gets
// colour (Model.colour); with colour off the preview is the plain text it was
// before this file existed, byte for byte.
//
// The adapter writes its preview as plain lines of a few regular shapes, so the
// shapes are recognised here (classifyPreviewLine) and a line that matches none
// is left exactly as it is. Parsing finished strings is the cost of not giving
// adapt.go a typed preview.

// previewKind names a shape of line the adapter writes into Node.Preview.
type previewKind int

const (
	// lineOther is every line the classifier does not know. It is never styled.
	lineOther previewKind = iota
	// lineTitle is the preview's first line: an instance's path, "Item <path>",
	// or "Could not inspect <path>". The path starts at the returned index.
	lineTitle
	// lineField is "Label: value" for a label the adapter writes. The value
	// starts at the returned index.
	lineField
	// lineDelivery is an indented "path: glyph state; attempt …" line listed under
	// Deliveries. The text after "path: " starts at the returned index.
	lineDelivery
	// lineMore is the indented "… more not listed" line.
	lineMore
)

// fieldLabels are the labels adapt.go puts before a colon.
// A line whose text before ": " is not in this list is not a field.
var fieldLabels = []string{
	"Kind", "Palette offers", "Support", "Pause", "Schedule", "Deliveries", "Diagnostic",
	"Last inspected", "From directory listings (nothing run)", "Listing problem", "Pending work", "Heartbeat",
	"Item id", "Cadence", "Due", "Firing held", "Delivery", "Nothing is offered",
	"Pausing is not offered", "Next due",
}

var errorAtRE = regexp.MustCompile(`^Error at \d{2}:\d{2}:\d{2}: `)

// classifyPreviewLine says which shape a preview line has and where its
// interesting part starts. It is pure and knows nothing but the line and
// whether it is the preview's first.
func classifyPreviewLine(line string, first bool) (previewKind, int) {
	if first {
		switch {
		case strings.HasPrefix(line, "/"):
			return lineTitle, 0
		case strings.HasPrefix(line, "Item "):
			return lineTitle, len("Item ")
		case strings.HasPrefix(line, "Could not inspect "):
			return lineTitle, len("Could not inspect ")
		}
	}
	for _, label := range fieldLabels {
		if strings.HasPrefix(line, label+": ") {
			return lineField, len(label) + 2
		}
	}
	if loc := errorAtRE.FindStringIndex(line); loc != nil {
		return lineField, loc[1]
	}
	if line == "  … more not listed" {
		return lineMore, 0
	}
	if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") {
		if i := strings.Index(line, ": "); i > 2 {
			return lineDelivery, i + 2
		}
	}
	return lineOther, 0
}

// valueRE finds what a field's value may colour: a clock time, or a state word.
var valueRE = regexp.MustCompile(`\b(?:\d{2}:\d{2}:\d{2}|running|paused|failed|pending|interrupted)\b`)

var stateWordStyle = map[string]State{
	"running": StateRunning, "paused": StatePaused, "failed": StateFailed, "pending": StatePending,
}

// valueSpans colours times faint and, when words is set, state words in their
// state colour. Everything else keeps the default style.
func valueSpans(text string, words bool, st styles) []span {
	var out []span
	at := 0
	for _, loc := range valueRE.FindAllStringIndex(text, -1) {
		found := text[loc[0]:loc[1]]
		var style lipgloss.Style
		id := ""
		switch {
		case found[0] >= '0' && found[0] <= '9':
			style, id = st.faint, "time"
		case !words:
			continue
		case found == "interrupted":
			style, id = st.warn, "word:interrupted"
		default:
			style, id = st.state[stateWordStyle[found]], "word:"+found
		}
		if loc[0] > at {
			out = append(out, span{text: text[at:loc[0]]})
		}
		out = append(out, span{text: found, style: style, id: id})
		at = loc[1]
	}
	if at < len(text) {
		out = append(out, span{text: text[at:]})
	}
	return out
}

// stylePreviewLine styles one line of an adapter preview, wrapped to w. It
// returns nil for a line the classifier does not know: the caller then shows
// that line as it always did.
func stylePreviewLine(line string, first bool, w int, st styles) []string {
	kind, at := classifyPreviewLine(line, first)
	noStyle := lipgloss.NewStyle()
	switch kind {
	case lineTitle:
		spans := []span{{text: line[at:], style: st.faint, id: "path"}}
		if at > 0 {
			spans = append([]span{{text: line[:at], style: st.label, id: "label"}}, spans...)
		}
		return wrap(tokens(spans), w, "", "", noStyle)
	case lineField:
		label, value := line[:at], line[at:]
		spans := []span{{text: label, style: st.label, id: "label"}}
		words := strings.HasPrefix(label, "Runner: ") || strings.HasPrefix(label, "Deliveries: ") || strings.HasPrefix(label, "Delivery: ")
		if strings.HasPrefix(label, "Delivery: ") {
			if i := strings.Index(value, ": "); i > 0 {
				spans = append(spans, span{text: value[:i+1], style: st.faint, id: "path"})
				value = value[i+1:]
			}
		}
		return wrap(tokens(append(spans, valueSpans(value, words, st)...)), w, "", "", noStyle)
	case lineDelivery:
		spans := []span{{text: line[2 : at-2], style: st.faint, id: "path"}, {text: ":"}}
		return wrap(tokens(append(spans, valueSpans(line[at-1:], true, st)...)), w, "  ", "  ", noStyle)
	case lineMore:
		return wrap(tokens([]span{{text: strings.TrimSpace(line), style: st.faint, id: "faint"}}), w, "  ", "  ", noStyle)
	}
	return nil
}

// fileHeaderRE is the line internal/payload writes before each file's text:
// the file's path, two spaces and its size. It is never indented.
var fileHeaderRE = regexp.MustCompile(`^(\S.*?)  (\d+ bytes|\d+\.\d KiB|\d+\.\d MiB)$`)

// stylePayload styles the text internal/payload produced for an item: each file's
// header line, and the body of a markdown or JSON file. Every other line, every
// other file's body and every notice the payload reader wrote is shown as it
// always was. text is already cleaned of control characters.
func stylePayload(text string, w int, st styles) []string {
	var out []string
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); {
		head := fileHeaderRE.FindStringSubmatch(lines[i])
		if head == nil {
			out = append(out, wrapPlain(lines[i], w)...)
			i++
			continue
		}
		out = append(out, wrap(tokens([]span{
			{text: head[1], style: st.mdHeading, id: "file"},
			{text: "  " + head[2], style: st.faint, id: "size"},
		}), w, "", "", lipgloss.NewStyle())...)
		j := i + 1
		for j < len(lines) && strings.HasPrefix(lines[j], "    ") {
			j++
		}
		out = append(out, styleBody(head[1], lines[i+1:j], w, st)...)
		i = j
	}
	return out
}

const bodyIndent = "    "

// styleBody styles one file's indented body. Only a file named *.md or
// *.markdown is read as markdown, and only *.json gets keys and strings
// coloured; any other file, and the reader's own notices in any file, stay plain.
func styleBody(name string, body []string, w int, st styles) []string {
	content, tail := splitBody(body)
	var out []string
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".markdown":
		if len(content) == 0 {
			break
		}
		room := max(1, w-len(bodyIndent))
		for _, line := range renderMarkdown(strings.Join(content, "\n"), room, st, true) {
			if line != "" {
				line = bodyIndent + line
			}
			out = append(out, line)
		}
	case ".json":
		for _, line := range content {
			out = append(out, jsonLine(line, w, st)...)
		}
	default:
		content, tail = nil, body
	}
	for _, line := range tail {
		out = append(out, wrapPlain(line, w)...)
	}
	return out
}

// splitBody separates a file's own text (with the indent removed) from the
// notices internal/payload adds around it: a lone "(empty)", "(binary file…)"
// or "(cannot be read…)" line, or the closing "… (shows the start only…)" line.
// The notices come back whole, indent included.
func splitBody(body []string) (content, tail []string) {
	if len(body) == 1 {
		if t := strings.TrimPrefix(body[0], bodyIndent); strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")") {
			return nil, body
		}
	}
	if n := len(body); n > 0 && strings.HasPrefix(strings.TrimPrefix(body[n-1], bodyIndent), payload.StartOnlyNote) {
		body, tail = body[:n-1], body[n-1:]
	}
	for _, line := range body {
		content = append(content, strings.TrimPrefix(line, bodyIndent))
	}
	return content, tail
}

// jsonStringRE finds a JSON string and, when a colon follows it, marks it a key.
var jsonStringRE = regexp.MustCompile(`"(?:[^"\\]|\\.)*"(\s*:)?`)

// jsonSpans colours a line's keys and strings and leaves everything else (numbers,
// literals, punctuation) as written. JSON is read a line at a time, which is how
// the payload reader cuts a file, so a cut value never needs the whole document.
func jsonSpans(line string, st styles) []span {
	var spans []span
	at := 0
	for _, m := range jsonStringRE.FindAllStringSubmatchIndex(line, -1) {
		end, style, id := m[1], st.jsonString, "json:string"
		if m[2] >= 0 {
			end, style, id = m[2], st.jsonKey, "json:key"
		}
		if m[0] > at {
			spans = append(spans, span{text: line[at:m[0]]})
		}
		spans = append(spans, span{text: line[m[0]:end], style: style, id: id})
		at = end
	}
	if at < len(line) {
		spans = append(spans, span{text: line[at:]})
	}
	return spans
}

func jsonLine(line string, w int, st styles) []string {
	trimmed := strings.TrimLeft(line, " ")
	if trimmed == "" {
		return []string{""}
	}
	indent := bodyIndent + line[:len(line)-len(trimmed)]
	return wrap(tokens(jsonSpans(trimmed, st)), w, indent, indent, lipgloss.NewStyle())
}
