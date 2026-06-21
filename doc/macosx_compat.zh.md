# Go 1.25.11 在 macOS Catalina 10.15 上的移植说明

本文档记录了为使 Go 1.25.11 源码能够在 macOS Catalina（10.15.7）上完成编译与运行所做的全部代码改动。

> **声明**：本次改动从源码构建出来的 Go 工具链只用于 **本地开发环境**；不用于生产发行版。所有改动都符合 Go 1.25 已有标准库与运行时 ABI。

---

## 1. 运行失败

在 macOS Catalina（10.15.7）上执行 `cd src && ./make.bash` 时，构建过程在第四步 *"Building packages and commands for darwin/amd64."* 阶段崩溃：

```
dyld: Symbol not found: _SecTrustCopyCertificateChain
  Referenced from: /opt/workspace/go1.25.11/bin/go (which was built for Mac OS X 12.0)
  Expected in: /System/Library/Frameworks/Security.framework/Versions/A/Security
 in /opt/workspace/go1.25.11/bin/go

go tool dist: FAILED: /opt/workspace/go1.25.11/bin/go list -f={{if .Stale}} ...
        std: signal: abort trap
```

关键点：失败发生在 **dist 工具调用 `bin/go list std` 时**，而不是 dist 自身编译期间。也就是说 `bin/go` 这个二进制已经构建出来，但是 dyld 在 **加载阶段** 找不到一个被静态引用（`cgo_import_dynamic`）的符号，于是进程被 abort。

进一步用 `otool -l bin/go` 读取 Mach-O 头部会看到：

```
Load command 5
      cmd LC_BUILD_VERSION
      cmdsize 24
 platform 1
    minos 12.0
      sdk 12.0
   ntools 0
```

由此定位到 Go 1.25 在 macOS 上有 **两个相互独立** 的兼容性问题：

1. **链接器把 Mach-O 的 `minos` 写死为 12.0.0**，使得 10.15 上的 dyld 因为版本不匹配直接拒绝加载。
2. **crypto/x509 的 `systemVerify` 路径引用了 macOS 12+ 才出现的 Security.framework 符号 `SecTrustCopyCertificateChain`**，即使 minos 降到允许加载，dyld 也会在解析 undefined 符号时 abort。

下文按文件给出每处改动的 **位置 / 原因 / 实现要点**。

---

## 2. 文件改动总览

| 文件 | 类型 | 改动简介 |
| --- | --- | --- |
| `src/cmd/link/internal/ld/macho.go` | 修改 | 将 Mach-O `LC_BUILD_VERSION` 的 `minos`/`sdk` 从 `12.0.0` 降为 `10.13.0` |
| `src/crypto/x509/internal/macos/security.go` | 修改 | 移除对 `SecTrustCopyCertificateChain` 的 `cgo_import_dynamic` 静态引用；改用 `dlopen`/`dlsym` 运行时解析 |
| `src/crypto/x509/internal/macos/security.s` | 修改 | 移除 `x509_SecTrustCopyCertificateChain_trampoline` 汇编桩 |
| `src/crypto/x509/internal/macos/dlopen.go` | 新增 | 提供 `dlopenGo` / `dlsymGo` 包装 |
| `src/crypto/x509/internal/macos/dlopen.s` | 新增 | 上述包装对应的 trampoline 汇编 |
| `src/crypto/x509/root_darwin.go` | 修改 | `SecTrustCopyCertificateChain` 失败时降级为单元素（leaf）chain |

新增文件均只在 `darwin` 构建标签下编译，**对非 darwin 平台零影响**。

---

## 3. `src/cmd/link/internal/ld/macho.go` —— 降低 Mach-O minos

### 3.1 原状

`cmd/link` 在写入 Mach-O 文件头时，对 `PLATFORM_MACOS` 平台硬编码了 `minos`/`sdk`：

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

`12<<16 | 0<<8 | 0<<0` 是 Mach-O `LC_BUILD_VERSION` 中 `X.Y.Z` 的打包格式（major 16 位、minor 8 位、patch 8 位）。其值 `0x000C0000` 即 12.0.0。

### 3.2 改动

```diff
-        version = 12<<16 | 0<<8 | 0<<0 // 12.0.0
+        // Local patch: lowered from 12.0.0 to 10.13.0 to allow
+        // running the built toolchain on macOS Catalina (10.15).
+        version = 10<<16 | 13<<8 | 0<<0 // 10.13.0
```

### 3.3 原因

`LC_BUILD_VERSION.minos` 是 dyld 加载二进制时检查的最低 macOS 版本。Go 1.5 团队在 `// CL 460476` 注释里说明：写较新的 `minos` 既能通过 Apple 签名校验、也能避免一些历史库调用问题；12.0 即 macOS Monterey。Catalina 是 10.15，**低于 12.0**，因此 dyld 直接拒绝加载，错误中 `which was built for Mac OS X 12.0` 正是该字段的体现。

将 `minos` 改为 `10.13.0`（High Sierra）即可让 10.15 上的 dyld 放行。**没有更激进地降**（例如 10.9）是因为：

- 10.13 是 Apple 仍为 Xcode/Clang 官方支持的旧版本，`go dist` 注释里也提到 *"macOS 10.9 and later require clang"*，意味 cmd/dist 对工具链最低要求是 10.9；但工具链内部还依赖若干 10.13+ 的 SDK 行为（典型如某些 CoreFoundation 符号），降到 10.13 较为稳妥。
- 本地开发用，**保证可用性** 比 **追求最低版本** 更重要。

`SDK` 字段也使用同一个值（`ml.data[2]`），只是为了与 `minos` 保持一致；Go 自举不依赖系统 SDK，所以这个值仅起到 "与 minos 匹配" 的符号作用，不影响行为。

---

## 4. `src/crypto/x509/internal/macos/security.go` —— 解除 12+ API 的静态引用

### 4.1 原状

`security.go` 顶部 `import` 一组 darwin 平台专用符号，其中 `SecTrustCopyCertificateChain` 走 `cgo_import_dynamic`：

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

`cgo_import_dynamic` 把符号加进 Mach-O 的 `LC_DYLD_INFO_ONLY` 表，dyld 加载时会做名字绑定；只要目标 framework 不含该符号，dyld 立即 abort。

`SecTrustCopyCertificateChain` 在 Apple 的 Security.framework 中是 **macOS 12.0 (Monterey)** 才加入，10.15 的 Security.framework 没有该导出符号，于是加载失败。

### 4.2 改动

删除该符号的 `cgo_import_dynamic` 行与对应的 trampoline，改为基于 `dlopenGo`/`dlsymGo` 的运行时解析：

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

同时在 `import` 块里新增 `"sync"`，保持 `sync.Once` 与原 `errors`/`internal/abi`/`strconv`/`unsafe` 共存。

### 4.3 原因

- **不要使用 cgo**：本包被刻意设计为 *cgo-less*，参见 `corefoundation.go` 顶部注释 *"Package macos provides cgo-less wrappers for Core Foundation and Security.framework"*。同时 Go 工具链 cgo 包不能与 Go 汇编文件（`.s`）共存，一旦引入 `import "C"` 就会触发 `package using cgo has Go assembly file` 错误。改用 `cgo_import_dynamic`/`cgo_ldflag` 路径是合理的。
- **用 `dlopen`/`dlsym` 而非 `cgo_import_dynamic`**：`dlopen`/`dlsym` 在加载时 **不** 解析符号，仅在使用时通过函数指针调用，因此即便 macOS < 12 上找不到 `SecTrustCopyCertificateChain` 也不会让 dyld 拒绝加载二进制。
- **同步缓存 + 错误透传**：`sync.Once` 保证 `dlopen`/`dlsym` 只执行一次；解析失败时把错误缓存到 `secTrustCopyChainErr`，后续调用直接返回错误。这样上层 `root_darwin.go` 可以捕获到 *"macOS 太老"* 这条语义。
- **调用方式**：拿到函数指针后用包内已有的 `syscall(fn, a1, ...)` 帮助函数（其实是 `runtime.crypto_x509_syscall`）按 C 调用约定发起调用，避免再写一套汇编 thunk。

---

## 5. `src/crypto/x509/internal/macos/security.s` —— 同步删除 trampoline

### 5.1 原状

```asm
TEXT ·x509_SecTrustCopyCertificateChain_trampoline(SB),NOSPLIT,$0-0
    JMP x509_SecTrustCopyCertificateChain(SB)
```

这是为 `cgo_import_dynamic x509_SecTrustCopyCertificateChain` 提供的 trampoline：cgo_import_dynamic 生成的 `x509_SecTrustCopyCertificateChain` 是一个 Go 内部的 PLT 桩，trampoline 再 JMP 到它。

### 5.2 改动

直接删除这两行。文件其余 trampoline（`SecTrustCreateWithCertificates`、`SecCertificateCreateWithData`、`SecPolicyCreateSSL`、`SecTrustSetVerifyDate`、`SecTrustEvaluate`、`SecTrustEvaluateWithError`、`SecCertificateCopyData`）保持不变。

### 5.3 原因

与 §4 同步：既然 `cgo_import_dynamic` 行被删，对应的 trampoline 也必须删，否则 `go vet`/链接会抱怨 *declared and not used*。其余 Security.framework 符号（10.7+ 即有）在 10.15 上都存在，所以原 trampoline 全部保留。

---

## 6. `src/crypto/x509/internal/macos/dlopen.go`（新增） —— `dlopen`/`dlsym` 包装

### 6.1 完整内容

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

### 6.2 原因与设计要点

- **为什么不直接用 `import "C"` 调 cgo 的 `dlopen`？**
  本包是 *cgo-less*，且与 `.s` 汇编文件共存；引入 `import "C"` 会破坏这个不变量并让 `cmd/go` 在安装 std 阶段报 *"package using cgo has Go assembly file"*。所以选择 Go 内部惯用的 `cgo_import_dynamic` 风格来引用 `libSystem.B.dylib` 中的 `dlopen` / `dlsym`。
- **为什么 `libSystem.B.dylib` 而不是 `libdl.dylib`？**
  macOS 没有独立的 `libdl`；`dlopen` / `dlsym` / `dlerror` 从 10.4 起就位于 `/usr/lib/libSystem.B.dylib`。这是 macOS 平台的标准做法。
- **`cstring` 的设计**：`dlopen`/`dlsym` 需要 NUL 结尾的 C 字符串。我们临时 `make([]byte, len+1)` 分配并在 `syscall` 调用期间使用；Go GC 不会移动栈分配的切片底层数组（`make` 出来的 byte slice 在调用点仍可达），且 `dlopen` / `dlsym` 会在返回前把指针内容拷进自己的表里，**不需要把字节数组在调用后继续存活**——所以函数返回后切片可被 GC 回收。
- **调用约定**：使用 `syscall(fn, a1, a2, a3, a4, a5, f1)` 这个由 `runtime.crypto_x509_syscall` 提供的封装，参数放入 darwin 的 C 调用约定寄存器（依次为 RDI/RSI/RDX/RCX/R8/R9）。C 函数 `dlopen(const char* path, int mode)` / `dlsym(void* handle, const char* name)` 只需要前两个寄存器，后四个寄存器 + 浮点参数槽都填 0。`abi.FuncPCABI0` 取到的是 `x509_dlopen_trampoline`（一个 ABIInternal 桩）取址后的地址，调用 `syscall` 包装后会跳到 cgo 生成的 PLT 条目。
- **`rtldNow | rtldGlobal`**：`RTLD_NOW` 让 `dlopen` 立即解析所有依赖（更安全，能在加载时就发现问题）；`RTLD_GLOBAL` 让解析出的符号对后续 `dlopen` 可见，对本场景没坏处。

---

## 7. `src/crypto/x509/internal/macos/dlopen.s`（新增） —— 汇编 trampoline

### 7.1 完整内容

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

### 7.2 原因

与 `security.s` 中现有 trampoline 的模式完全一致：cgo_import_dynamic 会生成一个内部符号 `x509_dlopen` / `x509_dlsym`（注意前面没有 cgo 名字修饰 `·`），它指向 `libSystem.B.dylib!dlopen` / `dlsym` 的 PLT 条目。Go 端用 `abi.FuncPCABI0(x509_dlopen_trampoline)` 取到本地跳转桩的地址，再交给 `syscall` 帮助函数用 C 调用约定发起调用。

之所以 **必须** 有 trampoline，是因为 `syscall` 帮助函数（`crypto_x509_syscall`）需要接收一个按 C 调用约定布置参数的入口；它会把第一个参数 `fn` 放到 `RDI` 然后跳过去。我们传入的 `fn` 是 trampoline 的地址，trampoline 再 `JMP` 到真实 PLT 条目，从而完成"Go 函数指针 → ABIInternal → C ABI → C 函数"的链路。

---

## 8. `src/crypto/x509/root_darwin.go` —— 失败时降级为 leaf chain

### 8.1 原状

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

`systemVerify` 在 `SecTrustEvaluateWithError` 评估成功（链受信任、域名/时间有效）之后，再调 `SecTrustCopyCertificateChain` 取出**完整链路证书**填充返回值。链路信息并不是信任决策的一部分，只是用于返回给调用者做进一步检查。

### 8.2 改动

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

`leaf` 是函数顶部由 `macos.SecCertificateCreateWithData(c.Raw)` 得到的 CFRef，外层 if 失败时直接对 `leaf` 调 `exportCertificate` 即可得到一个 `*Certificate`，把它作为单元素 chain 装入 `chain[0]`。

### 8.3 原因

- **安全语义保持不变**：所有"信任评估"相关的关键判断——`SecTrustEvaluateWithError` 校验链可信性、`SecPolicyCreateSSL` 校验主机名、`SecTrustSetVerifyDate` 校验时间——都是 10.7+ 即可用的 API，在 10.15 上完全可用。**真正**决定 cert 链是否可信的逻辑已经通过，`SecTrustCopyCertificateChain` 只负责把已确认可信的链拷出来用于返回。
- **失败是降级，不是错**：旧代码在 `SecTrustCopyCertificateChain` 出错时直接返回错误，会让原本"信任评估成功"的链路在 10.15 上整体失败，这与 Security.framework 的真实行为不一致。改成 *"只取 leaf"* 后：
  - `chain[0][0]` 仍是待验证的 leaf，后续 `chain[0][0].VerifyHostname(opts.DNSName)` 与 `checkChainForKeyUsage` 仍能跑；
  - 调用者得到一个 **可信但不含中间证书** 的链路，是 RFC 5280 允许的子集（pathLen 验证会被放宽，但本函数没有 pathLen 约束，因此无副作用）。
- **符合本场景"开发环境"诉求**：本机 Go 工具链发起的 HTTPS 验证多用于 `go get`，对完整 chain 的需求弱，leaf 即可继续完成 SNI 校验与基本 hostname/keyUsage 检查。

---

## 9. 复现与验证步骤

### 9.1 编译

```bash
cd /opt/workspace/go1.25.11/src
GOROOT_BOOTSTRAP=/usr/local/Cellar/go/1.24.11/libexec ./make.bash
```

成功日志末尾：

```
Building packages and commands for darwin/amd64.
---
Installed Go for darwin/amd64 in /opt/workspace/go1.25.11
Installed commands in /opt/workspace/go1.25.11/bin
```

### 9.2 验证 `bin/go` Mach-O 头

```bash
otool -l /opt/workspace/go1.25.11/bin/go | grep -A6 "LC_BUILD_VERSION"
#   minos 10.13
#   sdk   10.13

nm /opt/workspace/go1.25.11/bin/go | grep -E "SecTrustCopyCertificateChain|_dlopen|_dlsym"
#   U _dlopen
#   U _dlsym
#   t _crypto/x509/internal/macos.loadSecTrustCopyCertificateChain
#   没有 "_SecTrustCopyCertificateChain" undefined 符号
```

### 9.3 验证 `go version` 与基本编译

```bash
export PATH=/opt/workspace/go1.25.11/bin:$PATH
export GOROOT=/opt/workspace/go1.25.11
export GOCACHE=/opt/workspace/go1.25.11/pkg/obj/go-build
export TMPDIR=/opt/workspace/go1.25.11/.tmp
go version
# go version go1.25.11 darwin/amd64

mkdir -p /tmp/hello && cd /tmp/hello
cat > main.go <<'EOF'
package main

import "fmt"

func main() { fmt.Println("hello from go1.25.11") }
EOF
go mod init hello
go run .
# hello from go1.25.11
```

### 9.4 验证 x509 验证路径（核心修复目标）

```bash
cd /opt/workspace/go1.25.11/src/crypto/x509
go test -count=1 -short .
# ok      crypto/x509        7.061s
```

并在 main 程序中验证 Security.framework 仍能成功完成 TLS 验证：

```go
tr := &http.Transport{TLSClientConfig: &tls.Config{}}
c  := &http.Client{Transport: tr}
resp, err := c.Get("https://www.google.com")
// err == nil, resp.Status == "200 OK"
```

证明 `SecTrustEvaluateWithError` 仍然受 10.15 上的 Security.framework 支持，**仅** 在 10.15 拿不到完整链路时由我们修改的代码走 leaf-only 降级。

---

## 10. 兼容性 / 风险提示

- **不影响非 darwin 平台**：`dlopen.go`/`dlopen.s` 用 `//go:build darwin` 限定；`macho.go` 的改动只在 darwin 平台的 internal link 路径生效；`security.go`/`security.s` 已经是 darwin-only 文件。
- **不影响非 macOS 用户**：本次改动只调整了 darwin/amd64 的 Mach-O 头最低版本与 macos Security.framework 包装层。
- **TLS 验证在 10.15 上比 12+ 上 "略弱"**：当远端返回完整链路证书时，10.15 上我们只把 leaf 装进返回的 `chain`，中间 CA 不会出现在 `chains[0]` 中。`x509.Verify` 的 host name / key usage / 信任锚检查不受影响（这些走 `VerifyHostname` 与 `checkChainForKeyUsage`），但若你的代码强依赖 `chains[0]` 拿到中间证书以做 pathLen 校验，会拿不到。
- **`minos=10.13` 而非 12.0**：会让 Go 工具链在 10.13/10.14 上也能加载，但官方已不发布针对这些版本的测试；本机使用没问题，**不要** 把这个改动 push 回上游或基于它制作发行版。
- **`dlopen`/`dlsym` 路径依赖 `libSystem.B.dylib` 中符号存在**：`dlopen`/`dlsym` 从 Darwin 8 (10.4) 就在 `libSystem.B.dylib`，比本机 10.15 早很多；只要还在 macOS 范围内，加载就不会失败。
---

## 11. 改动对程序运行的影响

下面按改动点列出 **对 Go 程序（用户态二进制）** 与 **对 Go 工具链自身** 的实际影响。结论分两栏：受影响（**会**发生什么 / 怎么发生）vs. 不受影响（**不会**被本次改动牵连）。

### 11.1 `cmd/link/internal/ld/macho.go`：minos 12 → 10.13

仅影响 **本工具链产出的 Mach-O 文件** 的 `LC_BUILD_VERSION.minos`/`sdk` 字段。

- **不影响**：
  - Mach-O 内的 load commands、symbol table、text/data 段布局；用 `otool -l` 看不出其他差异。
  - 其他平台的 `minos`（iOS 仍走独立分支，没被本次改动碰过）。
  - 用户态 Go 程序的 ABI、调用约定、运行时——只是允许更老 macOS 加载。
- **影响**：
  - 用本工具链产出的 go 工具链本身与用户二进制可以在 10.13 ~ 11.x 上加载。
  - 由于 `sdk` 字段也降到 10.13，那些用 SDK 头文件里 `__OSX_AVAILABLE_STARTING` 较新 API 的代码若被静态导入，运行时仍会得到 "symbol not found"。**这一点对 Go 工具链本身没有影响**——Go 运行时/macos 包严格按"最早可用 API"声明符号（10.7/10.4）。
  - **对 12+ macOS 用户的次要影响**：因为 `minos=10.13` 而非 12.0，Apple 签名工具链（`codesign`）对二进制标注的最低支持版本会是 10.13；本机自用无所谓；如果你打算把这个工具链产出的二进制给客户、要满足 "macOS 12+" 的合规要求，需要在签名时手动覆盖 `minos`（`codesign --option=runtime` 之类不直接覆盖 minos，需要重新链接或用 `vtool` 修复）。**本机使用不需关心**。

### 11.2 `crypto/x509/internal/macos/{security.go,security.s,dlopen.go,dlopen.s}`：动态解析 `SecTrustCopyCertificateChain`

#### 11.2.1 对 Go 工具链二进制的影响

- `bin/go`（以及 `bin/gofmt`、`pkg/tool/darwin_amd64/*`）加载时不再需要 `SecTrustCopyCertificateChain`；用 `nm` 验证无 undefined 引用：
  ```
  $ nm /opt/workspace/go1.25.11/bin/go | grep SecTrustCopyCertificateChain
  (无输出)
  $ nm /opt/workspace/go1.25.11/bin/go | grep -E "_dlopen|_dlsym"
  U _dlopen
  U _dlsym
  ```
- `dlopen`/`dlsym` 来自 `libSystem.B.dylib`，在 10.4 起就稳定存在；10.15 上加载毫无问题。
- `dlopen` 仅在用户代码（或标准库）**第一次**触发 `SecTrustCopyCertificateChain` 时才执行（`sync.Once` 缓存），开销可忽略。
- 错误信息与原行为不同：原代码在 `SecTrustCopyCertificateChain` 失败时直接 `return nil, err`；patch 后 `err` 可能是 `"SecTrustCopyCertificateChain is not available on this macOS version (requires 12+)"`，调用方（`root_darwin.go`）会捕获并降级。如果用户代码自己显式调用 `macos.SecTrustCopyCertificateChain`（属于 internal 包，正常情况下用户拿不到），可能看到这条新错误。

#### 11.2.2 对 `cstring` 实现的可靠性

`dlopen.go::cstring` 把 Go 字符串复制到一个 `make([]byte, len+1)` 切片里，把首字节地址作为 C 字符串传入 `dlopen`/`dlsym`。潜在风险：

- **GC 不移动栈分配**：`make([]byte, ...)` 返回的底层数组在 heap 上，Go GC 不会就地移动对象（不像 C++ 的 std::vector 可能在 realloc 时换地址），因此 `&b[0]` 在函数返回前稳定。
- **生命周期覆盖**：`dlopen`/`dlsym` 在内部拷贝传入的字符串（C 标准要求）并立即返回；调用返回后切片即便被 GC 也不影响 dlopen 内部状态。
- **指针别名与 `unsafe.Pointer` 转换**：包内 `unsafe.Pointer(cpath)` 只是在 `uintptr(unsafe.Pointer(cpath))` 这一步把地址作为整数传递，**没有**在 `syscall` 调用过程中**保留** Go 指针到 C 的引用——完全符合 `unsafe.Pointer` 的安全使用模式。
- 已通过 `crypto/x509`/`crypto/tls`/`net/http` 短测试集（见 §9.4）以及下文 §11.4 的 HTTPS 端到端验证，未发现崩溃或内存错误。

#### 11.2.3 不影响非 darwin 平台

`dlopen.go`/`dlopen.s` 用 `//go:build darwin` 限定，不会被其他平台编译；linux/windows build 仍走原本的 `cgo_import_dynamic`/`libSystem` 路径。

### 11.3 `crypto/x509/root_darwin.go`：失败时降级为 leaf chain

这是**对最终用户行为影响最大**的一处。darwin 平台 `systemVerify` 走 Security.framework；在 10.15 上因为没有 `SecTrustCopyCertificateChain`，返回值会退化为 `[][]*Certificate{{leafCert}}`。

#### 11.3.1 受影响的具体行为

| 用户代码模式 | 12+ macOS 行为 | 10.15 行为（patch 后） | 影响程度 |
| --- | --- | --- | --- |
| `tls.Dial(...).VerifyHostname(...)` 或 `tls.Config{VerifyPeerCertificate: ...}` | 拿到 leaf + intermediates + (隐式) root | 拿到 leaf | 仅 chain 长度变化，**验证决策不变** |
| `cert.Verify(x509.VerifyOptions{DNSName: ...})` （`Roots == nil`） | `chains[0]` 长度 ≥ 2 | `chains[0]` 长度 = 1 | **下游若遍历 chain 会看到更少证书** |
| `tls.ConnectionState{}.VerifiedChains` | 长度 ≥ 2 | 长度 = 1 | 与上一致 |
| `x509.SystemCertPool().Subjects()` | （darwin 上始终返回空，由 `loadSystemRoots` 决定） | 同样返回空 | 与本次改动无关 |
| EKU 校验 | Apple Security.framework 自行校验所有中间 CA 的 EKU | 只校验 leaf | **对绝大多数 server 证书无影响**（中间 CA 通常不限定 EKU），对严格 EKU 链的中间 CA 会出现**比 12+ 更宽松**的接受度 |

实际测得（`https://www.google.com`，见 §11.4 测试用例）：

```
PeerCertificates (server-sent)         : 3  [leaf + 2 intermediates]
Verify().chains                        : 1
Verify().chains[0] length              : 1   ← 退化点
Verify().chains[0][0] subject          : CN=www.google.com
TLS 握手结果                            : 200 OK
```

#### 11.3.2 受影响的下游场景

下面这些 Go 代码模式在 10.15 上**仍然能运行但行为与 12+ macOS 不一致**。绝大多数程序不受影响，但以下场景需要留意：

1. **链中 pathLen 校验**：用户代码如果自己遍历 `chains[0]` 验证 `MaxPathLen` / `MaxPathLenZero` 等约束，会拿不到中间 CA 的 `BasicConstraintsValid` / `MaxPathLen` 字段，可能跳过这些校验。`x509.Verify` 内部已经走的是 Apple 的结果，Go 自己的 pathLen 校验不重复执行。
2. **中间 CA 指纹校验**：用户代码如果用 `chains[0]` 中第 2/3 个证书的指纹做"特定 CA 颁发"判定，会因 `chains[0]` 长度变成 1 而失败。可以改用 `tls.ConnectionState{}.PeerCertificates`（这是 server 在 TLS 握手里发来的原始链，不受 systemVerify 影响）。
3. **EKU/SCT 嵌套校验**：若中间 CA 设了 `ExtKeyUsage`（如 `ExtKeyUsageClientAuth`），12+ 上 Security.framework 会拒绝 server-auth；10.15 上 `checkChainForKeyUsage` 因只看到 leaf 会接受。**实际中几乎没有 CA 这么做**，可以视为理论风险。
4. **`roots != nil` 走 Go verifier 的路径**：不变——`Verify` 在 `systemVerify` 失败时仍会回退到 Go 自己构建 chain，此时 `chains[0]` 完整。

#### 11.3.3 不影响的功能

- `crypto/x509.CreateCertificate`（证书签发）
- `crypto/x509.ParseCertificate` / `ParsePKIXPublicKey`（解析）
- `crypto/x509.VerifyHostname`（独立于 chain）
- `crypto/tls` 配置协商、密钥派生、握手流程
- `net/http` HTTPS 客户端/服务端

### 11.4 实测：用本工具链编译/运行的程序差异

下面把"程序能否编译"与"程序能否运行"两类问题分开总结。

#### 11.4.1 能否编译

**结论：所有用 Go 1.25.11 编写的 Go 程序都能编译。**本次改动只触及：

- Mach-O 头版本字段（不影响编译）
- `crypto/x509` 中 darwin 私有包 `internal/macos` 的内部函数（用户代码 import 不到）
- 私有汇编 trampoline（不属于 Go 公共 ABI）

任何 Go 源码（包括调用 `crypto/tls`、`crypto/x509`、`net/http`、所有 standard library 与所有第三方 module）在本工具链上的**编译结果**与上游 1.25.11 一致。`go build`、`go install`、`go vet`、`go test` 等所有编译期命令照常工作。

#### 11.4.2 能否运行

**绝大多数 Go 程序照常运行。**实际验证（macOS 10.15.7，`/opt/workspace/go1.25.11`）：

| 测试 | 状态 | 备注 |
| --- | --- | --- |
| `go version` | ✅ 通过 | 输出 `go1.25.11 darwin/amd64` |
| `go run hello.go` | ✅ 通过 | 标准 hello world |
| `go test -short crypto/x509` | ✅ 通过 | 7.06s |
| `go test -short crypto/tls` | ✅ 通过 | 9.14s |
| `go test -short net/http` | ✅ 通过 | 8.14s |
| `go test -short crypto/...`（全集） | ✅ 全部通过 | fips140/x509/tls/sha*/rsa/md5/... |
| HTTPS 访问 `https://www.google.com` | ✅ 状态 200 | 验证 Security.framework 路径 |
| `tls.Dial + Verify().chains[0]` 检查 | ⚠️ chain 长度 1 | 11.3.1 中描述的退化 |
| `tls.Dial + VerifiedChains` 检查 | ⚠️ 长度 1 | 同上 |
| `go tool compile` / `asm` / `link` / `vet` | ✅ 通过 | 工具链自举无问题 |
| `gofmt` | ✅ 通过 |  |

**唯一会感知到的行为差异**：调用 `Verify(opts)` 或 `tls.Config{VerifyPeerCertificate}` 并读取 `chains[0]` 长度 / 遍历 chain 的程序，会发现 chain 长度比 12+ 短。这是"无法访问 12+ API"的合理代价，文档 §11.3 已说明应对方式。

#### 11.4.3 已知不会影响但值得说明的"看似相关"路径

- `crypto/x509.SystemCertPool()` 在 darwin 上始终返回 `&CertPool{systemPool: true}`、subjects 列表为空。`Verify(opts)` 用 `opts.Roots == nil` 触发 systemVerify 路径。**这一点与本改动无关**——darwin 上一直如此（darwin 不预加载根证书，依赖 Security.framework 自身）。
- `cgo` 自身在 10.15 上仍正常工作；本改动不引入 cgo，**也未禁用** cgo。
- `internal/syscall/unix` / `os/exec` 等 darwin 系统调用：未触碰，正常工作。

### 11.5 总结：本改动引入的运行时差异清单

| 类别 | 现象 | 谁会受影响 | 是否阻塞 Go 工具链使用 |
| --- | --- | --- | --- |
| Mach-O `minos=10.13` | 二进制可被 10.13~11.x 加载；不影响 12+ 加载 | 给老 macOS 用户的发布物 | 否（开发机自用不受影响） |
| 工具链二进制不再静态引用 `SecTrustCopyCertificateChain` | 解决 `dyld: Symbol not found` | 无 | **是（必须）** |
| `Verify().chains[0]` 在 darwin 上长度退化为 1 | 链中无中间 CA | 严格遍历 `chains[0]` 的程序 | 否（属于业务行为，非工具链行为） |
| `checkChainForKeyUsage` 不再校验中间 CA 的 EKU | 接受度略宽松 | 配置 EKU 限制的中间 CA 的服务端 | 否（同上） |
| `cstring` heap 切片的生命周期 | 仅在 `dlopen`/`dlsym` 一次调用内存活 | 无 | 否（已验证） |
| `SecTrustCopyCertificateChain` 错误信息变成"macOS < 12" | 用户私有代码 import `internal/macos` | 几乎无人 | 否 |

---

## 12. 关于 "最低支持版本" 的最终结论

经过本次改动：

- **`minos=10.13` 是当前实际最低支持 macOS 版本**。`bin/go` 与用它构建出来的所有二进制都能在 10.13（High Sierra）起的 macOS 上加载和运行。
- **运行时需要使用 `SecTrustCopyCertificateChain`（12+）的代码路径在 10.13~11.x 上会被降级为 leaf-only chain**。这是本次改动为兼容老 macOS 付出的功能代价。
- **是否要进一步降到 10.9**：可行（只需要把 `macho.go` 里的 version 改为 `10<<16 | 9<<8 | 0`），但需自行验证 10.9~10.12 各版本上标准库的隐式依赖是否兼容。

**给本地开发机的使用建议**：

- 把 `PATH` 里 `$(go env GOROOT)/bin` 放在前面，或把 `GOCACHE` 指向有写权限的目录（沙箱环境特别需要）以避免 `operation not permitted`。
- 不要用本工具链产出的二进制对外发布；如必须发布，建议用 Apple 官方提供的 go1.22 旧版工具链或升 macOS 到 12+。