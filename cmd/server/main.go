// cnb2api - CNB NPC chat 接口的 OpenAI 兼容反向代理。
//
// 用法:
//
//	cnb2api -config config.json
//
// 或环境变量:
//
//	CNB2API_LISTEN  监听地址 (默认 :7863)
//	CNB2API_API_KEY API 鉴权 key (默认空=不鉴权)
//	CNB2API_MODEL   模型名 (默认 deepseek-v4-flash)
//	CNB2API_POOL_MIN 凭证池最小数 (默认 2)
//	CNB2API_POOL_MAX 凭证池最大数 (默认 8)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"cnb2api/internal/auth"
	"cnb2api/internal/server"
)

type Config struct {
	Listen  string   `json:"listen"`
	APIKey  string   `json:"api_key"`
	Model   string   `json:"model"`
	Models  []string `json:"models"` // 支持的模型列表（默认 [model]）
	PoolMin int      `json:"pool_min"`
	PoolMax int      `json:"pool_max"`
	TTLMin  int      `json:"ttl_minutes"`
}

func defaultConfig() Config {
	return Config{
		Listen:  ":7863",
		APIKey:  "",
		Model:   "deepseek-v4-flash",
		Models:  []string{"deepseek-v4-flash", "deepseek-v4-pro"},
		PoolMin: 2,
		PoolMax: 8,
		TTLMin:  30,
	}
}

func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, err
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, err
		}
	}
	// 环境变量覆盖
	if v := os.Getenv("CNB2API_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("CNB2API_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("CNB2API_MODEL"); v != "" {
		cfg.Model = v
	}
	if v := os.Getenv("CNB2API_POOL_MIN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.PoolMin = n
		}
	}
	if v := os.Getenv("CNB2API_POOL_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.PoolMax = n
		}
	}
	if v := os.Getenv("CNB2API_TTL_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.TTLMin = n
		}
	}
	return cfg, nil
}

func main() {
	configPath := flag.String("config", "", "config file path (JSON)")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	log.Printf("cnb2api starting: listen=%s model=%s pool=[%d,%d] ttl=%dm",
		cfg.Listen, cfg.Model, cfg.PoolMin, cfg.PoolMax, cfg.TTLMin)

	poolCfg := auth.PoolConfig{
		MinSize: cfg.PoolMin,
		MaxSize: cfg.PoolMax,
		TTL:     time.Duration(cfg.TTLMin) * time.Minute,
	}
	pool, err := auth.NewPool(poolCfg)
	if err != nil {
		log.Fatalf("init csrf pool: %v", err)
	}
	defer pool.Close()
	log.Printf("csrf pool ready: %d token(s)", pool.Count())

	srv := server.New(pool, cfg.APIKey, cfg.Model, cfg.Models, 180*time.Second)
	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("listening on %s", cfg.Listen)
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatalf("server: %v", err)
	}
	_ = fmt.Sprintf
}
