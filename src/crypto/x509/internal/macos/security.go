// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin

package macos

import (
	"errors"
	"internal/abi"
	"strconv"
	"sync"
	"unsafe"
)

// Security.framework linker flags for the external linker. See Issue 42459.
//
//go:cgo_ldflag "-framework"
//go:cgo_ldflag "Security"

// Based on https://opensource.apple.com/source/Security/Security-59306.41.2/base/Security.h

const (
	// various macOS error codes that can be returned from
	// SecTrustEvaluateWithError that we can map to Go cert
	// verification error types.
	ErrSecCertificateExpired = -67818
	ErrSecHostNameMismatch   = -67602
	ErrSecNotTrusted         = -67843
)

type OSStatus struct {
	call   string
	status int32
}

func (s OSStatus) Error() string {
	return s.call + " error: " + strconv.Itoa(int(s.status))
}

//go:cgo_import_dynamic x509_SecTrustCreateWithCertificates SecTrustCreateWithCertificates "/System/Library/Frameworks/Security.framework/Versions/A/Security"

func SecTrustCreateWithCertificates(certs CFRef, policies CFRef) (CFRef, error) {
	var trustObj CFRef
	ret := syscall(abi.FuncPCABI0(x509_SecTrustCreateWithCertificates_trampoline), uintptr(certs), uintptr(policies),
		uintptr(unsafe.Pointer(&trustObj)), 0, 0, 0)
	if int32(ret) != 0 {
		return 0, OSStatus{"SecTrustCreateWithCertificates", int32(ret)}
	}
	return trustObj, nil
}
func x509_SecTrustCreateWithCertificates_trampoline()

//go:cgo_import_dynamic x509_SecCertificateCreateWithData SecCertificateCreateWithData "/System/Library/Frameworks/Security.framework/Versions/A/Security"

func SecCertificateCreateWithData(b []byte) (CFRef, error) {
	data := BytesToCFData(b)
	defer CFRelease(data)
	ret := syscall(abi.FuncPCABI0(x509_SecCertificateCreateWithData_trampoline), kCFAllocatorDefault, uintptr(data), 0, 0, 0, 0)
	// Returns NULL if the data passed in the data parameter is not a valid
	// DER-encoded X.509 certificate.
	if ret == 0 {
		return 0, errors.New("SecCertificateCreateWithData: invalid certificate")
	}
	return CFRef(ret), nil
}
func x509_SecCertificateCreateWithData_trampoline()

//go:cgo_import_dynamic x509_SecPolicyCreateSSL SecPolicyCreateSSL "/System/Library/Frameworks/Security.framework/Versions/A/Security"

func SecPolicyCreateSSL(name string) (CFRef, error) {
	var hostname CFString
	if name != "" {
		hostname = StringToCFString(name)
		defer CFRelease(CFRef(hostname))
	}
	ret := syscall(abi.FuncPCABI0(x509_SecPolicyCreateSSL_trampoline), 1 /* true */, uintptr(hostname), 0, 0, 0, 0)
	if ret == 0 {
		return 0, OSStatus{"SecPolicyCreateSSL", int32(ret)}
	}
	return CFRef(ret), nil
}
func x509_SecPolicyCreateSSL_trampoline()

//go:cgo_import_dynamic x509_SecTrustSetVerifyDate SecTrustSetVerifyDate "/System/Library/Frameworks/Security.framework/Versions/A/Security"

func SecTrustSetVerifyDate(trustObj CFRef, dateRef CFRef) error {
	ret := syscall(abi.FuncPCABI0(x509_SecTrustSetVerifyDate_trampoline), uintptr(trustObj), uintptr(dateRef), 0, 0, 0, 0)
	if int32(ret) != 0 {
		return OSStatus{"SecTrustSetVerifyDate", int32(ret)}
	}
	return nil
}
func x509_SecTrustSetVerifyDate_trampoline()

//go:cgo_import_dynamic x509_SecTrustEvaluate SecTrustEvaluate "/System/Library/Frameworks/Security.framework/Versions/A/Security"

func SecTrustEvaluate(trustObj CFRef) (CFRef, error) {
	var result CFRef
	ret := syscall(abi.FuncPCABI0(x509_SecTrustEvaluate_trampoline), uintptr(trustObj), uintptr(unsafe.Pointer(&result)), 0, 0, 0, 0)
	if int32(ret) != 0 {
		return 0, OSStatus{"SecTrustEvaluate", int32(ret)}
	}
	return CFRef(result), nil
}
func x509_SecTrustEvaluate_trampoline()

//go:cgo_import_dynamic x509_SecTrustEvaluateWithError SecTrustEvaluateWithError "/System/Library/Frameworks/Security.framework/Versions/A/Security"

func SecTrustEvaluateWithError(trustObj CFRef) (int, error) {
	var errRef CFRef
	ret := syscall(abi.FuncPCABI0(x509_SecTrustEvaluateWithError_trampoline), uintptr(trustObj), uintptr(unsafe.Pointer(&errRef)), 0, 0, 0, 0)
	if int32(ret) != 1 {
		errStr := CFErrorCopyDescription(errRef)
		err := errors.New(CFStringToString(errStr))
		errCode := CFErrorGetCode(errRef)
		CFRelease(errRef)
		CFRelease(errStr)
		return errCode, err
	}
	return 0, nil
}
func x509_SecTrustEvaluateWithError_trampoline()

//go:cgo_import_dynamic x509_SecCertificateCopyData SecCertificateCopyData "/System/Library/Frameworks/Security.framework/Versions/A/Security"

func SecCertificateCopyData(cert CFRef) ([]byte, error) {
	ret := syscall(abi.FuncPCABI0(x509_SecCertificateCopyData_trampoline), uintptr(cert), 0, 0, 0, 0, 0)
	if ret == 0 {
		return nil, errors.New("x509: invalid certificate object")
	}
	b := CFDataToSlice(CFRef(ret))
	CFRelease(CFRef(ret))
	return b, nil
}
func x509_SecCertificateCopyData_trampoline()

// Local patch: SecTrustCopyCertificateChain is a macOS 12+ API. We
// resolve it at runtime via dlopen/dlsym (declared in dlopen.go) so the
// Go toolchain binary does not require the symbol at load time and can
// still run on macOS Catalina (10.15). On older macOS versions, dlsym
// returns NULL and we surface an error; the caller in root_darwin.go
// handles that by treating the chain as empty.
var (
	secTrustCopyChainOnce sync.Once
	secTrustCopyChainFn   uintptr // function pointer to SecTrustCopyCertificateChain
	secTrustCopyChainErr  error
)

func loadSecTrustCopyCertificateChain() (uintptr, error) {
	secTrustCopyChainOnce.Do(func() {
		h := dlopenGo("/System/Library/Frameworks/Security.framework/Security")
		if h == nil {
			secTrustCopyChainErr = errors.New("dlopen Security.framework failed")
			return
		}
		p := dlsymGo(h, "SecTrustCopyCertificateChain")
		if p == nil {
			secTrustCopyChainErr = errors.New("SecTrustCopyCertificateChain is not available on this macOS version (requires 12+)")
			return
		}
		secTrustCopyChainFn = uintptr(p)
	})
	return secTrustCopyChainFn, secTrustCopyChainErr
}

func SecTrustCopyCertificateChain(trustObj CFRef) (CFRef, error) {
	fn, err := loadSecTrustCopyCertificateChain()
	if err != nil {
		return 0, err
	}
	// Call the dynamically resolved C function via the runtime syscall
	// helper. The C signature is CFArrayRef SecTrustCopyCertificateChain(
	// SecTrustRef trust). The Go runtime helper takes up to 5 args; the
	// extra args are zero and the floating-arg slot is unused.
	ret := syscall(fn, uintptr(trustObj), 0, 0, 0, 0, 0)
	if ret == 0 {
		return 0, OSStatus{"SecTrustCopyCertificateChain", -1}
	}
	return CFRef(ret), nil
}
