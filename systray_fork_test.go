package systray

// Regression tests for fork-specific behaviour that the upstream
// channel-lifecycle rewrite (ResetMenu/Remove/update) had to preserve.

import "testing"

func isClosed(ch chan struct{}) bool {
	select {
	case _, ok := <-ch:
		return !ok
	default:
		return false
	}
}

// fork semantics: ResetMenu must close every item's ClickedCh (c0199b8)
func TestForkResetMenuClosesChannels(t *testing.T) {
	for id := range menuItems {
		delete(menuItems, id)
	}
	parent := AddMenuItem("p", "")
	child := parent.AddSubMenuItem("c", "")
	other := AddMenuItem("o", "")

	ResetMenu()

	for name, it := range map[string]*MenuItem{"parent": parent, "child": child, "other": other} {
		if !isClosed(it.ClickedCh) {
			t.Errorf("%s: ClickedCh not closed by ResetMenu", name)
		}
	}
	if len(menuItems) != 0 {
		t.Errorf("menuItems not empty after ResetMenu: %v", menuItems)
	}
}

// fork semantics: SetTitleQuiet still updates a live item, and is a no-op once removed
func TestForkQuietUpdate(t *testing.T) {
	for id := range menuItems {
		delete(menuItems, id)
	}
	it := AddMenuItem("a", "")
	it.SetTitleQuiet("b")
	if it.title != "b" {
		t.Errorf("SetTitleQuiet did not set title, got %q", it.title)
	}
	it.Remove()
	it.SetTitleQuiet("c") // must not resurrect
	if _, ok := menuItems[it.id]; ok {
		t.Error("removed item resurrected by SetTitleQuiet")
	}
}

// double ResetMenu must not panic on already-closed channels
func TestForkDoubleResetMenu(t *testing.T) {
	for id := range menuItems {
		delete(menuItems, id)
	}
	AddMenuItem("x", "")
	ResetMenu()
	ResetMenu()
}
