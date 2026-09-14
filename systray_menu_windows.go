//go:build windows

package systray

import (
	"log"
	"sync"
)

// separatorParents remembers which menu a separator was added to. The Separator
// methods identify a separator by id alone, but every Win32 menu call needs the
// owning menu handle, which is keyed by parent id.
var separatorParents = struct {
	sync.RWMutex
	m map[uint32]uint32
}{m: make(map[uint32]uint32)}

func trackSeparator(id, parent uint32) {
	separatorParents.Lock()
	separatorParents.m[id] = parent
	separatorParents.Unlock()
}

func separatorParent(id uint32) (uint32, bool) {
	separatorParents.RLock()
	parent, ok := separatorParents.m[id]
	separatorParents.RUnlock()
	return parent, ok
}

// changeSeparatorVisibility shows or hides a separator. Windows has no hidden
// state for a menu entry, so hiding removes it and showing re-inserts it at the
// position its id implies.
func changeSeparatorVisibility(id uint32, visible bool) {
	parent, ok := separatorParent(id)
	if !ok {
		return
	}

	var err error
	if visible {
		err = wt.addSeparatorMenuItem(id, parent)
	} else {
		err = wt.hideMenuItem(id, parent)
	}
	if err != nil {
		log.Printf("systray error: unable to change separator visibility: %s\n", err)
	}
}

// removeSeparator deletes a separator from the menu for good.
func removeSeparator(id uint32) {
	parent, ok := separatorParent(id)
	if !ok {
		return
	}
	if err := wt.removeMenuItem(id, parent); err != nil {
		log.Printf("systray error: unable to removeSeparator: %s\n", err)
		return
	}
	separatorParents.Lock()
	delete(separatorParents.m, id)
	separatorParents.Unlock()
}

// refresh is a no-op on Windows. The Win32 menu is mutated in place, so there is
// no batched layout to push to the system the way the DBus backend must.
func refresh() {}

// addOrUpdateMenuItemQuiet is equivalent to addOrUpdateMenuItem on Windows. The
// "quiet" variant exists so the DBus backend can skip its LayoutUpdated signal;
// Win32 has no such signal to suppress.
func addOrUpdateMenuItemQuiet(item *MenuItem) {
	addOrUpdateMenuItem(item)
}
