//go:build (linux || freebsd || openbsd || netbsd) && !android

package systray

import (
	"bufio"
	"log"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/NordSecurity/systray/internal/generated/menu"
)

const (
	orderTestBigSubmenuSize = 2000
	orderTestIterations     = 50
)

var orderTestBusAddress string

func TestMain(m *testing.M) {
	os.Exit(runWithPrivateSessionBus(m))
}

func runWithPrivateSessionBus(m *testing.M) int {
	daemon, err := exec.LookPath("dbus-daemon")
	if err != nil {
		return m.Run()
	}
	cmd := exec.Command(daemon, "--session", "--nofork", "--print-address=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("stdout pipe: %v", err)
		return 1
	}
	if err := cmd.Start(); err != nil {
		log.Printf("start dbus-daemon: %v", err)
		return 1
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	address, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		log.Printf("read dbus-daemon address: %v", err)
		return 1
	}
	orderTestBusAddress = strings.TrimSpace(address)
	os.Setenv("DBUS_SESSION_BUS_ADDRESS", orderTestBusAddress)

	start, end := RunWithExternalLoop(nil, nil)
	start()
	defer end()

	return m.Run()
}

func TestMenuReadCallsAreRepliedInRequestOrder(t *testing.T) {
	if orderTestBusAddress == "" {
		t.Skip("dbus-daemon not available")
	}
	if instance.conn == nil {
		t.Fatal("systray did not connect to the session bus")
	}

	ResetMenu()
	big := AddMenuItem("big", "big")
	bigIDs := make([]int32, 0, orderTestBigSubmenuSize)
	for i := 0; i < orderTestBigSubmenuSize; i++ {
		bigIDs = append(bigIDs, int32(big.AddSubMenuItem("item", "item").id))
	}
	small := AddMenuItem("small", "small")
	smallIDs := []int32{int32(small.AddSubMenuItem("item", "item").id)}

	client, err := dbus.Connect(orderTestBusAddress)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	menuObj := client.Object(instance.conn.Names()[0], menuPath)
	method := dbusmenuInterface + ".GetGroupProperties"

	for i := 0; i < orderTestIterations; i++ {
		done := make(chan *dbus.Call, 2)
		bigCall := menuObj.Go(method, 0, done, bigIDs, []string{})
		smallCall := menuObj.Go(method, 0, done, smallIDs, []string{})

		var replies []*dbus.Call
		for len(replies) < 2 {
			select {
			case call := <-done:
				if call.Err != nil {
					t.Fatalf("iteration %d: GetGroupProperties failed: %v", i, call.Err)
				}
				replies = append(replies, call)
			case <-time.After(5 * time.Second):
				t.Fatalf("iteration %d: timed out waiting for replies", i)
			}
		}

		if replies[0] != bigCall || replies[1] != smallCall {
			t.Fatalf("iteration %d: small batch reply overtook the big batch reply sent before it", i)
		}
	}
}

type countingMenu struct {
	menu.Dbusmenuer
	invoked chan string
}

func (m *countingMenu) GetLayout(int32, int32, []string) (uint32, menuLayout, *dbus.Error) {
	m.invoked <- "GetLayout"
	return 0, menuLayout{}, nil
}

func (m *countingMenu) GetGroupProperties([]int32, []string) ([]struct {
	V0 int32
	V1 map[string]dbus.Variant
}, *dbus.Error) {
	m.invoked <- "GetGroupProperties"
	return nil, nil
}

func (m *countingMenu) GetProperty(int32, string) (dbus.Variant, *dbus.Error) {
	m.invoked <- "GetProperty"
	return dbus.MakeVariant(""), nil
}

func TestMenuReadCallsAreExecutedOnce(t *testing.T) {
	if orderTestBusAddress == "" {
		t.Skip("dbus-daemon not available")
	}

	target := &countingMenu{invoked: make(chan string, 4)}
	calls := make(chan *dbus.Message, orderedMenuCallsQueueSize)
	server, err := dbus.Connect(orderTestBusAddress, dbus.WithIncomingInterceptor(newOrderedMenuCallsInterceptor(calls)))
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	go serveOrderedMenuCalls(server, calls, target)
	if err := menu.ExportDbusmenu(server, menuPath, orderedMenuExport{target}); err != nil {
		t.Fatalf("export menu: %v", err)
	}

	client, err := dbus.Connect(orderTestBusAddress)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	menuObj := client.Object(server.Names()[0], menuPath)

	tests := []struct {
		member string
		args   []interface{}
	}{
		{member: "GetLayout", args: []interface{}{int32(0), int32(-1), []string{}}},
		{member: "GetGroupProperties", args: []interface{}{[]int32{0}, []string{}}},
		{member: "GetProperty", args: []interface{}{int32(0), "label"}},
	}

	for _, test := range tests {
		t.Run(test.member, func(t *testing.T) {
			if err := menuObj.Call(dbusmenuInterface+"."+test.member, 0, test.args...).Err; err != nil {
				t.Fatalf("%s failed: %v", test.member, err)
			}

			select {
			case got := <-target.invoked:
				if got != test.member {
					t.Fatalf("expected %s to be executed, got %s", test.member, got)
				}
			default:
				t.Fatalf("%s was replied to without being executed", test.member)
			}

			select {
			case got := <-target.invoked:
				t.Fatalf("%s was executed more than once", got)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}
