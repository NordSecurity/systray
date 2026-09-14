//go:build !ios

package systray

/*
#include "systray.h"
*/
import "C"

// changeSeparatorVisibility shows or hides a separator. Separators are tagged
// with their own id in add_separator, so they resolve through the same
// find_menu_item lookup as regular menu items.
func changeSeparatorVisibility(id uint32, visible bool) {
	if visible {
		C.show_menu_item(C.int(id))
		return
	}
	C.hide_menu_item(C.int(id))
}

// removeSeparator removes a separator from the menu.
func removeSeparator(id uint32) {
	C.remove_menu_item(C.int(id))
}

// refresh is a no-op on macOS. The AppKit menu is mutated in place, so there is
// no batched layout to push to the system the way the DBus backend must.
func refresh() {}

// addOrUpdateMenuItemQuiet is equivalent to addOrUpdateMenuItem on macOS. The
// "quiet" variant exists so the DBus backend can skip its LayoutUpdated signal;
// AppKit has no such signal to suppress.
func addOrUpdateMenuItemQuiet(item *MenuItem) {
	addOrUpdateMenuItem(item)
}
