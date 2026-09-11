//go:build (linux || freebsd || openbsd || netbsd) && !android

package systray

import (
	"fmt"
	"log"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"

	"github.com/NordSecurity/systray/internal/generated/menu"
)

// SetIcon sets the icon of a menu item.
// iconBytes should be the content of .ico/.jpg/.png
func (item *MenuItem) SetIcon(iconBytes []byte) {
	instance.menuLock.Lock()
	defer instance.menuLock.Unlock()
	m, exists := findLayout(int32(item.id))
	if exists {
		m.V1["icon-data"] = dbus.MakeVariant(iconBytes)
		refresh()
	}
}

// copyLayout makes full copy of layout
func copyLayout(in *menuLayout, depth int32) *menuLayout {
	out := menuLayout{
		V0: in.V0,
		V1: make(map[string]dbus.Variant, len(in.V1)),
	}
	for k, v := range in.V1 {
		out.V1[k] = v
	}
	if depth != 0 {
		depth--
		out.V2 = make([]dbus.Variant, len(in.V2))
		for i, v := range in.V2 {
			out.V2[i] = dbus.MakeVariant(copyLayout(v.Value().(*menuLayout), depth))
		}
	} else {
		out.V2 = []dbus.Variant{}
	}
	return &out
}

// GetLayout is com.canonical.dbusmenu.GetLayout method.
func (t *tray) GetLayout(parentID int32, recursionDepth int32, propertyNames []string) (revision uint32, layout menuLayout, err *dbus.Error) {
	instance.menuLock.RLock()
	defer instance.menuLock.RUnlock()
	if m, ok := findLayout(parentID); ok {
		// Always hand back the complete subtree. A client that asks for a
		// shallow layout must otherwise issue a follow-up GetLayout for
		// every submenu, and any follow-up that is lost or superseded by a
		// later LayoutUpdated leaves that submenu rendered as an empty
		// rectangle. Returning more than was asked for is harmless; a tray
		// menu is tiny. recursionDepth 0 is still honoured literally, since
		// that is the one case where the caller explicitly wants no
		// children at all.
		if recursionDepth > 0 {
			recursionDepth = -1
		}
		rev := instance.menuVersion.Load()
		if recursionDepth != 0 {
			// This reply carries the whole subtree, so the client is now
			// up to date. AboutToShow reads this to decide whether a
			// re-read is actually needed.
			instance.lastFetched.Store(rev)
		}
		// return copy of menu layout to prevent panic from cuncurrent access to layout
		return rev, *copyLayout(m, recursionDepth), nil
	}
	// Reporting success with an empty layout tells the client the item exists
	// and has no children, which is not what happened.
	return instance.menuVersion.Load(), menuLayout{V1: map[string]dbus.Variant{}, V2: []dbus.Variant{}},
		dbus.NewError("com.canonical.dbusmenu.Error.InvalidId",
			[]interface{}{fmt.Sprintf("no menu item with id %d", parentID)})
}

// GetGroupProperties is com.canonical.dbusmenu.GetGroupProperties method.
func (t *tray) GetGroupProperties(ids []int32, propertyNames []string) (properties []struct {
	V0 int32
	V1 map[string]dbus.Variant
}, err *dbus.Error) {
	instance.menuLock.Lock()
	defer instance.menuLock.Unlock()
	for _, id := range ids {
		if m, ok := findLayout(id); ok {
			p := struct {
				V0 int32
				V1 map[string]dbus.Variant
			}{
				V0: m.V0,
				V1: make(map[string]dbus.Variant, len(m.V1)),
			}
			for k, v := range m.V1 {
				p.V1[k] = v
			}
			properties = append(properties, p)
		}
	}
	return
}

// GetProperty is com.canonical.dbusmenu.GetProperty method.
func (t *tray) GetProperty(id int32, name string) (value dbus.Variant, err *dbus.Error) {
	instance.menuLock.Lock()
	defer instance.menuLock.Unlock()
	if m, ok := findLayout(id); ok {
		if p, ok := m.V1[name]; ok {
			return p, nil
		}
	}
	return
}

// Event is com.canonical.dbusmenu.Event method.
func (t *tray) Event(id int32, eventID string, data dbus.Variant, timestamp uint32) (err *dbus.Error) {
	switch eventID {
	case "clicked":
		systrayMenuItemSelected(uint32(id))
	case "opened":
		t.menuLock.RLock()
		rootMenuID := t.menu.V0
		t.menuLock.RUnlock()

		if id == rootMenuID {
			select {
			case TrayOpenedCh <- struct{}{}:
			default:
			}
		}
	case "closed":
		t.menuLock.RLock()
		rootMenuID := t.menu.V0
		t.menuLock.RUnlock()

		if id == rootMenuID {
			select {
			case TrayClosedCh <- struct{}{}:
			default:
			}
		}
	}
	return
}

// EventGroup is com.canonical.dbusmenu.EventGroup method.
func (t *tray) EventGroup(events []struct {
	V0 int32
	V1 string
	V2 dbus.Variant
	V3 uint32
}) (idErrors []int32, err *dbus.Error) {
	for _, event := range events {
		if event.V1 == "clicked" {
			systrayMenuItemSelected(uint32(event.V0))
		}
	}
	return
}

// AboutToShow is com.canonical.dbusmenu.AboutToShow method.
//
// It answers "has this subtree changed since you last fetched it?", NOT "does
// this item have children". Returning true unconditionally makes the client
// re-read and rebuild the submenu's items on every single open; rebuilding
// them while the popup is being mapped destroys the widgets mid-flight, so the
// submenu flickers and collapses instead of opening.
func (t *tray) AboutToShow(id int32) (needUpdate bool, err *dbus.Error) {
	instance.menuLock.RLock()
	defer instance.menuLock.RUnlock()
	return layoutStaleFor(id), nil
}

// AboutToShowGroup is com.canonical.dbusmenu.AboutToShowGroup method.
func (t *tray) AboutToShowGroup(ids []int32) (updatesNeeded []int32, idErrors []int32, err *dbus.Error) {
	instance.menuLock.RLock()
	defer instance.menuLock.RUnlock()
	for _, id := range ids {
		if _, ok := findLayout(id); !ok {
			idErrors = append(idErrors, id)
			continue
		}
		if layoutStaleFor(id) {
			updatesNeeded = append(updatesNeeded, id)
		}
	}
	return
}

// layoutStaleFor reports whether the client's last fetch predates the current
// layout revision. Callers must hold menuLock.
func layoutStaleFor(id int32) bool {
	if _, ok := findLayout(id); !ok {
		return false
	}
	return instance.menuVersion.Load() > instance.lastFetched.Load()
}

func createMenuPropSpec() map[string]map[string]*prop.Prop {
	instance.menuLock.Lock()
	defer instance.menuLock.Unlock()
	return map[string]map[string]*prop.Prop{
		"com.canonical.dbusmenu": {
			// The dbusmenu PROTOCOL version, not the layout revision. It is
			// declared read-only in the spec and must never change: a client
			// uses it to decide which protocol features the server supports,
			// and advertising 0 makes it fall back to a legacy code path.
			// The layout revision travels in LayoutUpdated and GetLayout.
			"Version": {
				Value:    uint32(3),
				Writable: false,
				Emit:     prop.EmitConst,
				Callback: nil,
			},
			"TextDirection": {
				Value:    "ltr",
				Writable: false,
				Emit:     prop.EmitTrue,
				Callback: nil,
			},
			"Status": {
				Value:    "normal",
				Writable: false,
				Emit:     prop.EmitTrue,
				Callback: nil,
			},
			"IconThemePath": {
				Value:    []string{},
				Writable: false,
				Emit:     prop.EmitTrue,
				Callback: nil,
			},
		},
	}
}

// menuLayout is a named struct to map into generated bindings. It represents the layout of a menu item
type menuLayout = struct {
	V0 int32                   // the unique ID of this item
	V1 map[string]dbus.Variant // properties for this menu item layout
	V2 []dbus.Variant          // child menu item layouts
}

func addOrUpdateMenuItem(item *MenuItem) {
	var layout *menuLayout
	instance.menuLock.Lock()
	defer instance.menuLock.Unlock()
	m, exists := findLayout(int32(item.id))
	if exists {
		layout = m
	} else {
		layout = &menuLayout{
			V0: int32(item.id),
			V1: map[string]dbus.Variant{},
			V2: []dbus.Variant{},
		}

		parent := instance.menu
		if item.parent != nil {
			m, ok := findLayout(int32(item.parent.id))
			if ok {
				parent = m
				parent.V1["children-display"] = dbus.MakeVariant("submenu")
			}
		}
		parent.V2 = append(parent.V2, dbus.MakeVariant(layout))
	}

	applyItemToLayout(item, layout)
	// Both additions and updates change the layout, so both must bump the
	// revision and notify. Skipping the notification for additions leaves
	// the client serving a stale tree under an unchanged revision.
	refresh()
}

func addSeparator(id uint32, parent uint32) {
	instance.menuLock.Lock()
	defer instance.menuLock.Unlock()

	m, ok := findLayout(int32(parent))
	if !ok {
		return
	}
	layout := &menuLayout{
		V0: int32(id),
		V1: map[string]dbus.Variant{
			"type": dbus.MakeVariant("separator"),
		},
		V2: []dbus.Variant{},
	}
	// A separator is a child like any other: without children-display the
	// parent of a separator-only submenu is not rendered as a submenu.
	if parent != 0 {
		m.V1["children-display"] = dbus.MakeVariant("submenu")
	}
	m.V2 = append(m.V2, dbus.MakeVariant(layout))
	refresh()
}

func changeSeparatorVisibility(id uint32, visible bool) {
	instance.menuLock.Lock()
	defer instance.menuLock.Unlock()
	item, exist := findLayout(int32(id))
	if !exist {
		return
	}
	if item.V0 == int32(id) {
		item.V1["visible"] = dbus.MakeVariant(visible)
		refresh()
		return
	}
}

func applyItemToLayout(in *MenuItem, out *menuLayout) {
	out.V1["enabled"] = dbus.MakeVariant(!in.disabled)
	out.V1["label"] = dbus.MakeVariant(in.title)

	if in.isCheckable {
		out.V1["toggle-type"] = dbus.MakeVariant("checkmark")
		if in.checked {
			out.V1["toggle-state"] = dbus.MakeVariant(1)
		} else {
			out.V1["toggle-state"] = dbus.MakeVariant(0)
		}
	} else {
		out.V1["toggle-type"] = dbus.MakeVariant("")
		out.V1["toggle-state"] = dbus.MakeVariant(0)
	}
}

func findLayout(id int32) (*menuLayout, bool) {
	if id == 0 {
		return instance.menu, true
	}
	return findSubLayout(id, instance.menu.V2)
}

func findSubLayout(id int32, vals []dbus.Variant) (*menuLayout, bool) {
	for _, i := range vals {
		item := i.Value().(*menuLayout)
		if item.V0 == id {
			return item, true
		}

		if len(item.V2) > 0 {
			child, ok := findSubLayout(id, item.V2)
			if ok {
				return child, true
			}
		}
	}

	return nil, false
}

func removeSubLayout(id int32, vals []dbus.Variant) ([]dbus.Variant, bool) {
	for idx, i := range vals {
		item := i.Value().(*menuLayout)
		if item.V0 == id {
			// vals[:idx:idx] caps the slice so the append allocates rather
			// than shifting elements inside the shared backing array, which
			// is visible to any other slice header still pointing at it.
			return append(vals[:idx:idx], vals[idx+1:]...), true
		}

		if len(item.V2) > 0 {
			// The recursive call returns the CHILD's new slice. It has to be
			// stored on the child; returning it to our own caller would make
			// them install a grandchild slice as their own children.
			if newChildren, removed := removeSubLayout(id, item.V2); removed {
				item.V2 = newChildren
				if len(item.V2) == 0 {
					delete(item.V1, "children-display")
				}
				return vals, true
			}
		}
	}

	return vals, false
}

func removeMenuItem(item *MenuItem) {
	instance.menuLock.Lock()
	defer instance.menuLock.Unlock()

	parent := instance.menu
	if item.parent != nil {
		m, ok := findLayout(int32(item.parent.id))
		if !ok {
			return
		}
		parent = m
	}

	if items, removed := removeSubLayout(int32(item.id), parent.V2); removed {
		parent.V2 = items
		// A parent still advertising children-display after losing its last
		// child renders as a submenu with nothing in it. delete on a nil map
		// is a no-op, so the root item needs no special case.
		if len(parent.V2) == 0 {
			delete(parent.V1, "children-display")
		}
		refresh()
	}
}

func removeSeparator(id uint32) {
	instance.menuLock.Lock()
	defer instance.menuLock.Unlock()

	if items, removed := removeSubLayout(int32(id), instance.menu.V2); removed {
		instance.menu.V2 = items
		refresh()
	}
}

func hideMenuItem(item *MenuItem) {
	instance.menuLock.Lock()
	defer instance.menuLock.Unlock()
	m, exists := findLayout(int32(item.id))
	if exists {
		m.V1["visible"] = dbus.MakeVariant(false)
		refresh()
	}
}

func showMenuItem(item *MenuItem) {
	instance.menuLock.Lock()
	defer instance.menuLock.Unlock()
	m, exists := findLayout(int32(item.id))
	if exists {
		m.V1["visible"] = dbus.MakeVariant(true)
		refresh()
	}
}

func refresh() {
	// The revision must advance whenever the layout changes, even if there is
	// no connection yet to announce it on. Otherwise a menu built during
	// onReady stays at revision 0 and every later staleness check is wrong.
	instance.menuVersion.Add(1)

	instance.lock.Lock()
	if instance.conn == nil || instance.menuProps == nil {
		instance.lock.Unlock()
		return
	}
	instance.lock.Unlock()
	err := menu.Emit(instance.conn, &menu.Dbusmenu_LayoutUpdatedSignal{
		Path: menuPath,
		Body: &menu.Dbusmenu_LayoutUpdatedSignalBody{
			Revision: instance.menuVersion.Load(),
		},
	})
	if err != nil {
		log.Printf("systray error: failed to emit layout updated signal: %v\n", err)
	}

}

func resetMenu() {
	instance.menuLock.Lock()
	defer instance.menuLock.Unlock()
	instance.menu = &menuLayout{}
	instance.menuVersion.Add(1)
}
