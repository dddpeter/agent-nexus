package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// handleHermesDiscover GET /api/adapters/hermes/discover —— 本机 hermes 网关自动探测
// （降低注册难度：hermes api_server 端口可配，扫 8640-8649 找到实际运行的网关并上报鉴权状态）。
// host 参数仅接受本机回环（防 SSRF；一期纳管场景即本机）。
func (s *Server) handleHermesDiscover(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Query().Get("host")
	if host == "" {
		host = "127.0.0.1"
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "host 仅支持本机回环地址")
		return
	}

	type candidate struct {
		Port         int    `json:"port"`
		BaseURL      string `json:"base_url"`
		Platform     string `json:"platform,omitempty"`
		Version      string `json:"version,omitempty"`
		AuthRequired bool   `json:"auth_required"`
	}
	found := make([]candidate, 0, 4)

	const (
		portLo, portHi = 8640, 8649
		probeTimeout   = 600 * time.Millisecond
	)
	client := &http.Client{Transport: probeTransport, Timeout: probeTimeout}

	var mu sync.Mutex
	var wg sync.WaitGroup
	for port := portLo; port <= portHi; port++ {
		wg.Add(1)
		go func(port int) {
			defer wg.Done()
			base := "http://" + host + ":" + strconv.Itoa(port)
			cctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
			defer cancel()

			// /health 公开端点：判定网关存在与身份
			req, err := http.NewRequestWithContext(cctx, http.MethodGet, base+"/health", nil)
			if err != nil {
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return
			}
			var h struct {
				Status   string `json:"status"`
				Platform string `json:"platform"`
				Version  string `json:"version"`
			}
			_ = json.Unmarshal(raw, &h)
			c := candidate{Port: port, BaseURL: base, Platform: h.Platform, Version: h.Version}
			if c.Platform == "" {
				c.Platform = "hermes-gateway" // 各 profile 门面（9900 等）无 platform 字段
			}

			// 鉴权判定：无凭证 GET /api/sessions —— 401 = 需要密钥；200 = 免鉴权（只读探测，无副作用）
			req2, err := http.NewRequestWithContext(cctx, http.MethodGet, base+"/api/sessions", nil)
			if err == nil {
				if resp2, err2 := client.Do(req2); err2 == nil {
					resp2.Body.Close()
					c.AuthRequired = resp2.StatusCode == http.StatusUnauthorized
				}
			}
			mu.Lock()
			found = append(found, c)
			mu.Unlock()
		}(port)
	}
	wg.Wait()

	// 端口序稳定输出
	for i := 0; i < len(found); i++ {
		for j := i + 1; j < len(found); j++ {
			if found[j].Port < found[i].Port {
				found[i], found[j] = found[j], found[i]
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scanned":    portHi - portLo + 1,
		"candidates": found,
	})
}

// probeTransport 直连：网关探测不经系统代理（本机地址走代理会失真）。
var probeTransport = &http.Transport{Proxy: nil}
