package artifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Pushpavel/fired/pkg/api"
	pa "github.com/Pushpavel/fired/pkg/artifacts"
)

// broker asks your backend for the credentials of one run. The backend
// maps the artifact (or its quota key) to an owner and returns whatever
// that owner's connected accounts provide right now, typically short-lived
// tokens scoped to the run.
type broker struct {
	cfg  BrokerConfig
	http *http.Client
}

func newBroker(cfg BrokerConfig) *broker {
	return &broker{cfg: cfg, http: &http.Client{Timeout: time.Duration(cfg.Timeout)}}
}

func (b *broker) credentials(ctx context.Context, req pa.CredentialsRequest) (*pa.CredentialsResponse, error) {
	out := &pa.CredentialsResponse{}
	if b.cfg.URL == "" {
		return out, nil
	}
	body, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, "POST", b.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	if b.cfg.Token != "" {
		hr.Header.Set("Authorization", "Bearer "+b.cfg.Token)
	}
	resp, err := b.http.Do(hr)
	if err != nil {
		return nil, &brokerDown{err}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout {
		return nil, &brokerDown{fmt.Errorf("%d: %s", resp.StatusCode, strings.TrimSpace(string(data)))}
	}
	if resp.StatusCode >= 300 {
		var e api.Error
		if json.Unmarshal(data, &e) == nil && e.Err.Code != "" {
			return nil, fmt.Errorf("%d %s: %s", resp.StatusCode, e.Err.Code, e.Err.Message)
		}
		return nil, fmt.Errorf("%d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, fmt.Errorf("decode response: %v", err)
	}
	return out, nil
}

// brokerDown is the broker being unreachable or unavailable: runs wait for
// it, as they do for fired. A refusal (any other error status) fails the
// run.
type brokerDown struct{ err error }

func (e *brokerDown) Error() string { return "broker unavailable: " + e.err.Error() }
func (e *brokerDown) Unwrap() error { return e.err }
