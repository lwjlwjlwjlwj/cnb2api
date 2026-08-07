// Package auth 负责 CNB CSRF token 的获取与生命周期管理。
//
// 鉴权机制（逆向自 cnb.cool 前端 _app.js）：
//  1. GET https://cnb.cool/ 首页，响应 Set-Cookie 里带 `csrfkey`（HTTPOnly）
//  2. 首页 HTML 里内嵌 <script id="cnb-csrftoken-script">window.csrftoken="xxx"</script>
//  3. 请求 /ai/chat/completions 时，需同时携带：
//     - Cookie: csrfkey=<csrfkey>
//     - Header:  Csrftoken: <csrftoken>
//  CSRF 校验比对 cookie 中的 csrfkey 与 header 中的 csrftoken 是否配对。
//
// 重要：每次 Fetch 必须使用**独立会话**（独立 cookie jar），否则会拿到相同的
// csrfkey（会话 cookie 由服务端按会话签发）。Pool 并发获取时天然得到不同凭证。
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"strings"
	"sync"
	"time"
)

// CSRF 是一次完整的鉴权凭证：csrfkey(cookie) + csrftoken(header) 必须配对使用。
type CSRF struct {
	Key     string    `json:"csrfkey"`  // cookie 值
	Token   string    `json:"token"`    // header 值
	Created time.Time `json:"created"`  // 获取时间
	Valid   bool      `json:"valid"`    // 是否通过健康检查
	Checked time.Time `json:"checked"`  // 最近一次健康检查时间
	ErrCnt  int       `json:"err_cnt"`  // 连续错误次数
	inUse   int       `json:"-"`        // 当前正在使用的请求数（并发占位）
}

var (
	csrfTokenRe = regexp.MustCompile(`window\.csrftoken="([0-9a-f]{32,64})"`)
	csrfKeyRe   = regexp.MustCompile(`(?:^|;)\s*csrfkey=([0-9a-f]{32,64})`)
	baseURL     = "https://cnb.cool"
	chatPath    = "/ai/chat/completions"
)

// ErrInvalidToken 表示 token 获取或校验失败。
var ErrInvalidToken = errors.New("cnb: invalid csrf token")

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"

// newSessionClient 创建带全新 cookie jar 的客户端（每次调用都是独立会话）。
func newSessionClient(timeout time.Duration) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Timeout: timeout,
		Jar:     jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// 跟随重定向（最多 10 次），保留 cookie
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

// Fetch 使用**全新独立会话**访问首页，返回 csrfkey + csrftoken 配对。
// 每次调用都会建立新会话，因此并发调用会得到不同凭证。
func Fetch(timeout time.Duration) (*CSRF, error) {
	hc := newSessionClient(timeout)
	defer hc.CloseIdleConnections()

	req, err := http.NewRequest(http.MethodGet, baseURL+"/", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch home: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8MB 足够
	if err != nil {
		return nil, fmt.Errorf("read home body: %w", err)
	}

	csrf := &CSRF{Created: time.Now()}

	// 1. 从 HTML 提取 csrftoken
	m := csrfTokenRe.FindSubmatch(body)
	if m == nil {
		return nil, fmt.Errorf("%w: csrftoken not found in home page", ErrInvalidToken)
	}
	csrf.Token = string(m[1])

	// 2. 从响应 Set-Cookie / cookie jar 提取 csrfkey
	//    优先从 jar 读（自动管理）；若 jar 里没有，从原始 Set-Cookie 头提取
	found := false
	u, _ := req.URL.Parse(baseURL + "/")
	for _, ck := range hc.Jar.Cookies(u) {
		if ck.Name == "csrfkey" {
			csrf.Key = ck.Value
			found = true
			break
		}
	}
	if !found {
		for _, sc := range resp.Header.Values("Set-Cookie") {
			if m := csrfKeyRe.FindStringSubmatch(sc); m != nil {
				csrf.Key = m[1]
				found = true
				break
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: csrfkey cookie not found", ErrInvalidToken)
	}

	csrf.Valid = true
	csrf.Checked = time.Now()
	return csrf, nil
}

// Verify 用一次最小请求验证凭证是否仍有效（200 = 有效）。
// 使用独立会话，仅携带该凭证的 csrfkey，避免串扰。
func Verify(cs *CSRF, timeout time.Duration) bool {
	if cs == nil || cs.Key == "" || cs.Token == "" {
		return false
	}
	hc := newSessionClient(timeout)
	defer hc.CloseIdleConnections()

	body := `{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"ping"}],"tools":[],"maxTokens":1}`
	req, err := http.NewRequest(http.MethodPost, baseURL+chatPath, strings.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Csrftoken", cs.Token)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/event-stream, application/json")
	req.Header.Set("Origin", baseURL)
	req.Header.Set("Referer", baseURL+"/")
	// 带上该凭证配对的 csrfkey cookie
	u, _ := req.URL.Parse(baseURL + "/")
	hc.Jar.SetCookies(u, []*http.Cookie{{Name: "csrfkey", Value: cs.Key, Path: "/"}})

	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode == http.StatusOK
}

// Pool 是 CSRF 凭证池：自动获取、弹性扩容、按 TTL 过期清理、健康检查、轮转分配、并发安全。
type Pool struct {
	mu      sync.Mutex
	timeout time.Duration
	tokens  []*CSRF
	minSize int
	maxSize int
	ttl     time.Duration
	stop    chan struct{}
	next    int
}

// PoolConfig 控制凭证池行为。
type PoolConfig struct {
	MinSize int           // 池最小凭证数
	MaxSize int           // 池最大凭证数（高负载时扩容上限）
	TTL     time.Duration // 凭证最大寿命，到期后清理（自动销毁）
	Timeout time.Duration // 单个 HTTP 请求超时
}

// DefaultPoolConfig 返回合理默认值。
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{MinSize: 2, MaxSize: 8, TTL: 30 * time.Minute, Timeout: 15 * time.Second}
}

// NewPool 创建凭证池并启动后台维护协程。
func NewPool(cfg PoolConfig) (*Pool, error) {
	if cfg.MaxSize < 1 {
		cfg.MaxSize = 1
	}
	if cfg.MinSize < 1 {
		cfg.MinSize = 1
	}
	if cfg.MinSize > cfg.MaxSize {
		cfg.MinSize = cfg.MaxSize
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Minute
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	p := &Pool{
		timeout: cfg.Timeout,
		minSize: cfg.MinSize,
		maxSize: cfg.MaxSize,
		ttl:     cfg.TTL,
		stop:    make(chan struct{}),
	}

	// 初始填充：**并发**获取，确保拿到多个不同凭证
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < cfg.MinSize; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cs, err := Fetch(cfg.Timeout)
			if err != nil {
				return
			}
			mu.Lock()
			p.tokens = append(p.tokens, cs)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if p.Count() == 0 {
		return nil, errors.New("cnb: failed to acquire any csrf token")
	}
	go p.maintain()
	return p, nil
}

// Acquire 分配一个凭证。
// 策略：优先分配空闲(inUse==0)凭证；若全部忙但池未满 maxSize，则**弹性扩容**新凭证；
// 池已满则退而分配 inUse 最少的凭证（允许并发复用）。
func (p *Pool) Acquire() (*CSRF, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 1. 优先找空闲可用凭证（inUse==0）
	for i := 0; i < len(p.tokens); i++ {
		idx := (p.next + i) % len(p.tokens)
		t := p.tokens[idx]
		if t.Valid && t.inUse == 0 && time.Since(t.Created) < p.ttl && t.ErrCnt < 3 {
			t.inUse++
			p.next = (idx + 1) % len(p.tokens)
			return t, nil
		}
	}

	// 2. 全部忙，但池未满 → 弹性扩容（新建凭证）
	if len(p.tokens) < p.maxSize {
		cs, err := Fetch(p.timeout)
		if err == nil {
			cs.inUse = 1
			p.tokens = append(p.tokens, cs)
			return cs, nil
		}
		// 扩容失败，降级到下面的复用
	}

	// 3. 池已满或扩容失败 → 分配 inUse 最少的凭证（并发复用）
	var best *CSRF
	bestUse := int(^uint(0) >> 1)
	for _, t := range p.tokens {
		if t.Valid && time.Since(t.Created) < p.ttl && t.ErrCnt < 3 && t.inUse < bestUse {
			best = t
			bestUse = t.inUse
		}
	}
	if best != nil {
		best.inUse++
		return best, nil
	}
	return nil, errors.New("cnb: no valid csrf token available")
}

// Release 归还凭证（请求结束后调用，减少 inUse 计数）。
func (p *Pool) Release(cs *CSRF) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cs == nil {
		return
	}
	if cs.inUse > 0 {
		cs.inUse--
	}
}

// Report 上报一次请求结果：成功(ok=true) 或失败(ok=false)。
// 成功/失败后自动归还凭证（减少 inUse 计数）。
func (p *Pool) Report(cs *CSRF, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cs == nil {
		return
	}
	if ok {
		cs.ErrCnt = 0
		cs.Valid = true
		cs.Checked = time.Now()
	} else {
		cs.ErrCnt++
		if cs.ErrCnt >= 3 {
			cs.Valid = false
		}
	}
	// 归还凭证（减少 inUse 计数）
	if cs.inUse > 0 {
		cs.inUse--
	}
}

// Count 返回当前池内凭证数。
func (p *Pool) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.tokens)
}

// Stats 返回池状态。
func (p *Pool) Stats() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]map[string]any, 0, len(p.tokens))
	for _, t := range p.tokens {
		out = append(out, map[string]any{
			"csrfkey":  t.Key,
			"token":    t.Token[:8] + "...", // 隐私裁剪
			"created":  t.Created.Format(time.RFC3339),
			"valid":    t.Valid,
			"err_cnt":  t.ErrCnt,
			"ttl_left": (p.ttl - time.Since(t.Created)).Round(time.Second).String(),
		})
	}
	return out
}

// refill 尝试新增一个凭证（独立会话）；若池已满则先移除最旧失效凭证。
func (p *Pool) refill() error {
	cs, err := Fetch(p.timeout)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.tokens) >= p.maxSize {
		// 淘汰失效的；没有失效则淘汰最老的
		for i, t := range p.tokens {
			if !t.Valid {
				p.tokens = append(p.tokens[:i], p.tokens[i+1:]...)
				goto added
			}
		}
		p.tokens = append(p.tokens[:0], p.tokens[1:]...) // 淘汰最老
	added:
		if len(p.tokens) >= p.maxSize {
			return errors.New("cnb: pool full")
		}
	}
	p.tokens = append(p.tokens, cs)
	return nil
}

// maintain 后台维护（每分钟）：
//  1. 清理过期/失效凭证（TTL 到期自动销毁）
//  2. 池低于 minSize → 补充
func (p *Pool) maintain() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.cleanup()
			// 补充到 minSize
			for p.Count() < p.minSize {
				if err := p.refill(); err != nil {
					break
				}
			}
		}
	}
}

// cleanup 移除过期或失效凭证。
func (p *Pool) cleanup() {
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := p.tokens[:0]
	for _, t := range p.tokens {
		if t.Valid && time.Since(t.Created) < p.ttl {
			kept = append(kept, t)
		}
	}
	p.tokens = kept
}

// Close 停止后台维护。
func (p *Pool) Close() {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
}

// MarshalJSON 便于调试输出。
func (c *CSRF) MarshalJSON() ([]byte, error) {
	type alias CSRF
	return json.Marshal((*alias)(c))
}
