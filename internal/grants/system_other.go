//go:build !darwin

package grants

import (
	"context"
	"errors"
	"os"
)

// errNotMac is the answer of every macOS check on another system.
var errNotMac = errors.New("macOS grants: not applicable on this system")

type otherSystem struct{}

// NewSystem returns a System whose macOS checks all fail with errNotMac.
func NewSystem() System { return otherSystem{} }

func (otherSystem) Home() string { h, _ := os.UserHomeDir(); return h }
func (otherSystem) ManagerName(context.Context) (string, error) {
	return "", errNotMac
}
func (otherSystem) OpenDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return f.Close()
}
func (otherSystem) AXTrusted() (bool, error)                   { return false, errNotMac }
func (otherSystem) AXRequest() error                           { return errNotMac }
func (otherSystem) ScreenPreflight() (bool, error)             { return false, errNotMac }
func (otherSystem) ScreenRequest() error                       { return errNotMac }
func (otherSystem) Automation(string) (int32, error)           { return 0, errNotMac }
func (otherSystem) Gateway(context.Context) (string, error)    { return "", errNotMac }
func (otherSystem) SendUDP(context.Context, string) error      { return errNotMac }
func (otherSystem) Clipboard(context.Context) (string, error)  { return "", errNotMac }
func (otherSystem) SetClipboard(context.Context, string) error { return errNotMac }
