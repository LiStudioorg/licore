// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package store

import "errors"

// ErrExists 表示目标目录下已存在同名镜像且未指定 --force。
var ErrExists = errors.New("licore/store: 镜像已存在")

// ErrImageNotFound 表示要操作的镜像引用在本地 store 中不存在。
var ErrImageNotFound = errors.New("licore/store: 镜像不存在")

// ErrImageInUse 表示镜像仍被容器引用，拒绝删除（除非强制）。
var ErrImageInUse = errors.New("licore/store: 镜像仍被容器使用")
