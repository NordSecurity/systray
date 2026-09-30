//go:build (linux || freebsd || openbsd || netbsd) && !android

package systray

import (
	"github.com/NordSecurity/systray/internal/generated/menu"
	"github.com/godbus/dbus/v5"
)

const dbusmenuInterface = "com.canonical.dbusmenu"

// orderedMenuCallsQueueSize bounds the menu calls waiting for the ordered worker.
const orderedMenuCallsQueueSize = 256

// newOrderedMenuCallsInterceptor returns an interceptor that takes over replying to dbusmenu read calls,
// queueing them to be served by serveOrderedMenuCalls in the order they arrived.
// While the queue is full, it blocks the connection's read loop.
func newOrderedMenuCallsInterceptor(calls chan<- *dbus.Message) dbus.Interceptor {
	return func(msg *dbus.Message) {
		if !isOrderedMenuCall(msg) {
			return
		}
		msg.Flags |= dbus.FlagNoReplyExpected
		calls <- msg
	}
}

func isOrderedMenuCall(msg *dbus.Message) bool {
	if msg.Type != dbus.TypeMethodCall || msg.Flags&dbus.FlagNoReplyExpected != 0 {
		return false
	}
	path, _ := msg.Headers[dbus.FieldPath].Value().(dbus.ObjectPath)
	iface, _ := msg.Headers[dbus.FieldInterface].Value().(string)
	if path != menuPath || iface != dbusmenuInterface {
		return false
	}
	member, _ := msg.Headers[dbus.FieldMember].Value().(string)
	switch member {
	case "GetLayout", "GetGroupProperties", "GetProperty":
		return true
	}
	return false
}

// orderedMenuExport is exported instead of the menu itself, so the calls answered by
// serveOrderedMenuCalls are not executed a second time by godbus's own dispatch.
type orderedMenuExport struct{ menu.Dbusmenuer }

func (orderedMenuExport) GetLayout(int32, int32, []string) (uint32, menuLayout, *dbus.Error) {
	return 0, menuLayout{}, nil
}

func (orderedMenuExport) GetGroupProperties([]int32, []string) ([]struct {
	V0 int32
	V1 map[string]dbus.Variant
}, *dbus.Error) {
	return nil, nil
}

func (orderedMenuExport) GetProperty(int32, string) (dbus.Variant, *dbus.Error) {
	return dbus.Variant{}, nil
}

// serveOrderedMenuCalls replies to the queued menu calls one at a time until quit.
func serveOrderedMenuCalls(conn *dbus.Conn, calls <-chan *dbus.Message, target menu.Dbusmenuer) {
	for {
		select {
		case msg := <-calls:
			body, dbusErr := callMenuMethod(target, msg)
			sendMenuReply(conn, msg, body, dbusErr)
		case <-quitChan:
			return
		}
	}
}

func callMenuMethod(target menu.Dbusmenuer, msg *dbus.Message) ([]interface{}, *dbus.Error) {
	member, _ := msg.Headers[dbus.FieldMember].Value().(string)
	switch member {
	case "GetLayout":
		var parentID, recursionDepth int32
		var propertyNames []string
		if err := dbus.Store(msg.Body, &parentID, &recursionDepth, &propertyNames); err != nil {
			return nil, &dbus.ErrMsgInvalidArg
		}
		revision, layout, dbusErr := target.GetLayout(parentID, recursionDepth, propertyNames)
		return []interface{}{revision, layout}, dbusErr
	case "GetGroupProperties":
		var ids []int32
		var propertyNames []string
		if err := dbus.Store(msg.Body, &ids, &propertyNames); err != nil {
			return nil, &dbus.ErrMsgInvalidArg
		}
		properties, dbusErr := target.GetGroupProperties(ids, propertyNames)
		return []interface{}{properties}, dbusErr
	case "GetProperty":
		var id int32
		var name string
		if err := dbus.Store(msg.Body, &id, &name); err != nil {
			return nil, &dbus.ErrMsgInvalidArg
		}
		value, dbusErr := target.GetProperty(id, name)
		return []interface{}{value}, dbusErr
	}
	return nil, dbus.NewError("org.freedesktop.DBus.Error.UnknownMethod", []interface{}{"Unknown / invalid method"})
}

func sendMenuReply(conn *dbus.Conn, call *dbus.Message, body []interface{}, dbusErr *dbus.Error) {
	reply := &dbus.Message{
		Type: dbus.TypeMethodReply,
		Headers: map[dbus.HeaderField]dbus.Variant{
			dbus.FieldReplySerial: dbus.MakeVariant(call.Serial()),
		},
		Body: body,
	}
	if sender, ok := call.Headers[dbus.FieldSender]; ok {
		reply.Headers[dbus.FieldDestination] = sender
	}
	if dbusErr != nil {
		reply.Type = dbus.TypeError
		reply.Headers[dbus.FieldErrorName] = dbus.MakeVariant(dbusErr.Name)
		reply.Body = dbusErr.Body
	}
	if len(reply.Body) > 0 {
		reply.Headers[dbus.FieldSignature] = dbus.MakeVariant(dbus.SignatureOf(reply.Body...))
	}
	conn.Send(reply, nil)
}
