//go:build darwin

package grants

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// The system frameworks the C checks live in. purego loads them with dlopen, so no cgo is
// needed (Q4). Library validation allows Apple-signed system code under the hardened runtime.
const (
	appServicesPath    = "/System/Library/Frameworks/ApplicationServices.framework/ApplicationServices"
	coreGraphicsPath   = "/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics"
	coreServicesPath   = "/System/Library/Frameworks/CoreServices.framework/CoreServices"
	coreFoundationPath = "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation"
)

// Four-character codes from AEDataModel.h.
const (
	typeApplicationBundleID = 0x62756e64 // 'bund'
	typeWildCard            = 0x2a2a2a2a // '****'
)

type darwinFuncs struct {
	axTrusted            func() bool
	axTrustedWithOptions func(opts uintptr) bool
	axPromptKey          uintptr // the CFStringRef kAXTrustedCheckOptionPrompt
	cgPreflight          func() bool
	cgRequest            func() bool
	aeCreateDesc         func(typ uint32, data unsafe.Pointer, size int, desc unsafe.Pointer) int16
	aeDisposeDesc        func(desc unsafe.Pointer) int16
	aeDetermine          func(target unsafe.Pointer, class, id uint32, ask bool) int32
	cfDictionaryCreate   func(alloc uintptr, keys, values unsafe.Pointer, n int, kcb, vcb uintptr) uintptr
	cfRelease            func(ref uintptr)
	cfTrue               uintptr // kCFBooleanTrue
	cfKeyCallBacks       uintptr // &kCFTypeDictionaryKeyCallBacks
	cfValueCallBacks     uintptr // &kCFTypeDictionaryValueCallBacks
}

var (
	loadOnce sync.Once
	funcs    darwinFuncs
	loadErr  error
)

// deref reads the pointer-sized value at the address addr, for a C global that holds a
// pointer (a CFStringRef constant).
func deref(addr uintptr) uintptr { return **(**uintptr)(unsafe.Pointer(&addr)) }

func load() error {
	loadOnce.Do(func() {
		open := func(path string) uintptr {
			if loadErr != nil {
				return 0
			}
			h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
			if err != nil {
				loadErr = fmt.Errorf("dlopen %s: %w", path, err)
			}
			return h
		}
		sym := func(lib uintptr, name string) uintptr {
			if loadErr != nil {
				return 0
			}
			p, err := purego.Dlsym(lib, name)
			if err != nil {
				loadErr = fmt.Errorf("dlsym %s: %w", name, err)
			}
			return p
		}
		reg := func(fptr any, lib uintptr, name string) {
			if p := sym(lib, name); loadErr == nil {
				purego.RegisterFunc(fptr, p)
			}
		}
		as, cg, cs, cf := open(appServicesPath), open(coreGraphicsPath), open(coreServicesPath), open(coreFoundationPath)
		reg(&funcs.axTrusted, as, "AXIsProcessTrusted")
		reg(&funcs.axTrustedWithOptions, as, "AXIsProcessTrustedWithOptions")
		if p := sym(as, "kAXTrustedCheckOptionPrompt"); loadErr == nil {
			funcs.axPromptKey = deref(p)
		}
		reg(&funcs.cgPreflight, cg, "CGPreflightScreenCaptureAccess")
		reg(&funcs.cgRequest, cg, "CGRequestScreenCaptureAccess")
		reg(&funcs.aeCreateDesc, cs, "AECreateDesc")
		reg(&funcs.aeDisposeDesc, cs, "AEDisposeDesc")
		reg(&funcs.aeDetermine, cs, "AEDeterminePermissionToAutomateTarget")
		reg(&funcs.cfDictionaryCreate, cf, "CFDictionaryCreate")
		reg(&funcs.cfRelease, cf, "CFRelease")
		if p := sym(cf, "kCFBooleanTrue"); loadErr == nil {
			funcs.cfTrue = deref(p)
		}
		funcs.cfKeyCallBacks = sym(cf, "kCFTypeDictionaryKeyCallBacks")
		funcs.cfValueCallBacks = sym(cf, "kCFTypeDictionaryValueCallBacks")
	})
	return loadErr
}

type darwinSystem struct{ home string }

// NewSystem returns the System of this Mac.
func NewSystem() System {
	home, _ := os.UserHomeDir()
	return darwinSystem{home: home}
}

func (s darwinSystem) Home() string { return s.home }

func (darwinSystem) ManagerName(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "/bin/launchctl", "managername").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (darwinSystem) OpenDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (darwinSystem) AXTrusted() (bool, error) {
	if err := load(); err != nil {
		return false, err
	}
	return funcs.axTrusted(), nil
}

// AXRequest calls AXIsProcessTrustedWithOptions({kAXTrustedCheckOptionPrompt: true}) once.
func (darwinSystem) AXRequest() error {
	if err := load(); err != nil {
		return err
	}
	keys := [1]uintptr{funcs.axPromptKey}
	vals := [1]uintptr{funcs.cfTrue}
	d := funcs.cfDictionaryCreate(0, unsafe.Pointer(&keys[0]), unsafe.Pointer(&vals[0]), 1, funcs.cfKeyCallBacks, funcs.cfValueCallBacks)
	if d == 0 {
		return errors.New("CFDictionaryCreate failed")
	}
	defer funcs.cfRelease(d)
	funcs.axTrustedWithOptions(d)
	return nil
}

func (darwinSystem) ScreenPreflight() (bool, error) {
	if err := load(); err != nil {
		return false, err
	}
	return funcs.cgPreflight(), nil
}

func (darwinSystem) ScreenRequest() error {
	if err := load(); err != nil {
		return err
	}
	funcs.cgRequest()
	return nil
}

// Automation asks AEDeterminePermissionToAutomateTarget about target with askUserIfNeeded
// false, so it never shows a dialog. The AEDesc is an opaque buffer: only the framework reads
// its fields, so its packing does not matter here.
func (darwinSystem) Automation(target string) (int32, error) {
	if err := load(); err != nil {
		return 0, err
	}
	var desc [32]byte
	id := []byte(target)
	if e := funcs.aeCreateDesc(typeApplicationBundleID, unsafe.Pointer(&id[0]), len(id), unsafe.Pointer(&desc[0])); e != 0 {
		return 0, fmt.Errorf("AECreateDesc: OSErr %d", e)
	}
	st := funcs.aeDetermine(unsafe.Pointer(&desc[0]), typeWildCard, typeWildCard, false)
	funcs.aeDisposeDesc(unsafe.Pointer(&desc[0]))
	runtime.KeepAlive(id)
	return st, nil
}

// Gateway reads the default route's gateway from `route -n get default`.
func (darwinSystem) Gateway(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "/sbin/route", "-n", "get", "default").Output()
	if err != nil {
		return "", err
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "gateway:"); ok {
			v = strings.TrimSpace(v)
			if net.ParseIP(v) == nil {
				return "", fmt.Errorf("gateway %q is not an address", v)
			}
			return v, nil
		}
	}
	return "", errors.New("route printed no gateway")
}

func (darwinSystem) SendUDP(ctx context.Context, addr string) error { return SendUDP(ctx, addr) }

func (darwinSystem) Clipboard(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "/usr/bin/pbpaste").Output()
	return string(out), err
}

func (darwinSystem) SetClipboard(ctx context.Context, s string) error {
	cmd := exec.CommandContext(ctx, "/usr/bin/pbcopy")
	cmd.Stdin = strings.NewReader(s)
	return cmd.Run()
}
