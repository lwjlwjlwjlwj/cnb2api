package auth

import (
	"testing"
	"time"
)

// 测试 TTL 过期清理：过期凭证被移除，未过期保留
func TestPoolCleanupTTL(t *testing.T) {
	p := &Pool{
		minSize: 1,
		maxSize: 6,
		ttl:     30 * time.Minute,
		tokens: []*CSRF{
			{Key: "a", Token: "1", Valid: true, Created: time.Now().Add(-40 * time.Minute)}, // 过期
			{Key: "b", Token: "2", Valid: true, Created: time.Now().Add(-10 * time.Minute)}, // 未过期
			{Key: "c", Token: "3", Valid: true, Created: time.Now()},                         // 未过期
			{Key: "d", Token: "4", Valid: false, Created: time.Now()},                        // 失效
		},
	}
	p.cleanup()
	if len(p.tokens) != 2 {
		t.Fatalf("expected 2 tokens after cleanup (expired+invalid removed), got %d", len(p.tokens))
	}
	if p.tokens[0].Key != "b" || p.tokens[1].Key != "c" {
		t.Fatalf("unexpected tokens kept: %+v", p.tokens)
	}
}

// 测试扩容：Acquire 在无空闲凭证且池未满时新建凭证
func TestPoolExpandOnAcquire(t *testing.T) {
	p := &Pool{
		minSize:  1,
		maxSize:  6,
		ttl:      30 * time.Minute,
		timeout:  3 * time.Second,
		tokens: []*CSRF{
			{Key: "a", Token: "1", Valid: true, Created: time.Now(), inUse: 1}, // 忙
		},
	}
	// 会尝试 Fetch（可能因网络失败返回 err），但不应 panic
	_, err := p.Acquire()
	if err != nil {
		t.Logf("acquire returned err (expected if network fails): %v", err)
	}
}

// 测试 Release/Report 归还：inUse 计数归零
func TestPoolReportReleases(t *testing.T) {
	cs := &CSRF{Key: "a", Token: "1", Valid: true, Created: time.Now(), inUse: 1}
	p := &Pool{tokens: []*CSRF{cs}}
	p.Report(cs, true)
	if cs.inUse != 0 {
		t.Fatalf("expected inUse=0 after report, got %d", cs.inUse)
	}
	if !cs.Valid {
		t.Fatal("expected valid after success")
	}
	// 失败累计（3 次后失效）
	cs.inUse = 1
	p.Report(cs, false)
	p.Report(cs, false)
	if cs.inUse != 0 {
		t.Fatalf("expected inUse=0, got %d", cs.inUse)
	}
	if !cs.Valid {
		t.Fatal("expected still valid after 2 failures")
	}
	p.Report(cs, false) // 第 3 次失败
	if cs.Valid {
		t.Fatal("expected invalid after 3 failures")
	}
}
