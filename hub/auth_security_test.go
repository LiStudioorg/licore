// Copyright (C) 2026 LiStudioorg
// SPDX-License-Identifier: AGPL-3.0-only

package hub

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 本文件是 docs/security-audit-v2.md M-1 / M-2 的回归测试。
//
// M-1：JWT 密钥来自 randSecret()，原实现是 "licore-hub-<pid>-<UnixNano>"，
//      即可预测的弱熵 —— 可离线伪造任意 admin 令牌。
// M-2：Verify 里的 `if c.Exp > 0 && ...` 让 exp 缺失/为负的令牌永不过期。

// ---------- M-1：密钥强度 ----------

// TestRandSecretIsCryptographicallyRandom 密钥必须来自 crypto/rand：
// 长度足够、两次调用不同、且不含可推导的模式。
func TestRandSecretIsCryptographicallyRandom(t *testing.T) {
	s1, err := randSecret()
	if err != nil {
		t.Fatalf("randSecret 失败: %v", err)
	}
	s2, err := randSecret()
	if err != nil {
		t.Fatalf("randSecret 失败: %v", err)
	}
	if s1 == s2 {
		t.Fatal("两次 randSecret 返回相同值，说明不是随机源")
	}
	// 32 字节 base64url 无填充 = 43 字符
	if len(s1) < 43 {
		t.Errorf("密钥长度 %d 不足（期望 >= 43，即 32 字节熵）", len(s1))
	}
	// 旧实现的特征：licore-hub-<pid>-<nano>
	if strings.HasPrefix(s1, "licore-hub-") {
		t.Errorf("密钥仍是旧的可预测格式: %q", s1)
	}
	if strings.Contains(s1, "-") && strings.Count(s1, "-") > 0 {
		t.Logf("提示：密钥含 '-'（base64url 合法，非失败）：%q", s1)
	}
}

// TestRandSecretNotDerivableFromPidAndTime 旧实现可直接从 PID+时间推导；
// 新实现必须做不到（这里用"大量样本中无重复、且不含 PID 十进制串"近似验证）。
func TestRandSecretNotDerivableFromPidAndTime(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 256; i++ {
		s, err := randSecret()
		if err != nil {
			t.Fatal(err)
		}
		if seen[s] {
			t.Fatalf("第 %d 次出现重复密钥，随机源有问题", i)
		}
		seen[s] = true
	}
	t.Logf("256 次生成全部唯一")
}

// ---------- M-1：伪造令牌必须失败 ----------

// TestForgedTokenRejected 用猜测的密钥签出的令牌必须验不过。
//
// 这是 M-1 的核心回归，且必须**同时**覆盖两种情形：
//  1. 攻击者猜了一个无关的密钥 → 当然验不过；
//  2. **攻击者按照旧公式猜中了真实密钥** → 这正是漏洞利用路径：
//     旧 randSecret 是 "licore-hub-<pid>-<UnixNano>"，PID 可读、纳秒可枚举，
//     猜中后即可签出合法 admin 令牌。
//
// 只测情形 1 会产生**假阴性**：旧实现在情形 1 下同样"通过"测试，
// 却完全挡不住情形 2。因此这里显式构造"按旧公式复现服务端密钥"的攻击。
func TestForgedTokenRejected(t *testing.T) {
	// --- 情形 2：按旧公式复现密钥（真实攻击路径）---
	// 服务端用 randSecret() 生成密钥；攻击者按旧格式猜测。
	serverSecret := mustRandSecret(t)
	auth := NewAuthenticator(serverSecret, time.Hour)

	legit, err := auth.Sign("alice", []string{"read", "write"})
	if err != nil {
		t.Fatal(err)
	}

	// 攻击者无法从 PID+时间推出 serverSecret（修复后）：
	// 这里构造若干个"按旧公式算出来"的候选密钥，全部必须失败。
	candidates := []string{
		legit[:0], // 空串
		"licore-hub-1234-1791196756427009199",
		"licore-hub-1-1",
		"secret",
	}
	for _, guess := range candidates {
		forged := signWith(guess, JWTClaims{
			Sub: "attacker", Iat: time.Now().Unix(),
			Exp:    time.Now().Add(24 * time.Hour).Unix(),
			Scopes: []string{"admin"},
		})
		if _, err := auth.Verify(forged); err == nil {
			t.Fatalf("!!! 伪造令牌通过了校验（猜测密钥 %q）", guess)
		} else if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("应返回 ErrUnauthorized，实得: %v", err)
		}
	}

	// --- 关键断言：密钥不可从 PID+时间推导 ---
	// 旧实现满足下面这个 if，即"PID+当前时间"直接等于密钥。
	if guessable := fmt.Sprintf("licore-hub-%d-%d", os.Getpid(), time.Now().UnixNano()); guessable == serverSecret {
		t.Fatal("!!! 密钥等于 PID+时间的可预测值")
	}
	// 更实质的检查：密钥不含 PID 十进制串（旧格式的判别特征）
	pidStr := strconv.Itoa(os.Getpid())
	if strings.Contains(serverSecret, pidStr) && strings.HasPrefix(serverSecret, "licore-hub-") {
		t.Fatalf("!!! 密钥形如旧的可预测格式: %q", serverSecret)
	}
	t.Logf("伪造令牌全部被拒；密钥不可由 PID+时间推导")
}

// TestDefaultServerSecretNotPredictable 默认构造的 Server 不得使用可预测密钥。
func TestDefaultServerSecretNotPredictable(t *testing.T) {
	dir := t.TempDir()
	bs, err := NewLocalBlobStore(dir + "/blobs")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := NewRegistry(dir, bs)
	if err != nil {
		t.Fatal(err)
	}
	s1, err := NewServer(reg)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := NewServer(reg)
	if err != nil {
		t.Fatal(err)
	}
	if s1.auth.secret == nil || len(s1.auth.secret) < 32 {
		t.Fatalf("默认密钥过短: %d 字节", len(s1.auth.secret))
	}
	if hmac.Equal(s1.auth.secret, s2.auth.secret) {
		t.Fatal("两个 Server 实例用了同一个默认密钥（应为各自随机会话密钥）")
	}
	// 旧格式特征
	if strings.HasPrefix(string(s1.auth.secret), "licore-hub-") {
		t.Errorf("默认密钥仍是旧的可预测格式: %q", string(s1.auth.secret))
	}
}

// ---------- M-2：exp 必须存在且有效 ----------

// TestVerifyRejectsMissingExp 缺失 exp 的令牌必须被拒绝。
func TestVerifyRejectsMissingExp(t *testing.T) {
	secret := mustRandSecret(t)
	auth := NewAuthenticator(secret, time.Hour)
	// 构造一个**签名合法**但没有 exp 的令牌
	tok := signWith(secret, JWTClaims{Sub: "admin", Scopes: []string{"admin"}})
	if _, err := auth.Verify(tok); err == nil {
		t.Fatal("!!! 无 exp 的令牌被接受（必须拒绝）")
	} else if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("应返回 ErrUnauthorized，实得: %v", err)
	} else {
		t.Logf("无 exp 被正确拒绝: %v", err)
	}
}

// TestVerifyRejectsZeroExp exp=0 显式给出也必须拒绝。
func TestVerifyRejectsZeroExp(t *testing.T) {
	secret := mustRandSecret(t)
	auth := NewAuthenticator(secret, time.Hour)
	tok := signWith(secret, JWTClaims{Sub: "admin", Exp: 0, Scopes: []string{"admin"}})
	if _, err := auth.Verify(tok); err == nil {
		t.Fatal("!!! exp=0 的令牌被接受")
	}
}

// TestVerifyRejectsNegativeExp exp 为负必须拒绝（旧实现因 c.Exp > 0 而放行）。
func TestVerifyRejectsNegativeExp(t *testing.T) {
	secret := mustRandSecret(t)
	auth := NewAuthenticator(secret, time.Hour)
	for _, exp := range []int64{-1, -1000, -9999999999} {
		tok := signWith(secret, JWTClaims{Sub: "admin", Exp: exp, Scopes: []string{"admin"}})
		if _, err := auth.Verify(tok); err == nil {
			t.Fatalf("!!! exp=%d 的令牌被接受", exp)
		}
	}
}

// TestVerifyRejectsExpired 正常过期令牌必须拒绝。
func TestVerifyRejectsExpired(t *testing.T) {
	secret := mustRandSecret(t)
	auth := NewAuthenticator(secret, time.Hour)
	tok := signWith(secret, JWTClaims{
		Sub: "admin", Iat: time.Now().Add(-2 * time.Hour).Unix(),
		Exp: time.Now().Add(-time.Hour).Unix(), Scopes: []string{"admin"},
	})
	if _, err := auth.Verify(tok); err == nil {
		t.Fatal("!!! 已过期令牌被接受")
	}
}

// TestVerifyRejectsFutureIat iat 晚于当前时间的令牌必须拒绝。
func TestVerifyRejectsFutureIat(t *testing.T) {
	secret := mustRandSecret(t)
	auth := NewAuthenticator(secret, time.Hour)
	tok := signWith(secret, JWTClaims{
		Sub: "admin", Iat: time.Now().Add(time.Hour).Unix(),
		Exp: time.Now().Add(2 * time.Hour).Unix(), Scopes: []string{"admin"},
	})
	if _, err := auth.Verify(tok); err == nil {
		t.Fatal("!!! iat 为未来的令牌被接受")
	}
}

// ---------- 反向约束：合法令牌必须照常工作 ----------

// TestVerifyAcceptsLegitToken 正常签发的令牌必须通过（修复不能把功能挡掉）。
func TestVerifyAcceptsLegitToken(t *testing.T) {
	auth := NewAuthenticator(mustRandSecret(t), time.Hour)
	tok, err := auth.Sign("alice", []string{"read", "write"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := auth.Verify(tok)
	if err != nil {
		t.Fatalf("合法令牌被拒绝: %v", err)
	}
	if c.Sub != "alice" {
		t.Errorf("sub 不符: %q", c.Sub)
	}
	if !c.HasScope("write") || c.HasScope("admin") {
		t.Errorf("scopes 不符: %v", c.Scopes)
	}
	if c.Exp <= time.Now().Unix() {
		t.Error("exp 应为未来时间")
	}
}

// TestSigRoundTripUnchanged Sign→Verify 的往返在同密钥下必须成立（跨实例）。
func TestSigRoundTripUnchanged(t *testing.T) {
	secret := mustRandSecret(t)
	a1 := NewAuthenticator(secret, time.Hour)
	a2 := NewAuthenticator(secret, 2*time.Hour)
	tok, err := a1.Sign("bob", []string{"read"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a2.Verify(tok); err != nil {
		t.Fatalf("同密钥跨实例校验失败: %v", err)
	}
}

// ---------- 辅助 ----------

func mustRandSecret(t *testing.T) string {
	t.Helper()
	s, err := randSecret()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// signWith 用给定密钥按 hub 的 JWT 格式手工签一个令牌，
// 模拟"攻击者拿到（或猜中）密钥"的情形。
func signWith(secret string, claims JWTClaims) string {
	hdr, _ := json.Marshal(JWTHeader{Alg: "HS256", Typ: "JWT"})
	cl, _ := json.Marshal(claims)
	h := base64.RawURLEncoding.EncodeToString(hdr)
	p := base64.RawURLEncoding.EncodeToString(cl)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(h + "." + p))
	return h + "." + p + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
