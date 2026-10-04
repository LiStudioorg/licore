// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import "testing"

func TestExecOptionsValidate(t *testing.T) {
	if err := (&ExecOptions{Cmd: nil}).validate(); err == nil {
		t.Error("空命令应报错")
	}
	if err := (&ExecOptions{Cmd: []string{"/bin/sh"}}).validate(); err != nil {
		t.Fatalf("合法命令应通过: %v", err)
	}
	if err := (&ExecOptions{Cmd: []string{"/bin/sh", "a\x00b"}}).validate(); err == nil {
		t.Error("含 NUL 参数应报错")
	}
}
