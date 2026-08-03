package tagger

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/config"
)

// RemoteTaggerList fetches the model list a paired A-side currently
// advertises through its remote-status endpoint. Servers running an
// older protocol (no taggers field) yield an empty list, which callers
// treat as "every model on the server". Callers should pass a context
// with a deadline; the HTTP client applies its own timeout on top.
func RemoteTaggerList(ctx context.Context, cfg *config.Config) ([]RemoteTaggerInfo, error) {
	url := strings.TrimRight(cfg.Tagger.RemoteClient.URL, "/") + "/api/v1/tagger/remote-status"
	httpReq, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if cfg.Tagger.RemoteClient.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+cfg.Tagger.RemoteClient.Token)
	}
	client := &http.Client{Timeout: remoteClientTimeout}
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("remote tagger status: %w", err)
	}
	defer httpResp.Body.Close()
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("remote tagger status returned %d: %s", httpResp.StatusCode, string(body))
	}
	var out struct {
		Taggers []RemoteTaggerInfo `json:"taggers"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode status response: %w", err)
	}
	return out.Taggers, nil
}

// RemoteTaggerListTimeout bounds the whole list fetch so the auto-tag
// dialog's model dropdown never stalls a page load behind an
// unreachable server. The per-request client timeout is shorter than
// this; the context deadline is a final guard.
const RemoteTaggerListTimeout = 10 * time.Second
