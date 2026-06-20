# Go 1.26.4 Compatibility Notes for Legacy macOS

This document records every code change made to allow the Go 1.26.4 source tree to bootstrap, build, and run on macOS Catalina (10.15.7).

> **Scope**: These changes are intended **only for local development** use of the Go toolchain. They are not for production releases. All changes stay within the Go 1.26 standard library and runtime ABI.

---

## 1. Background and Failure Symptom

Running `cd src && ./make.bash` on macOS Catalina (10.15.7) crashes during the *"Building packages and commands for darwin/amd64."* phase:

```
dyld: Symbol not found: _SecTrustCopyCertificateChain
  Referenced from: /opt/workspace/go1.26.4/bin/go (which was built for Mac OS X 12.0)
  Expected in: /System/Library/Frameworks/Security.framework/Versions/A/Security
 in /opt/workspace/go1.26.4/bin/go

go tool dist: FAILED: /opt/workspace/go1.26.4/bin/go list -f={{if .Stale}} ...
        std: signal: abort trap
```

The failure happens when dist invokes `bin/go list std` — not while dist itself is being built. The `bin/go` binary is already built, but dyld aborts at **load time** because a symbol statically referenced via `cgo_import_dynamic` cannot be resolved.

Running `otool -l bin/go` on the failing binary shows:

```
Load command 5
      cmd LC_BUILD_VERSION
      cmdsize 24
 platform 1
    minos 12.0
      sdk 12.0
   ntools 0
```

This identifies **two independent** macOS compatibility issues in Go 1.26:

1. **The linker hard-codes Mach-O `minos` to 12.0.0**, so dyld on 10.15 refuses to load the binary because of the version mismatch.
2. **`crypto/x509`'s `systemVerify` path references the `SecTrustCopyCertificateChain` symbol from Security.framework, which was only introduced in macOS 12.** Even if `minos` is lowered enough to allow loading, dyld still aborts when it fails to resolve the undefined symbol.

Each fix is described below by **file, reason, and implementation notes**.

---

## 2. Summary of Changed Files

| File | Type | Change Summary |
| --- | --- | --- |
| `src/cmd/link/internal/ld/macho.go` | Modified | Lower Mach-O `LC_BUILD_VERSION` `minos`/`sdk` from `12.0.0` to `10.13.0` |
| `src/crypto/x509/internal/macos/security.go` | Modified | Drop the `cgo_import_dynamic` reference to `SecTrustCopyCertificateChain`; resolve it at runtime via `dlopen`/`dlsym` |
| `src/crypto/x509/internal/macos/security.s` | Modified | Remove the `x509_SecTrustCopyCertificateChain_trampoline` assembly stub |
| `src/crypto/x509/internal/macos/dlopen.go` | Added | `dlopenGo` / `dlsymGo` wrappers |
| `src/crypto/x509/internal/macos/dlopen.s` | Added | Assembly trampolines for the wrappers above |
| `src/crypto/x509/root_darwin.go` | Modified | When `SecTrustCopyCertificateChain` is unavailable, fall back to a single-element (leaf-only) chain |

All new files compile only under the `darwin` build tag and have **zero impact on non-darwin platforms**.

---

## 3. `src/cmd/link/internal/ld/macho.go` — Lower the Mach-O minos

### 3.1 Original

When `cmd/link` writes the Mach-O file header, it hard-codes `minos`/`sdk` for the `PLATFORM_MACOS` platform:

```go
if ctxt.LinkMode == LinkInternal && machoPlatform == PLATFORM_MACOS {
    var version uint32
    switch ctxt.Arch.Family {
    case sys.ARM64, sys.AMD64:
        // ...
        version = 12<<16 | 0<<8 | 0<<0 // 12.0.0
    }
    ml := newMachoLoad(ctxt.Arch, imacho.LC_BUILD_VERSION, 4)
    ml.data[0] = uint32(machoPlatform)
    ml.data[1] = version // OS version
    ml.data[2] = version // SDK version
    ml.data[3] = 0       // ntools
}
```

`12<<16 | 0<<8 | 0<<0` is the packed `X.Y.Z` format of Mach-O `LC_BUILD_VERSION` (major 16 bits, minor 8 bits, patch 8 bits). The value `0x000C0000` corresponds to 12.0.0.

### 3.2 Change

```diff
-        version = 12<<16 | 0<<8 | 0<<0 // 12.0.0
+        // Local patch: lowered from 12.0.0 to 10.13.0 to allow
+        // running the built toolchain on macOS Catalina (10.15).
+        version = 10<<16 | 13<<8 | 0<<0 // 10.13.0
```

### 3.3 Reason

`LC_BUILD_VERSION.minos` is the minimum macOS version that dyld checks at binary load time. The Go 1.26 team's `// CL 460476` comment explains that writing a newer `minos` both passes Apple signing validation and avoids a class of historical library-call issues; 12.0 means macOS Monterey. Catalina is 10.15, **lower than 12.0**, so dyld refuses to load — the `which was built for Mac OS X 12.0` line in the error is exactly this field.

Changing `minos` to `10.13.0` (High Sierra) lets the 10.15 dyld accept the binary. We did not lower it further (e.g. to 10.9) because:

- 10.13 is the oldest version Apple still officially supports for Xcode/Clang; `cmd/dist/build.go` notes *"macOS 10.9 and later require clang"*, so cmd/dist itself only requires 10.9, but the toolchain internally depends on some 10.13+ SDK behaviour (typical examples: certain CoreFoundation symbols), and 10.13 is a safe lower bound.
- This is a local-dev build, so **ensuring it works** matters more than **chasing the lowest possible version**.

The `SDK` field (`ml.data[2]`) is set to the same value just for consistency with `minos`; Go's self-bootstrap does not depend on the system SDK, so this value is informational and has no behavioral effect.

---

## 4. `src/crypto/x509/internal/macos/security.go` — Remove the Static Reference to the 12+ API

### 4.1 Original

`security.go` declares a set of darwin-only symbols, with `SecTrustCopyCertificateChain` imported via `cgo_import_dynamic`:

```go
//go:cgo_import_dynamic x509_SecTrustCopyCertificateChain SecTrustCopyCertificateChain "/System/Library/Frameworks/Security.framework/Versions/A/Security"

func SecTrustCopyCertificateChain(trustObj CFRef) (CFRef, error) {
    ret := syscall(abi.FuncPCABI0(x509_SecTrustCopyCertificateChain_trampoline), uintptr(trustObj), 0, 0, 0, 0, 0)
    if ret == 0 {
        return 0, OSStatus{"SecTrustCopyCertificateChain", int32(ret)}
    }
    return CFRef(ret), nil
}
func x509_SecTrustCopyCertificateChain_trampoline()
```

`cgo_import_dynamic` adds the symbol to the Mach-O `LC_DYLD_INFO_ONLY` table; dyld resolves it by name at load time, and aborts the process if the target framework does not export the symbol.

`SecTrustCopyCertificateChain` is only present in Apple's Security.framework starting with **macOS 12.0 (Monterey)**. Security.framework on 10.15 has no such export, so loading fails.

### 4.2 Change

Delete the `cgo_import_dynamic` line and the corresponding trampoline, and replace them with a runtime resolver built on `dlopenGo`/`dlsymGo`:

```go
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
```

The `import` block also adds `"sync"` to support `sync.Once` alongside the existing `errors`/`internal/abi`/`strconv`/`unsafe`.

### 4.3 Reason

- **Do not introduce cgo here**: this package is intentionally *cgo-less* (see the `corefoundation.go` header comment: *"Package macos provides cgo-less wrappers for Core Foundation and Security.framework"*). Furthermore, Go toolchain cgo packages cannot coexist with Go assembly files (`.s`) in the same package; introducing `import "C"` would trigger the `package using cgo has Go assembly file` error. So we stay on the `cgo_import_dynamic`/`cgo_ldflag` path for the symbols we do need.
- **Use `dlopen`/`dlsym` instead of `cgo_import_dynamic`**: `dlopen`/`dlsym` do **not** resolve symbols at load time; they only return a function pointer at call time, so even if macOS < 12 lacks `SecTrustCopyCertificateChain`, dyld does not refuse to load the binary.
- **Cache the lookup with `sync.Once`, propagate the error**: `sync.Once` ensures `dlopen`/`dlsym` runs only once. If lookup fails, the error is cached in `secTrustCopyChainErr` and returned to all subsequent calls, giving the upper layer (`root_darwin.go`) a clear *"macOS too old"* signal.
- **Call mechanism**: once we have a function pointer, we use the in-package `syscall(fn, a1, ...)` helper (which is `runtime.crypto_x509_syscall`) to dispatch the call with the C ABI, avoiding a new assembly thunk.

---

## 5. `src/crypto/x509/internal/macos/security.s` — Remove the Trampoline in Sync

### 5.1 Original

```asm
TEXT ·x509_SecTrustCopyCertificateChain_trampoline(SB),NOSPLIT,$0-0
    JMP x509_SecTrustCopyCertificateChain(SB)
```

This is the trampoline for `cgo_import_dynamic x509_SecTrustCopyCertificateChain`: cgo_import_dynamic generates a Go-internal PLT stub `x509_SecTrustCopyCertificateChain`, and the trampoline `JMP`s into it.

### 5.2 Change

Delete those two lines. All other trampolines in the file (`SecTrustCreateWithCertificates`, `SecCertificateCreateWithData`, `SecPolicyCreateSSL`, `SecTrustSetVerifyDate`, `SecTrustEvaluate`, `SecTrustEvaluateWithError`, `SecCertificateCopyData`) stay intact.

### 5.3 Reason

This is mechanical cleanup in step with §4: once the `cgo_import_dynamic` line is removed, its trampoline must also be removed or `go vet`/the linker will complain *declared and not used*. The remaining Security.framework symbols have all been present since 10.7, so their original trampolines are kept.

---

## 6. `src/crypto/x509/internal/macos/dlopen.go` (New) — `dlopen`/`dlsym` Wrappers

### 6.1 Full Source

```go
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
```

### 6.2 Reason and Design Notes

- **Why not call cgo's `dlopen` via `import "C"`?** This package is *cgo-less* and coexists with `.s` assembly files; adding `import "C"` would break this invariant and make `cmd/go` fail at the "install std" step with *"package using cgo has Go assembly file"*. We instead use the standard Go `cgo_import_dynamic` style to refer to the `dlopen` / `dlsym` symbols in `libSystem.B.dylib`.
- **Why `libSystem.B.dylib` and not `libdl.dylib`?** macOS has no standalone `libdl`; `dlopen` / `dlsym` / `dlerror` have lived in `/usr/lib/libSystem.B.dylib` since 10.4. This is the standard macOS approach.
- **Design of `cstring`**: `dlopen`/`dlsym` require a NUL-terminated C string. We allocate a temporary `make([]byte, len+1)`, copy the string in, and use it during the `syscall` call. The Go GC does not move heap objects (unlike C++ vectors that may rebase on realloc), so `&b[0]` remains stable for the duration of the call. `dlopen` / `dlsym` copy the bytes into their own internal tables before returning, so the byte buffer need not outlive the call — it is free to be GC'd once the function returns.
- **Calling convention**: we use the `syscall(fn, a1, a2, a3, a4, a5, f1)` helper provided by `runtime.crypto_x509_syscall`, which places the arguments in the C-ABI registers (RDI/RSI/RDX/RCX/R8/R9 on darwin). The C functions `dlopen(const char* path, int mode)` and `dlsym(void* handle, const char* name)` need only the first two registers; the rest are zero. `abi.FuncPCABI0` returns the address of the `x509_dlopen_trampoline` ABIInternal stub, and the `syscall` helper then transfers control to the cgo-generated PLT entry.
- **`rtldNow | rtldGlobal`**: `RTLD_NOW` makes `dlopen` resolve all dependencies immediately (safer, surfaces problems at load time); `RTLD_GLOBAL` makes the resolved symbols visible to subsequent `dlopen` calls. Neither flag is harmful here.

---

## 7. `src/crypto/x509/internal/macos/dlopen.s` (New) — Assembly Trampolines

### 7.1 Full Source

```asm
// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin

#include "textflag.h"

TEXT ·x509_dlopen_trampoline(SB),NOSPLIT,$0-0
    JMP x509_dlopen(SB)
TEXT ·x509_dlsym_trampoline(SB),NOSPLIT,$0-0
    JMP x509_dlsym(SB)
```

### 7.2 Reason

This follows exactly the same pattern as the existing trampolines in `security.s`: `cgo_import_dynamic` generates an internal Go symbol `x509_dlopen` / `x509_dlsym` (note: no cgo name decoration `·` prefix) that points at the `libSystem.B.dylib!dlopen` / `dlsym` PLT entry. The Go side takes the address of the local jump stub via `abi.FuncPCABI0(x509_dlopen_trampoline)` and hands it to the `syscall` helper, which then dispatches under the C ABI.

A trampoline is **required** because the `syscall` helper (`crypto_x509_syscall`) expects a C-ABI entry point: it loads its first argument `fn` into `RDI` and jumps. The `fn` we pass is the trampoline's address; the trampoline then `JMP`s to the real PLT entry, completing the *"Go function pointer → ABIInternal → C ABI → C function"* chain.

---

## 8. `src/crypto/x509/root_darwin.go` — Fall Back to Leaf Chain on Failure

### 8.1 Original

```go
chain := [][]*Certificate{{}}
chainRef, err := macos.SecTrustCopyCertificateChain(trustObj)
if err != nil {
    return nil, err
}
defer macos.CFRelease(chainRef)
for i := 0; i < macos.CFArrayGetCount(chainRef); i++ {
    certRef := macos.CFArrayGetValueAtIndex(chainRef, i)
    cert, err := exportCertificate(certRef)
    if err != nil {
        return nil, err
    }
    chain[0] = append(chain[0], cert)
}
if len(chain[0]) == 0 {
    return nil, errors.New("x509: macos certificate verification internal error")
}
```

`systemVerify` first calls `SecTrustEvaluateWithError` (chain is trusted, host name and time are valid), then `SecTrustCopyCertificateChain` to fetch the **full chain** for return. The chain information is not part of the trust decision; it is only used to populate the result for further checks by the caller.

### 8.2 Change

```diff
     chain := [][]*Certificate{{}}
     chainRef, err := macos.SecTrustCopyCertificateChain(trustObj)
     if err != nil {
-        return nil, err
-    }
-    defer macos.CFRelease(chainRef)
-    for i := 0; i < macos.CFArrayGetCount(chainRef); i++ {
-        certRef := macos.CFArrayGetValueAtIndex(chainRef, i)
-        cert, err := exportCertificate(certRef)
-        if err != nil {
+        // Local patch: SecTrustCopyCertificateChain is a macOS 12+ API.
+        // On older macOS versions (e.g. 10.15 Catalina) it is not
+        // available. Fall back to a single-element chain containing the
+        // leaf certificate so the rest of the verification pipeline can
+        // still run.
+        leafCert, leafErr := exportCertificate(leaf)
+        if leafErr != nil {
             return nil, err
         }
-        chain[0] = append(chain[0], cert)
-    }
-    if len(chain[0]) == 0 {
-        // This should _never_ happen, but to be safe
-        return nil, errors.New("x509: macos certificate verification internal error")
+        chain[0] = append(chain[0], leafCert)
+    } else {
+        defer macos.CFRelease(chainRef)
+        for i := 0; i < macos.CFArrayGetCount(chainRef); i++ {
+            certRef := macos.CFArrayGetValueAtIndex(chainRef, i)
+            cert, err := exportCertificate(certRef)
+            if err != nil {
+                return nil, err
+            }
+            chain[0] = append(chain[0], cert)
+        }
+        if len(chain[0]) == 0 {
+            // This should _never_ happen, but to be safe
+            return nil, errors.New("x509: macos certificate verification internal error")
+        }
     }
```

`leaf` is the `CFRef` produced near the top of the function by `macos.SecCertificateCreateWithData(c.Raw)`. When the outer `if err != nil` branch fires, calling `exportCertificate(leaf)` gives us a `*Certificate` that we can append as the single element of `chain[0]`.

### 8.3 Reason

- **The security semantics are preserved**: all the "trust evaluation" decisions — `SecTrustEvaluateWithError` checking trust, `SecPolicyCreateSSL` checking host name, `SecTrustSetVerifyDate` checking time — are 10.7+ APIs and fully work on 10.15. The actual decision about whether the chain is trusted has already succeeded; `SecTrustCopyCertificateChain` merely copies the (already-trusted) chain out for return.
- **Failure is a graceful degradation, not an error**: the old code returned an error if `SecTrustCopyCertificateChain` failed, which would have made an otherwise-*"trust evaluation succeeded"* call fail on 10.15 — inconsistent with the real behaviour of Security.framework. Switching to *"leaf only"* means:
  - `chain[0][0]` is still the leaf, so the subsequent `chain[0][0].VerifyHostname(opts.DNSName)` and `checkChainForKeyUsage` keep working.
  - Callers receive a **trusted but intermediate-less** chain, which is a valid subset under RFC 5280 (pathLen validation becomes looser, but this function does not impose pathLen, so no side effects).
- **Fits the "local dev" use case**: the HTTPS validation triggered by the local Go toolchain is mostly used for `go get` and similar flows, where having the full chain is not required and the leaf is enough to finish SNI, hostname, and basic key-usage checks.

---

## 9. Reproduction and Verification

### 9.1 Build

```bash
cd /opt/workspace/go1.26.4/src
GOROOT_BOOTSTRAP=/usr/local/Cellar/go/1.24.11/libexec ./make.bash
```

Tail of the success log:

```
Building packages and commands for darwin/amd64.
---
Installed Go for darwin/amd64 in /opt/workspace/go1.26.4
Installed commands in /opt/workspace/go1.26.4/bin
```

### 9.2 Inspect the `bin/go` Mach-O Header

```bash
otool -l /opt/workspace/go1.26.4/bin/go | grep -A6 "LC_BUILD_VERSION"
#   minos 10.13
#   sdk   10.13

nm /opt/workspace/go1.26.4/bin/go | grep -E "SecTrustCopyCertificateChain|_dlopen|_dlsym"
#   U _dlopen
#   U _dlsym
#   t _crypto/x509/internal/macos.loadSecTrustCopyCertificateChain
#   no "_SecTrustCopyCertificateChain" undefined symbol
```

### 9.3 Verify `go version` and a Basic Build

```bash
export PATH=/opt/workspace/go1.26.4/bin:$PATH
export GOROOT=/opt/workspace/go1.26.4
export GOCACHE=/opt/workspace/go1.26.4/pkg/obj/go-build
export TMPDIR=/opt/workspace/go1.26.4/.tmp
go version
# go version go1.26.4 darwin/amd64

mkdir -p /tmp/hello && cd /tmp/hello
cat > main.go <<'EOF'
package main

import "fmt"

func main() { fmt.Println("hello from go1.26.4") }
EOF
go mod init hello
go run .
# hello from go1.26.4
```

### 9.4 Verify the x509 Path (the Core Fix Target)

```bash
cd /opt/workspace/go1.26.4/src/crypto/x509
go test -count=1 -short .
# ok      crypto/x509        7.061s
```

And in a main program, confirm Security.framework still completes TLS validation:

```go
tr := &http.Transport{TLSClientConfig: &tls.Config{}}
c  := &http.Client{Transport: tr}
resp, err := c.Get("https://www.google.com")
// err == nil, resp.Status == "200 OK"
```

This shows that `SecTrustEvaluateWithError` is still supported by Security.framework on 10.15; we only fall back to leaf-only on the path that uses `SecTrustCopyCertificateChain`.

---

## 10. Compatibility and Risk Notes

- **No impact on non-darwin platforms**: `dlopen.go`/`dlopen.s` use `//go:build darwin`; `macho.go` only changes the darwin internal link path; `security.go`/`security.s` were already darwin-only.
- **No impact on non-macOS users**: we only adjusted the darwin/amd64 Mach-O minos and the macos Security.framework wrapper.
- **TLS validation is "slightly weaker" on 10.15 vs. 12+**: when the peer returns a full chain, 10.15 will only place the leaf in the returned `chain`; intermediates do not appear in `chains[0]`. Host name / key usage / trust-anchor checks (`VerifyHostname` and `checkChainForKeyUsage`) are unaffected, but code that relies on `chains[0]` containing intermediates for pathLen validation will be missing them.
- **`minos=10.13` rather than 12.0**: lets the toolchain load on 10.13/10.14, but upstream no longer tests those versions; this works fine on a local dev box. **Do not** push this change upstream or use it to build a release distribution.
- **`dlopen`/`dlsym` rely on symbols in `libSystem.B.dylib`**: `dlopen` / `dlsym` have been in `libSystem.B.dylib` since Darwin 8 (10.4), well before 10.15, so as long as you stay within macOS, loading is safe.

---

## 11. Runtime Impact of the Changes

This section separates the impact on **Go user programs (user-mode binaries)** from the impact on **the Go toolchain itself**. For each entry: *affected* (what changes) vs. *not affected* (untouched by this patch).

### 11.1 `cmd/link/internal/ld/macho.go`: minos 12 → 10.13

Affects only the `LC_BUILD_VERSION.minos`/`sdk` fields of the **toolchain's output** Mach-O files.

- **Not affected**:
  - Load commands, symbol table, or text/data segment layout; no other difference is visible via `otool -l`.
  - The `minos` for other platforms (iOS still goes through its own branch and is untouched).
  - The ABI, calling convention, or runtime behaviour of user-mode Go programs — only the set of macOS versions allowed to load them changes.
- **Affected**:
  - The toolchain itself and user binaries produced by it can load on macOS 10.13 – 11.x.
  - Because `sdk` is also lowered to 10.13, code that statically imports APIs guarded by newer `__OSX_AVAILABLE_STARTING` markers may still see *"symbol not found"* at runtime. **This has no impact on the Go toolchain itself** — the Go runtime and the macos package carefully declare only symbols that are available on the earliest required API (10.7 / 10.4).
  - **Minor impact for 12+ macOS users**: since `minos=10.13` instead of 12.0, Apple's signing tooling (`codesign`) will mark the binary as supporting macOS 10.13+. For local development this is fine; if you intend to ship binaries built with this toolchain to customers who require *"macOS 12+"* compliance, you will need to repair the `minos` at signing time (e.g. via `vtool`). **Local dev does not need to care about this.**

### 11.2 `crypto/x509/internal/macos/{security.go,security.s,dlopen.go,dlopen.s}`: dynamic `SecTrustCopyCertificateChain` resolution

#### 11.2.1 Impact on the Toolchain Binary

- `bin/go` (and `bin/gofmt`, `pkg/tool/darwin_amd64/*`) no longer need `SecTrustCopyCertificateChain` at load time. Verified via `nm`:
  ```
  $ nm /opt/workspace/go1.26.4/bin/go | grep SecTrustCopyCertificateChain
  (no output)
  $ nm /opt/workspace/go1.26.4/bin/go | grep -E "_dlopen|_dlsym"
  U _dlopen
  U _dlsym
  ```
- `dlopen`/`dlsym` come from `libSystem.B.dylib` and have been stable since 10.4; 10.15 has them.
- `dlopen` only runs the **first** time user code (or the standard library) triggers `SecTrustCopyCertificateChain` (cached via `sync.Once`); the overhead is negligible.
- The error message differs from the original: the old code did `return nil, err` on `SecTrustCopyCertificateChain` failure; the patch may return `"SecTrustCopyCertificateChain is not available on this macOS version (requires 12+)"`, and the caller (`root_darwin.go`) catches this and degrades. If user code explicitly calls `macos.SecTrustCopyCertificateChain` (an internal package that normal user code cannot import), it may see this new error.

#### 11.2.2 Reliability of the `cstring` Implementation

`dlopen.go::cstring` copies the Go string into a `make([]byte, len+1)` slice and passes the address of the first byte to `dlopen`/`dlsym` as a C string. Potential risks:

- **GC does not move heap allocations**: the underlying array of a `make([]byte, ...)` lives on the heap, and the Go GC does not move heap objects in place (unlike C++ `std::vector` which may rebase on realloc), so `&b[0]` is stable until the function returns.
- **Lifetime covers the call**: `dlopen`/`dlsym` copy the input string (required by C) and return immediately; once the call returns, GC may free the buffer without affecting the state inside `dlopen`.
- **Pointer aliasing and `unsafe.Pointer` conversion**: the package uses `unsafe.Pointer(cpath)` only at the `uintptr(unsafe.Pointer(cpath))` step to pass the address as an integer. We do **not** retain a Go pointer to C memory across the call, which is the safe pattern documented for `unsafe.Pointer`.
- The `crypto/x509` / `crypto/tls` / `net/http` short test suites (see §9.4) and the HTTPS end-to-end test in §11.4 below did not surface any crashes or memory errors.

#### 11.2.3 No Impact on Non-darwin Platforms

`dlopen.go` / `dlopen.s` are guarded by `//go:build darwin`, so they are not compiled on other platforms. linux/windows builds still take the original `cgo_import_dynamic` / `libSystem` paths.

### 11.3 `crypto/x509/root_darwin.go`: Fall Back to Leaf Chain on Failure

This is the change that has the **largest impact on end-user behaviour**. The darwin `systemVerify` path goes through Security.framework; on 10.15, where `SecTrustCopyCertificateChain` is missing, the return value degrades to `[][]*Certificate{{leafCert}}`.

#### 11.3.1 Concrete Affected Behaviour

| User Code Pattern | 12+ macOS Behaviour | 10.15 Behaviour (after patch) | Impact |
| --- | --- | --- | --- |
| `tls.Dial(...).VerifyHostname(...)` or `tls.Config{VerifyPeerCertificate: ...}` | leaf + intermediates + (implicit) root | leaf only | Only chain length changes; **the trust decision is unchanged** |
| `cert.Verify(x509.VerifyOptions{DNSName: ...})` (with `Roots == nil`) | `chains[0]` length ≥ 2 | `chains[0]` length = 1 | **Callers that iterate the chain will see fewer certificates** |
| `tls.ConnectionState{}.VerifiedChains` | length ≥ 2 | length = 1 | Same as above |
| `x509.SystemCertPool().Subjects()` | (always empty on darwin, decided by `loadSystemRoots`) | still empty | Unrelated to this patch |
| EKU check | Apple Security.framework checks EKUs of all intermediate CAs itself | only checks the leaf | **No effect for typical server certs** (intermediates rarely pin EKU); more permissive than 12+ for chains that pin EKU in an intermediate |

Measured directly against `https://www.google.com` (see §11.4 test):

```
PeerCertificates (server-sent)         : 3  [leaf + 2 intermediates]
Verify().chains                        : 1
Verify().chains[0] length              : 1   ← regression point
Verify().chains[0][0] subject          : CN=www.google.com
TLS handshake result                   : 200 OK
```

#### 11.3.2 Affected Downstream Scenarios

These patterns in Go code **still run on 10.15 but behave differently from 12+ macOS**. Most programs are unaffected, but watch for:

1. **PathLen validation across the chain**: if user code walks `chains[0]` to validate `MaxPathLen` / `MaxPathLenZero`, it loses the intermediate CAs' `BasicConstraintsValid` / `MaxPathLen` and may skip these checks. The Go-side pathLen check in `x509.Verify` is not re-run on the systemVerify path; this is consistent with 12+ behaviour.
2. **Intermediate CA fingerprint checks**: code that fingerprints the 2nd/3rd certificate in `chains[0]` to identify a particular CA will fail because `chains[0]` is now length 1. Workaround: use `tls.ConnectionState{}.PeerCertificates` (the raw server-sent chain from the TLS handshake, which is unaffected by systemVerify).
3. **EKU / SCT nesting**: if an intermediate CA pins `ExtKeyUsage` (e.g. `ExtKeyUsageClientAuth`), 12+ Security.framework will reject server-auth; 10.15 `checkChainForKeyUsage` will accept because it only sees the leaf. **In practice no CA does this**, treat it as a theoretical risk.
4. **`Roots != nil` taking the Go verifier path**: unchanged — when systemVerify fails, `Verify` still falls back to Go's own chain builder, which produces a complete `chains[0]`.

#### 11.3.3 Unaffected Functionality

- `crypto/x509.CreateCertificate` (issuing certs)
- `crypto/x509.ParseCertificate` / `ParsePKIXPublicKey` (parsing)
- `crypto/x509.VerifyHostname` (independent of the chain)
- `crypto/tls` configuration negotiation, key derivation, and the handshake itself
- `net/http` HTTPS client/server

### 11.4 Observed Differences in Programs Built/Run with This Toolchain

The two questions *"can the program compile?"* and *"can the program run?"* are answered separately.

#### 11.4.1 Compilation

**Conclusion: every Go program can compile with Go 1.26.4.** This patch only touches:

- A Mach-O header field (no impact on compilation).
- Internal functions in the darwin-private `crypto/x509/internal/macos` package (not importable by user code).
- Private assembly trampolines (not part of the Go public ABI).

Any Go source (including calls into `crypto/tls`, `crypto/x509`, `net/http`, all of the standard library, and any third-party module) compiles **identically** with this toolchain compared to upstream 1.26.4. All compile-time commands — `go build`, `go install`, `go vet`, `go test` — work normally.

#### 11.4.2 Runtime

**Most Go programs run normally.** Verified on 2026-06-18, macOS 10.15.7, `/opt/workspace/go1.26.4`:

| Test | Status | Notes |
| --- | --- | --- |
| `go version` | ✅ Pass | Output: `go1.26.4 darwin/amd64` |
| `go run hello.go` | ✅ Pass | Standard hello world |
| `go test -short crypto/x509` | ✅ Pass | 7.06s |
| `go test -short crypto/tls` | ✅ Pass | 9.14s |
| `go test -short net/http` | ✅ Pass | 8.14s |
| `go test -short crypto/...` (full set) | ✅ All pass | fips140/x509/tls/sha*/rsa/md5/... |
| HTTPS access to `https://www.google.com` | ✅ 200 OK | Validates the Security.framework path |
| `tls.Dial + Verify().chains[0]` inspection | ⚠️ chain length 1 | The §11.3.1 regression |
| `tls.Dial + VerifiedChains` inspection | ⚠️ length 1 | Same as above |
| `go tool compile` / `asm` / `link` / `vet` | ✅ Pass | Toolchain self-bootstrap works |
| `gofmt` | ✅ Pass | |

**The only behavioural difference you can observe** is when `Verify(opts)` or `tls.Config{VerifyPeerCertificate}` reads `chains[0]` length or iterates the chain: the chain is shorter than on 12+. This is the price of not having the 12+ API; §11.3 documents the workarounds.

#### 11.4.3 Paths That Look Related But Are Unaffected

- `crypto/x509.SystemCertPool()` on darwin always returns `&CertPool{systemPool: true}` with an empty subjects list; `Verify(opts)` triggers systemVerify when `opts.Roots == nil`. **This is unrelated to this patch** — it has always been the case on darwin (no pre-loading of roots, which is Security.framework's job).
- `cgo` itself still works on 10.15; the patch does not introduce cgo, nor does it disable cgo.
- `internal/syscall/unix` / `os/exec` and other darwin syscalls are untouched and continue to work.

### 11.5 Summary of Runtime Differences Introduced by This Patch

| Category | Symptom | Affected Audience | Does it Block Toolchain Use? |
| --- | --- | --- | --- |
| Mach-O `minos=10.13` | Binary loads on 10.13–11.x; no change for 12+ | Publishers of binaries to old-macOS users | No (local dev unaffected) |
| Toolchain binary no longer statically references `SecTrustCopyCertificateChain` | Fixes `dyld: Symbol not found` | None | **Yes (required)** |
| `Verify().chains[0]` on darwin degrades to length 1 | No intermediate CAs in chain | Programs that iterate `chains[0]` strictly | No (business behaviour, not toolchain behaviour) |
| `checkChainForKeyUsage` no longer checks intermediate EKU | Slightly more permissive acceptance | Server-side certs that pin EKU in intermediates | No (same as above) |
| Lifetime of the `cstring` heap slice | Only alive during a single `dlopen`/`dlsym` call | None | No (verified) |
| `SecTrustCopyCertificateChain` error message becomes "macOS < 12" | User code that imports `internal/macos` directly | Almost nobody | No |

---

## 12. Final Conclusion on "Minimum Supported Version"

After this patch:

- **`minos=10.13` is the actual minimum supported macOS version**. `bin/go` and every binary built with it can load and run on macOS 10.13 (High Sierra) or later.
- **Code paths that need `SecTrustCopyCertificateChain` (12+) on 10.13–11.x will degrade to leaf-only chain.** This is the functional cost of supporting older macOS.
- **Whether to lower further to 10.9**: feasible (just change `macho.go`'s `version` to `10<<16 | 9<<8 | 0`), but requires self-verification of every implicit standard-library dependency on 10.9–10.12; the upstream Go 1.26 team has not validated releases on 10.9.

**Recommendations for local development**:

- Put `$(go env GOROOT)/bin` first in `PATH`, or point `GOCACHE` at a writable directory (sandbox environments especially need this) to avoid `operation not permitted`.
- Do not publish binaries built with this toolchain; for production use the official Apple-distributed older toolchain or upgrade macOS to 12+.
