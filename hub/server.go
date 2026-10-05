// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package hub

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// ServerOption 配置 Server 的可选参数。
type ServerOption func(*Server)

// WithAuth 覆盖默认 JWT 密钥与有效期。
func WithAuth(secret string) ServerOption {
	return func(s *Server) { s.auth = NewAuthenticator(secret, 24*time.Hour) }
}

// Server 是 LiCore hub 的 HTTP 服务。
type Server struct {
	reg  *Registry
	auth *Authenticator
	// users 是登录口令表（预留：可由外部注入）。
	users map[string]string
}

// NewServer 构造 hub 服务。
func NewServer(reg *Registry, opts ...ServerOption) (*Server, error) {
	if reg == nil {
		return nil, fmt.Errorf("registry 不能为空")
	}
	// 默认密钥必须是密码学随机的；生成失败即启动失败（见 randSecret 说明）。
	secret, err := randSecret()
	if err != nil {
		return nil, err
	}
	s := &Server{
		reg:   reg,
		auth:  NewAuthenticator(secret, 24*time.Hour),
		users: map[string]string{},
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// SetUser 注册一个登录用户（name → password）。
func (s *Server) SetUser(name, pass string) {
	if s.users == nil {
		s.users = map[string]string{}
	}
	s.users[name] = pass
}

// Handler 返回完整的 http.Handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.route)
	return mux
}

// route 按路径前缀分发；除 login 外全部需鉴权。
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/auth/login" && r.Method == http.MethodPost:
		s.handlLogin(w, r)
	default:
		if !s.authorized(w, r) {
			return
		}
		s.routeAuthed(w, r, p)
	}
}

func (s *Server) routeAuthed(w http.ResponseWriter, r *http.Request, p string) {
	switch {
	case strings.HasPrefix(p, "/tags/"):
		s.handlTag(w, r, strings.TrimPrefix(p, "/tags/"))
	case strings.HasPrefix(p, "/blobs/"):
		s.handlBlob(w, r, strings.TrimPrefix(p, "/blobs/"))
	case p == "/search":
		s.handlSearch(w, r)
	case p == "/_catalog":
		s.handlCatalog(w, r)
	default:
		http.NotFound(w, r)
	}
}

// ------------------ 鉴权 ------------------

// authorized 校验 Authorization: Bearer <jwt>。
func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	hdr := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(hdr, "Bearer ")
	if !ok || s.auth == nil {
		writeErr(w, http.StatusUnauthorized, ErrUnauthorized)
		return false
	}
	claims, err := s.auth.Verify(tok)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return false
	}
	// 写操作需要 write 权限。
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodDelete:
		if !claims.HasScope("write") {
			writeErr(w, http.StatusForbidden, ErrUnauthorized)
			return false
		}
	}
	return true
}

func (s *Server) handlLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("解析登录请求: %w: %w", err, ErrBadRequest))
		return
	}
	got, ok := s.users[req.Username]
	if !ok || got != req.Password {
		writeErr(w, http.StatusUnauthorized, ErrUnauthorized)
		return
	}
	tok, err := s.auth.Sign(req.Username, []string{"read", "write"})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok, "user": req.Username})
}

// ------------------ tags ------------------

func (s *Server) handlTag(w http.ResponseWriter, r *http.Request, rest string) {
	name, version, ok := splitNameTag(rest)
	if !ok {
		writeErr(w, http.StatusBadRequest, ErrBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		rec, err := s.reg.Resolve(name, version)
		if err != nil {
			var status int
			if errors.Is(err, ErrNotFound) {
				status = http.StatusNotFound
			} else {
				status = http.StatusBadRequest
			}
			writeErr(w, status, err)
			return
		}
		writeJSON(w, http.StatusOK, rec)
	case http.MethodPost, http.MethodPut:
		var req struct {
			Digest string `json:"digest"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("解析 tag 请求: %w", err))
			return
		}
		if err := s.reg.Tag(name, version, req.Digest); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, ErrNotFound) {
				status = http.StatusNotFound
			}
			writeErr(w, status, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"name": name, "version": version, "digest": req.Digest})
	case http.MethodDelete:
		if err := s.reg.DeleteTag(name, version); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, ErrNotFound) {
				status = http.StatusNotFound
			}
			writeErr(w, status, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeErr(w, http.StatusMethodNotAllowed, ErrBadRequest)
	}
}

func splitNameTag(rest string) (name, version string, ok bool) {
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return "", "", false
	}
	i := strings.LastIndex(rest, "/")
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// ------------------ blobs ------------------

func (s *Server) handlBlob(w http.ResponseWriter, r *http.Request, digest string) {
	digest = strings.TrimPrefix(digest, "sha256:")
	if !validDigest(digest) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("摘要 %q 非法: %w", digest, ErrBadDigest))
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		rc, size, err := s.reg.GetBlob(digest)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, ErrNotFound) {
				status = http.StatusNotFound
			}
			writeErr(w, status, err)
			return
		}
		defer func() { _ = rc.Close() }()
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
		if r.Method == http.MethodHead {
			return
		}
		_, _ = io.Copy(w, rc)
	case http.MethodPost, http.MethodPut:
		// 从 form 字段或裸 body 上传。digest 在 URL 里，按内容校验。
		blob, err := s.reg.PutBlob(digest, r.Body, r.ContentLength)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, ErrDigestMismatch) {
				status = http.StatusConflict
			}
			writeErr(w, status, err)
			return
		}
		writeJSON(w, http.StatusCreated, blob)
	default:
		writeErr(w, http.StatusMethodNotAllowed, ErrBadRequest)
	}
}

// ------------------ search / catalog ------------------

func (s *Server) handlSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	results, err := s.reg.Search(q)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(results), "results": results})
}

func (s *Server) handlCatalog(w http.ResponseWriter, r *http.Request) {
	results, err := s.reg.Search("")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	names := map[string]bool{}
	for _, res := range results {
		names[res.Name] = true
	}
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	writeJSON(w, http.StatusOK, map[string]any{"repositories": out})
}

// ------------------ 工具 ------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	slog.Debug("hub http error", "code", code, "err", err)
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// randSecret 生成一个会话级随机密钥（登录/签名用）。
//
// **必须用 crypto/rand**：此前是 `fmt.Sprintf("licore-hub-%d-%d", os.Getpid(),
// time.Now().UnixNano())` —— 那不是随机数，而是"PID + 启动纳秒"，
// 两者都可被攻击者获取/枚举，据此可离线伪造出任意 admin 令牌
// （docs/security-audit-v2.md M-1）。
//
// 语义保持"会话级"：不落盘、进程重启即换新密钥，因此重启后旧令牌全部失效。
// 这是有意的取舍——持久化密钥需要落到磁盘上，反而引入新的保管问题；
// 需要跨重启稳定的部署应显式通过 WithAuth 注入。
//
// 返回 (secret, error)：**crypto/rand 失败时必须让启动失败**，不能回退到
// 时间戳之类的弱熵源。弱密钥是静默的灾难——服务照常起来，攻击者却能伪造
// 令牌，没有任何症状。
func randSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成 hub 会话密钥失败（crypto/rand 不可用）: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
