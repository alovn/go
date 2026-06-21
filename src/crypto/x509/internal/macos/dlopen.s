// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin

#include "textflag.h"

TEXT ·x509_dlopen_trampoline(SB),NOSPLIT,$0-0
	JMP x509_dlopen(SB)
TEXT ·x509_dlsym_trampoline(SB),NOSPLIT,$0-0
	JMP x509_dlsym(SB)
