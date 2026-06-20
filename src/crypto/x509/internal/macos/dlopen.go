// Local patch: provide dlopen/dlsym wrappers so the Go toolchain binary
// does not require SecTrustCopyCertificateChain (macOS 12+) at load time.
// Loaded on demand from /usr/lib/libSystem.B.dylib at the time the
// symbol is first requested.

//go:build darwin

package macos

import (
	"internal/abi"
	"unsafe"
)

// cgo_import_dynamic links against the symbols in libSystem.B.dylib. These
// two functions have been part of libSystem since 10.4, so the dyld look-up
// during binary load is satisfied on macOS 10.15+. Their resolution
// happens lazily and is handled by the standard cgo machinery.
//
//go:cgo_import_dynamic x509_dlopen dlopen "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic x509_dlsym dlsym "/usr/lib/libSystem.B.dylib"

func x509_dlopen_trampoline()
func x509_dlsym_trampoline()

// cstring converts a Go string to a NUL-terminated C string buffer. The
// returned slice is intended to be referenced only for the duration of a
// single call; the caller must not retain it across calls.
func cstring(s string) *byte {
	if s == "" {
		return nil
	}
	b := make([]byte, len(s)+1)
	copy(b, s)
	return &b[0]
}

const (
	rtldNow    = 0x00000002
	rtldGlobal = 0x00000008
)

// dlopenGo opens a shared library. Pass the empty string to obtain a
// handle on the main program. Returns nil on failure.
func dlopenGo(path string) unsafe.Pointer {
	cpath := cstring(path)
	ret := syscall(
		abi.FuncPCABI0(x509_dlopen_trampoline),
		uintptr(unsafe.Pointer(cpath)),
		uintptr(rtldNow|rtldGlobal),
		0, 0, 0, 0,
	)
	return unsafe.Pointer(ret)
}

// dlsymGo looks up a symbol in a handle returned by dlopenGo. Returns nil
// if the symbol cannot be found.
func dlsymGo(handle unsafe.Pointer, name string) unsafe.Pointer {
	if handle == nil {
		return nil
	}
	cname := cstring(name)
	ret := syscall(
		abi.FuncPCABI0(x509_dlsym_trampoline),
		uintptr(handle),
		uintptr(unsafe.Pointer(cname)),
		0, 0, 0, 0,
	)
	return unsafe.Pointer(ret)
}
