package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/zealtv/easyhog/internal/core"
)

func hiddenModel(t *testing.T, opts ...Option) (Model, *spy, *[]string) {
	t.Helper()
	s := &spy{}
	var reported []string
	opts = append([]Option{WithActions(s.handle), WithSelectionObserver(func(k string) { reported = append(reported, k) })}, opts...)
	m := NewWithData(HiddenSampleData(), opts...)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return next.(Model), s, &reported
}

func rowKeys(m Model) []string {
	var out []string
	for _, r := range m.rows {
		out = append(out, r.node.Key)
	}
	return out
}

func runCmd(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if b, ok := cmd().(tea.BatchMsg); ok {
		for _, c := range b {
			runCmd(c)
		}
	}
}

func TestHiddenInstancesAreLeftOutByDefaultAndCounted(t *testing.T) {
	m, _, _ := hiddenModel(t)
	for _, k := range rowKeys(m) {
		if k == "gamma" || k == "zeta" {
			t.Fatalf("hidden instance %s listed: %v", k, rowKeys(m))
		}
	}
	if len(m.rows) != 4 || m.hiddenCount() != 2 {
		t.Fatalf("rows %v, hidden %d", rowKeys(m), m.hiddenCount())
	}
	v := m.View()
	if !strings.Contains(v, "2 hidden") {
		t.Fatalf("header lacks the count:\n%s", v)
	}
	if !strings.Contains(v, "2 hidden (malformed, legacy or unsupported)") {
		t.Fatalf("status line lacks the count:\n%s", v)
	}
}

func TestPaletteTogglesHiddenInstances(t *testing.T) {
	m, sp, _ := hiddenModel(t)
	for _, word := range []string{"hidden", "show"} {
		m2, _ := feed(m, typeText(":"+word)...)
		got := m2.filtered()
		if len(got) == 0 || got[0].action != ActionToggleHidden || got[0].name != "Show hidden instances" {
			t.Fatalf("typing %q finds %v", word, got)
		}
	}
	m, cmd := feed(m, append(typeText(":hidden"), key(tea.KeyEnter))...)
	if cmd != nil || len(sp.calls) != 0 {
		t.Fatal("the toggle is a view action: it must start no work and reach no handler")
	}
	if !m.showHidden || len(m.rows) != 6 || m.hiddenCount() != 0 {
		t.Fatalf("not shown: %v", rowKeys(m))
	}
	if !strings.Contains(m.View(), "2 hidden, shown") {
		t.Fatalf("header does not say they are shown:\n%s", m.View())
	}
	m, _ = feed(m, typeText(":hide")...)
	if got := m.filtered(); len(got) == 0 || got[0].name != "Hide unsupported instances" {
		t.Fatalf("while shown the action offers hiding: %v", got)
	}
	m, _ = feed(m, key(tea.KeyEnter))
	if m.showHidden || len(m.rows) != 4 {
		t.Fatalf("not hidden again: %v", rowKeys(m))
	}
}

func TestToggleNotOfferedWhenNothingIsHidden(t *testing.T) {
	m, _ := newSpied(t, "full")
	for _, c := range m.commands() {
		if c.action == ActionToggleHidden {
			t.Fatal("offered with nothing hidden")
		}
	}
}

func TestBareKeysNeverToggleHidden(t *testing.T) {
	m, sp, _ := hiddenModel(t)
	for r := rune(0x20); r < 0x7f; r++ {
		next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		if cmd != nil || next.(Model).showHidden {
			t.Fatalf("key %q toggled or scheduled work", r)
		}
	}
	for kt := tea.KeyType(-300); kt <= 127; kt++ {
		if kt == tea.KeyCtrlC {
			continue
		}
		next, _ := m.Update(tea.KeyMsg{Type: kt})
		if next.(Model).showHidden && kt != tea.KeyRunes {
			t.Fatalf("key type %d toggled", kt)
		}
	}
	if len(sp.calls) != 0 {
		t.Fatal(sp.calls)
	}
}

func TestSelectionMovesToAVisibleNeighbourWhenItsInstanceHides(t *testing.T) {
	m, _, reported := hiddenModel(t)
	m, _ = feed(m, append(typeText(":hidden"), key(tea.KeyEnter))...) // show them
	m, _ = feed(m, key(tea.KeyDown), key(tea.KeyDown))                // alpha, beta, gamma
	if m.selectedKey() != "gamma" {
		t.Fatalf("selected %s", m.selectedKey())
	}
	*reported = nil
	m, cmd := feed(m, append(typeText(":hide"), key(tea.KeyEnter))...)
	runCmd(cmd)
	if k := m.selectedKey(); k != "delta" {
		t.Fatalf("highlight on %q, want the next visible neighbour", k)
	}
	if !reportsNoInstance(*reported) {
		t.Fatalf("hiding reported a selection to the refresher: %v", *reported)
	}
}

func TestSelectionUnderAHiddenInstanceMoves(t *testing.T) {
	d := HiddenSampleData()
	for i := range d.Nodes {
		if d.Nodes[i].Key == "gamma" {
			d.Nodes[i].Children = []Node{{Key: "gamma/x", Label: "x"}}
		}
	}
	var reported []string
	m := NewWithData(d, WithSelectionObserver(func(k string) { reported = append(reported, k) }))
	m, _ = feed(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = feed(m, append(typeText(":hidden"), key(tea.KeyEnter))...)
	m = m.setExpanded("gamma", true)
	m, _ = feed(m, key(tea.KeyDown), key(tea.KeyDown), key(tea.KeyDown))
	if m.selectedKey() != "gamma/x" {
		t.Fatalf("selected %s", m.selectedKey())
	}
	reported = nil
	m, cmd := feed(m, append(typeText(":hide"), key(tea.KeyEnter))...)
	runCmd(cmd)
	if m.selectedKey() != "delta" || !reportsNoInstance(reported) {
		t.Fatalf("selected %q, reported %v", m.selectedKey(), reported)
	}
}

func TestASnapshotThatHidesTheSelectionReportsNothing(t *testing.T) {
	d := HiddenSampleData()
	for i := range d.Nodes {
		if d.Nodes[i].Key == "beta" {
			d.Nodes[i].Hidden = false
		}
	}
	var reported []string
	m := NewWithData(d, WithSelectionObserver(func(k string) { reported = append(reported, k) }))
	m, _ = feed(m, tea.WindowSizeMsg{Width: 100, Height: 30}, key(tea.KeyDown))
	if m.selectedKey() != "beta" {
		t.Fatalf("selected %s", m.selectedKey())
	}
	reported = nil
	d2 := HiddenSampleData()
	for i := range d2.Nodes {
		if d2.Nodes[i].Key == "beta" {
			d2.Nodes[i].Hidden = true // the script was negotiated and found unsupported
		}
	}
	m2, cmd := m.Update(SnapshotMsg{Data: d2})
	runCmd(cmd)
	if !reportsNoInstance(reported) || m2.(Model).selectedKey() == "beta" {
		t.Fatalf("selected %q, reported %v", m2.(Model).selectedKey(), reported)
	}
	// A deliberate move afterwards is still reported.
	reported = nil
	_, cmd = m2.Update(key(tea.KeyDown))
	runCmd(cmd)
	if len(reported) != 1 || reported[0] == "" {
		t.Fatalf("a key press moving the highlight was not reported: %v", reported)
	}
}

func TestEmptyVisibleTreeSaysHowToShowHiddenInstances(t *testing.T) {
	d := HiddenSampleData()
	for i := range d.Nodes {
		d.Nodes[i].Hidden = true
	}
	m := NewWithData(d)
	m, _ = feed(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	v := strings.Join(strings.Fields(strings.ReplaceAll(m.View(), "│", " ")), " ")
	for _, want := range []string{"No instances to show", "6 instances are hidden", "Show hidden instances", "0 instances"} {
		if !strings.Contains(v, want) {
			t.Fatalf("missing %q:\n%s", want, v)
		}
	}
	if m.selectedKey() != "" {
		t.Fatal("nothing should be selected")
	}
	m, _ = feed(m, append(typeText(":show"), key(tea.KeyEnter))...)
	if m.selectedKey() == "" || len(m.rows) != 6 {
		t.Fatalf("showing did not restore the tree: %v", rowKeys(m))
	}
}

func TestAdapterHidesByClassOnly(t *testing.T) {
	hid := map[core.Class]bool{core.ClassMalformed: true, core.ClassLegacy: true, core.ClassUnsupported: true}
	for _, c := range []core.Class{core.ClassNotInspected, core.ClassMalformed, core.ClassLegacy, core.ClassUnsupported,
		core.ClassUnowned, core.ClassOwnedBySession, core.ClassExternal, core.ClassFailing} {
		// A host-looking name must not matter, either way.
		for _, name := range []string{"host", "app"} {
			v := iv(name, c, core.HealthOK, core.Inspection{})
			if got := instanceNode(v, nil, false).Hidden; got != hid[c] {
				t.Errorf("class %v name %s: hidden %v, want %v", c, name, got, hid[c])
			}
		}
	}
}

func TestTypingRunDoesNotFindTheToggle(t *testing.T) {
	m, _, _ := hiddenModel(t)
	for _, q := range []string{"run", "stop", "ply"} {
		m2, _ := feed(m, typeText(":"+q)...)
		for _, c := range m2.filtered() {
			if c.action == ActionToggleHidden {
				t.Errorf("typing %q found the toggle", q)
			}
		}
	}
	m2, _ := feed(m, typeText(":uns")...)
	if got := m2.filtered(); len(got) != 1 || got[0].action != ActionToggleHidden {
		t.Errorf("typing uns finds %v", got)
	}
	// Either state of the toggle answers to both words.
	m3, _ := feed(m, append(typeText(":hidden"), key(tea.KeyEnter))...)
	for _, q := range []string{"hidden", "show", "hide"} {
		m4, _ := feed(m3, typeText(":"+q)...)
		if got := m4.filtered(); len(got) == 0 || got[0].action != ActionToggleHidden {
			t.Errorf("while shown, typing %q finds %v", q, got)
		}
	}
}

// reportsNoInstance reports whether the observer heard nothing, or only "no
// selection": either way no script runs.
func reportsNoInstance(reported []string) bool {
	for _, k := range reported {
		if k != "" {
			return false
		}
	}
	return true
}
