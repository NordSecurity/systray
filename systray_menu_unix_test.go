//go:build (linux || freebsd || openbsd || netbsd) && !android

package systray

import (
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
)

// resetTestMenu gives each test a clean root. Menu state lives in the package
// level instance, so tests must not run in parallel.
func resetTestMenu() {
	instance.menuLock.Lock()
	defer instance.menuLock.Unlock()
	instance.menu = &menuLayout{
		V1: map[string]dbus.Variant{},
		V2: []dbus.Variant{},
	}
}

// addGroup builds one top-level item with n submenu children, mirroring the
// shape that exposed the empty submenu bug.
func addGroup(t *testing.T, label string, n int) *MenuItem {
	t.Helper()
	parent := newMenuItem(label, "", nil)
	addOrUpdateMenuItem(parent)
	for i := 1; i <= n; i++ {
		addOrUpdateMenuItem(newMenuItem(fmt.Sprintf("%s.%d", label, i), "", parent))
	}
	return parent
}

func layoutOf(t *testing.T, id uint32) *menuLayout {
	t.Helper()
	instance.menuLock.RLock()
	defer instance.menuLock.RUnlock()
	m, ok := findLayout(int32(id))
	if !ok {
		t.Fatalf("no layout for id %d", id)
	}
	return m
}

func hasChildrenDisplay(m *menuLayout) bool {
	_, ok := m.V1["children-display"]
	return ok
}

// childIDs returns the ids of a layout's direct children, in order.
func childIDs(m *menuLayout) []int32 {
	ids := make([]int32, 0, len(m.V2))
	for _, c := range m.V2 {
		ids = append(ids, c.Value().(*menuLayout).V0)
	}
	return ids
}

// TestGetLayoutReturnsFullSubtree covers the empty submenu bug directly.
//
// A client that asked for a shallow layout used to receive every submenu
// parent with children-display="submenu" and an empty child array, and had to
// fetch the children in a separate round trip. Any such follow-up that was
// lost or superseded left the submenu rendered as an empty rectangle. Any
// positive recursion depth must now return the whole subtree.
func TestGetLayoutReturnsFullSubtree(t *testing.T) {
	tests := []struct {
		name              string
		depth             int32
		wantTopLevel      int
		wantGrandchildren int
	}{
		{"depth 0 returns the node alone", 0, 0, 0},
		{"depth 1 is widened to the full subtree", 1, 2, 3},
		{"depth 2 returns the full subtree", 2, 2, 3},
		{"depth -1 returns the full subtree", -1, 2, 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetTestMenu()
			group := addGroup(t, "Group 1", 3)
			addGroup(t, "Group 2", 3)

			_, layout, dbusErr := (&tray{}).GetLayout(0, tt.depth, nil)
			if dbusErr != nil {
				t.Fatalf("GetLayout returned error: %v", dbusErr)
			}

			if got := len(layout.V2); got != tt.wantTopLevel {
				t.Fatalf("top level children = %d, want %d", got, tt.wantTopLevel)
			}
			if tt.wantTopLevel == 0 {
				return
			}

			var found *menuLayout
			for _, c := range layout.V2 {
				if m := c.Value().(*menuLayout); m.V0 == int32(group.id) {
					found = m
				}
			}
			if found == nil {
				t.Fatalf("group %d missing from returned layout", group.id)
			}
			if !hasChildrenDisplay(found) {
				t.Errorf("group is missing children-display")
			}
			if got := len(found.V2); got != tt.wantGrandchildren {
				t.Errorf("group children = %d, want %d "+
					"(children-display set with no children renders as an empty submenu)",
					got, tt.wantGrandchildren)
			}
		})
	}
}

// TestAboutToShow pins the contract that AboutToShow answers "has this
// subtree changed since you last fetched it?", not "does this item have
// children?". Answering the latter makes a client re-read and rebuild the
// submenu on every open, which destroys the popup mid-map: the submenu
// flickers and collapses instead of expanding.
func TestAboutToShow(t *testing.T) {
	resetTestMenu()
	group := addGroup(t, "Group", 3)
	leaf := newMenuItem("Leaf", "", nil)
	addOrUpdateMenuItem(leaf)

	tr := &tray{}

	// Nothing fetched yet, so every known item is stale.
	for _, id := range []int32{int32(group.id), int32(leaf.id)} {
		if got, _ := tr.AboutToShow(id); !got {
			t.Errorf("before any fetch: AboutToShow(%d) = false, want true", id)
		}
	}
	if got, _ := tr.AboutToShow(99999); got {
		t.Errorf("AboutToShow(unknown id) = true, want false")
	}

	// A full fetch brings the client up to date.
	if _, _, dbusErr := tr.GetLayout(0, -1, nil); dbusErr != nil {
		t.Fatalf("GetLayout returned error: %v", dbusErr)
	}
	for _, id := range []int32{int32(group.id), int32(leaf.id)} {
		if got, _ := tr.AboutToShow(id); got {
			t.Errorf("after a full fetch: AboutToShow(%d) = true, want false "+
				"(a client told true rebuilds the submenu on every open)", id)
		}
	}

	// A later change makes it stale again.
	addOrUpdateMenuItem(newMenuItem("Group.4", "", group))
	if got, _ := tr.AboutToShow(int32(group.id)); !got {
		t.Errorf("after a change: AboutToShow(group) = false, want true")
	}
}

// TestAboutToShowGroupDoesNotForceRebuild is the group form of the same
// contract: a client that batches its pre-popup question must not be told
// every item needs an update.
func TestAboutToShowGroupDoesNotForceRebuild(t *testing.T) {
	resetTestMenu()
	group := addGroup(t, "Group", 2)
	leaf := newMenuItem("Leaf", "", nil)
	addOrUpdateMenuItem(leaf)

	tr := &tray{}
	ids := []int32{int32(group.id), int32(leaf.id), 99999}

	updates, idErrors, dbusErr := tr.AboutToShowGroup(ids)
	if dbusErr != nil {
		t.Fatalf("AboutToShowGroup returned error: %v", dbusErr)
	}
	if len(updates) != 2 {
		t.Errorf("before any fetch: updatesNeeded = %v, want both known ids", updates)
	}
	if len(idErrors) != 1 || idErrors[0] != 99999 {
		t.Errorf("idErrors = %v, want [99999]", idErrors)
	}

	if _, _, dbusErr := tr.GetLayout(0, -1, nil); dbusErr != nil {
		t.Fatalf("GetLayout returned error: %v", dbusErr)
	}

	updates, idErrors, _ = tr.AboutToShowGroup(ids)
	if len(updates) != 0 {
		t.Errorf("after a full fetch: updatesNeeded = %v, want none", updates)
	}
	if len(idErrors) != 1 || idErrors[0] != 99999 {
		t.Errorf("idErrors = %v, want [99999]", idErrors)
	}
}

// TestAdvertisedProtocolVersion guards the dbusmenu PROTOCOL version. It was
// being used as a layout revision counter, so the tray advertised version 0
// and then climbed; a client reads this to decide which protocol features the
// server supports, and 0 selects a legacy path.
func TestAdvertisedProtocolVersion(t *testing.T) {
	spec := createMenuPropSpec()["com.canonical.dbusmenu"]["Version"]

	if spec.Value != uint32(3) {
		t.Errorf("Version = %v, want uint32(3)", spec.Value)
	}
	if spec.Writable {
		t.Errorf("Version is writable; the spec declares it access=\"read\"")
	}
	if spec.Emit != prop.EmitConst {
		t.Errorf("Version Emit = %v, want EmitConst: it must never change", spec.Emit)
	}

	// The layout revision must still advance independently of it.
	before := instance.menuVersion.Load()
	resetTestMenu()
	addOrUpdateMenuItem(newMenuItem("x", "", nil))
	if instance.menuVersion.Load() <= before {
		t.Errorf("menuVersion did not advance on a layout change")
	}
}

// TestGetLayoutUnknownID guards against reporting success with an empty
// layout, which tells the client the item exists and has no children.
func TestGetLayoutUnknownID(t *testing.T) {
	resetTestMenu()
	addGroup(t, "Group", 2)

	_, _, dbusErr := (&tray{}).GetLayout(99999, -1, nil)
	if dbusErr == nil {
		t.Fatalf("GetLayout(unknown id) returned no error")
	}
	t.Logf("GetLayout(unknown id) -> %v", dbusErr)
}

// TestAddSeparatorSetsChildrenDisplay guards a submenu whose only child is a
// separator. Without children-display the parent is not rendered as a submenu
// at all.
func TestAddSeparatorSetsChildrenDisplay(t *testing.T) {
	resetTestMenu()
	parent := newMenuItem("Parent", "", nil)
	addOrUpdateMenuItem(parent)

	addSeparator(currentID.Add(1), parent.id)

	if m := layoutOf(t, parent.id); !hasChildrenDisplay(m) {
		t.Errorf("parent of a separator-only submenu has no children-display; V1=%v", m.V1)
	}
	if hasChildrenDisplay(instance.menu) {
		t.Errorf("root gained children-display from a top level separator")
	}
}

func TestAddSeparatorUnknownParentIsIgnored(t *testing.T) {
	resetTestMenu()
	// Must not panic, and must not touch the root.
	addSeparator(currentID.Add(1), 99999)

	if got := len(instance.menu.V2); got != 0 {
		t.Errorf("root children = %d, want 0", got)
	}
}

// TestRemoveNestedSeparatorKeepsRootIntact covers a tree corruption bug: the
// recursive branch of removeSubLayout returned the grandchild slice to its
// caller, which installed it as its own children. Separator.Remove always
// starts the walk at the root, so removing a separator nested in a submenu
// replaced the entire root menu with that submenu's children.
func TestRemoveNestedSeparatorKeepsRootIntact(t *testing.T) {
	resetTestMenu()
	a := newMenuItem("A", "", nil)
	addOrUpdateMenuItem(a)
	b := newMenuItem("B", "", nil)
	addOrUpdateMenuItem(b)
	child := newMenuItem("child", "", a)
	addOrUpdateMenuItem(child)

	sepID := currentID.Add(1)
	addSeparator(sepID, a.id)

	removeSeparator(sepID)

	if got, want := childIDs(instance.menu), []int32{int32(a.id), int32(b.id)}; !equalIDs(got, want) {
		t.Fatalf("root children = %v, want %v (root menu was corrupted)", got, want)
	}
	if got, want := childIDs(layoutOf(t, a.id)), []int32{int32(child.id)}; !equalIDs(got, want) {
		t.Errorf("A's children = %v, want %v", got, want)
	}
}

func TestRemoveNestedMenuItemKeepsRootIntact(t *testing.T) {
	resetTestMenu()
	a := newMenuItem("A", "", nil)
	addOrUpdateMenuItem(a)
	b := newMenuItem("B", "", nil)
	addOrUpdateMenuItem(b)
	child1 := newMenuItem("child1", "", a)
	addOrUpdateMenuItem(child1)
	child2 := newMenuItem("child2", "", a)
	addOrUpdateMenuItem(child2)

	removeMenuItem(child1)

	if got, want := childIDs(instance.menu), []int32{int32(a.id), int32(b.id)}; !equalIDs(got, want) {
		t.Fatalf("root children = %v, want %v", got, want)
	}
	if got, want := childIDs(layoutOf(t, a.id)), []int32{int32(child2.id)}; !equalIDs(got, want) {
		t.Errorf("A's children = %v, want %v", got, want)
	}
}

// TestRemoveLastChildClearsChildrenDisplay covers the deterministic form of
// the empty submenu: a parent that keeps advertising a submenu after its last
// child is gone.
func TestRemoveLastChildClearsChildrenDisplay(t *testing.T) {
	t.Run("direct child", func(t *testing.T) {
		resetTestMenu()
		parent := newMenuItem("Parent", "", nil)
		addOrUpdateMenuItem(parent)
		child := newMenuItem("Child", "", parent)
		addOrUpdateMenuItem(child)

		if m := layoutOf(t, parent.id); !hasChildrenDisplay(m) {
			t.Fatalf("precondition failed: parent has no children-display")
		}

		removeMenuItem(child)

		if m := layoutOf(t, parent.id); hasChildrenDisplay(m) {
			t.Errorf("parent still advertises children-display with %d children", len(m.V2))
		}
	})

	t.Run("nested separator", func(t *testing.T) {
		resetTestMenu()
		parent := newMenuItem("Parent", "", nil)
		addOrUpdateMenuItem(parent)
		sepID := currentID.Add(1)
		addSeparator(sepID, parent.id)

		removeSeparator(sepID)

		if m := layoutOf(t, parent.id); hasChildrenDisplay(m) {
			t.Errorf("parent still advertises children-display with %d children", len(m.V2))
		}
	})
}

// TestConcurrentMenuMutations is meaningful under -race. addSeparator used to
// walk the tree via findLayout before taking the lock, which races any
// concurrent append. A non-root parent is required to reach the walk at all:
// findLayout(0) returns the root without touching the child slice, so a
// root-only version of this test passes even against the unfixed code.
func TestConcurrentMenuMutations(t *testing.T) {
	resetTestMenu()
	parent := newMenuItem("Parent", "", nil)
	addOrUpdateMenuItem(parent)
	addOrUpdateMenuItem(newMenuItem("seed", "", parent))

	const iterations = 500
	var wg sync.WaitGroup
	wg.Add(4)

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			addOrUpdateMenuItem(newMenuItem(fmt.Sprintf("top %d", i), "", nil))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			addSeparator(currentID.Add(1), parent.id)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			(&tray{}).GetLayout(0, -1, nil)
		}
	}()
	go func() {
		defer wg.Done()
		tr := &tray{}
		for i := 0; i < iterations; i++ {
			tr.AboutToShow(int32(parent.id))
			tr.AboutToShowGroup([]int32{int32(parent.id)})
		}
	}()

	wg.Wait()
}

func equalIDs(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestShortcutForItem(t *testing.T) {
	for name, tt := range map[string]struct {
		mods KeyModifier
		key  string
		want [][]string
	}{
		"no shortcut":     {0, "", nil},
		"letter only":     {0, "S", [][]string{{"s"}}},
		"single modifier": {KeyModifierControl, "S", [][]string{{"Control", "s"}}},
		"all modifiers": {
			KeyModifierShift | KeyModifierControl | KeyModifierAlt | KeyModifierSuper, "S",
			[][]string{{"Control", "Alt", "Shift", "Super", "s"}},
		},
		"named key":     {KeyModifierAlt, "Return", [][]string{{"Alt", "Return"}}},
		"renamed key":   {KeyModifierControl, "PageUp", [][]string{{"Control", "Page_Up"}}},
		"function key":  {0, "F5", [][]string{{"F5"}}},
		"unknown key":   {KeyModifierControl, "Menu", nil},
		"modifier only": {KeyModifierControl, "", nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := shortcutForItem(&MenuItem{shortcutMods: tt.mods, shortcutKey: tt.key})
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("shortcutForItem() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApplyItemToLayout_shortcut(t *testing.T) {
	item := &MenuItem{title: "Save", shortcutMods: KeyModifierControl, shortcutKey: "S"}
	layout := &menuLayout{V1: map[string]dbus.Variant{}}

	applyItemToLayout(item, layout)
	shortcut, ok := layout.V1["shortcut"]
	if !ok {
		t.Fatal("expected a shortcut property")
	}
	if want := [][]string{{"Control", "s"}}; !reflect.DeepEqual(shortcut.Value(), want) {
		t.Errorf("shortcut = %v, want %v", shortcut.Value(), want)
	}

	item.shortcutKey = ""
	applyItemToLayout(item, layout)
	if _, ok := layout.V1["shortcut"]; ok {
		t.Error("expected the shortcut property to be removed")
	}
}
