// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// batchOptions 是批量转换的参数。
type batchOptions struct {
	fromFile  string
	outputDir string
	arch      string
	noCleanup bool
	keepImage bool
	dataDir   string
}

// runBatchConvert 执行批量转换（第二步实现）。
func runBatchConvert(_ *cobra.Command, _ io.Writer, _ batchOptions) error {
	return fmt.Errorf("convert: 批量转换尚未实现")
}
