package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/discovery"
)

// Every instance directory is named ".groundhog", so the terminal names an
// instance by its project folder ("notes", not
// "notes/.groundhog"). discovery's Display keeps the suffix.
func TestInstanceLabelDropsTheGroundhogSuffix(t *testing.T) {
	for _, c := range []struct {
		name, path, display, want string
	}{
		{"plain", "/r/notes/.groundhog", "notes/.groundhog", "notes"},
		{"nested", "/r/vendored/b/.groundhog", "vendored/b/.groundhog", "vendored/b"},
		{"root is a project", "/work/proj/.groundhog", ".groundhog", "proj"},
		{"root is the instance", "/work/proj/.groundhog", ".", "proj"},
		{"a name that merely ends in the word", "/r/my.groundhog/.groundhog", "my.groundhog/.groundhog", "my.groundhog"},
		{"not an instance spelling", "/r/odd", "odd", "odd"},
	} {
		in := discovery.Instance{Path: c.path, Display: c.display}
		if got := instanceLabel(in); got != c.want {
			t.Errorf("%s: label %q, want %q", c.name, got, c.want)
		}
		if got := instanceNode(core.InstanceView{Instance: in}, nil, false).Label; got != c.want {
			t.Errorf("%s: node label %q, want %q", c.name, got, c.want)
		}
	}
}

func TestInstanceLabelNeverEmptyOrDot(t *testing.T) {
	for _, in := range []discovery.Instance{
		{Path: "/.groundhog", Display: ".groundhog"},
		{Path: "/", Display: "."},
	} {
		if got := instanceLabel(in); got == "" || got == "." && in.Display != "." {
			t.Errorf("%+v: label %q", in, got)
		}
	}
}

func labelRig(t *testing.T, v core.InstanceView) Model {
	t.Helper()
	m := NewWithData(FromViewWith(core.View{Root: "/r", Instances: []core.InstanceView{v}}, ticks()))
	m, _ = feed(m, tea.WindowSizeMsg{Width: 120, Height: 30})
	return m
}

// The tree row, the preview header, the palette's "on <instance>" label and the
// "in <instance>" subtitle of an item all read the same, with no suffix.
func TestEveryPlaceTheInstanceLabelShowsDropsTheSuffix(t *testing.T) {
	v := inspected() // /r/alpha/.groundhog, with items
	m := labelRig(t, v)

	screen := ansi.Strip(m.View())
	if !strings.Contains(screen, "❯ ▸") || !strings.Contains(screen, " alpha ") {
		t.Fatalf("tree row does not read alpha:\n%s", screen)
	}
	for _, l := range strings.Split(screen, "\n") {
		if strings.Contains(l, "❯ ▸") && strings.Contains(l, "/.groundhog") {
			t.Errorf("tree row keeps the suffix: %q", l)
		}
	}
	if head := ansi.Strip(m.previewLines(60)[0]); !strings.HasSuffix(strings.TrimSpace(head), "alpha") {
		t.Errorf("preview header %q", head)
	}

	pal, _ := feed(m, typeText(">")[0])
	if out := ansi.Strip(pal.View()); !strings.Contains(out, "on alpha") || strings.Contains(out, "on alpha/") {
		t.Errorf("palette label:\n%s", out)
	}

	item, _ := feed(m, key(tea.KeyRight), key(tea.KeyDown))
	sub := ansi.Strip(item.previewLines(80)[1])
	if !strings.Contains(sub, " · in alpha") || strings.Contains(sub, ".groundhog") {
		t.Errorf("item subtitle %q", sub)
	}
}

// A search root that is a project, or the instance itself, still names the project.
func TestRootThatIsAProjectOrTheInstanceShowsTheProjectName(t *testing.T) {
	for _, display := range []string{".groundhog", "."} {
		v := supported("x")
		v.Instance = discovery.Instance{Path: "/work/myproj/.groundhog", Display: display}
		m := labelRig(t, v)
		screen := ansi.Strip(m.View())
		if !strings.Contains(screen, "❯   ○ myproj ") {
			t.Errorf("Display %q: tree row does not read myproj:\n%s", display, screen)
		}
		pal, _ := feed(m, typeText(">")[0])
		if out := ansi.Strip(pal.View()); !strings.Contains(out, "on myproj") {
			t.Errorf("Display %q: palette does not name myproj:\n%s", display, out)
		}
	}
}

// Labels stay sanitised once the suffix goes.
func TestTrimmedLabelStaysInert(t *testing.T) {
	v := supported("x")
	v.Instance.Display = "evil\x1b[2J\x07dir/.groundhog"
	m := labelRig(t, v)
	if out := m.View(); strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\x07") {
		t.Fatal("control sequence reached the frame")
	}
}
