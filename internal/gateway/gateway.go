// Package gateway runs an optional, local LiteLLM proxy for providers that do
// not natively speak Claude's Messages API. It deliberately has no knowledge
// of the UI: it produces a small, private endpoint which driver.Options hands
// to Claude Code.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Provider is a hosted API LiteLLM knows how to translate into its local
// Anthropic-compatible endpoint.
type Provider string

const (
	OpenAI     Provider = "openai"
	Cerebras   Provider = "cerebras"
	Fireworks  Provider = "fireworks"
	OpenRouter Provider = "openrouter"
)

type preset struct {
	keyEnv       string
	modelPrefix  string
	defaultModel string
}

var presets = map[Provider]preset{
	OpenAI:     {keyEnv: "OPENAI_API_KEY", modelPrefix: "openai/", defaultModel: "gpt-4.1"},
	Cerebras:   {keyEnv: "CEREBRAS_API_KEY", modelPrefix: "cerebras/", defaultModel: "llama-3.3-70b"},
	Fireworks:  {keyEnv: "FIREWORKS_API_KEY", modelPrefix: "fireworks_ai/", defaultModel: "accounts/fireworks/models/llama-v3p3-70b-instruct"},
	OpenRouter: {keyEnv: "OPENROUTER_API_KEY", modelPrefix: "openrouter/", defaultModel: "anthropic/claude-sonnet-4"},
}

// DefaultModel makes a provider usable with no model flag. It is intentionally
// a stable, generally available tool-capable model rather than a moving alias.
func DefaultModel(provider string) (string, bool) {
	p, ok := presets[Provider(provider)]
	return p.defaultModel, ok
}

// Process is a running LiteLLM proxy plus the private values Claude needs to
// reach it. Upstream API keys never appear in Env or Claude's environment.
type Process struct {
	URL         string
	Token       string
	UpstreamKey string
	cmd         *exec.Cmd
}

type configFile struct {
	ModelList []modelEntry `json:"model_list"`
	General   general      `json:"general_settings"`
}
type modelEntry struct {
	ModelName string      `json:"model_name"`
	Params    modelParams `json:"litellm_params"`
}
type modelParams struct {
	Model  string `json:"model"`
	APIKey string `json:"api_key"`
}
type general struct {
	MasterKey string `json:"master_key"`
}

// Start launches LiteLLM on a random loopback port. LiteLLM must already be
// installed (pipx install 'litellm[proxy]'); acy never downloads executable
// code or asks for credentials behind the user's back.
func Start(ctx context.Context, dir, bin, provider string, models ...string) (*Process, error) {
	p, ok := presets[Provider(provider)]
	if !ok {
		return nil, fmt.Errorf("unsupported LiteLLM provider %q (want openai, cerebras, fireworks, or openrouter)", provider)
	}
	if os.Getenv(p.keyEnv) == "" {
		return nil, fmt.Errorf("%s is required for --provider %s", p.keyEnv, provider)
	}
	if bin == "" {
		bin = "litellm"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("find LiteLLM binary %q: %w; install it with: pipx install 'litellm[proxy]'", bin, err)
	}
	modelList := make([]string, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		if model != "" && !seen[model] {
			seen[model] = true
			modelList = append(modelList, model)
		}
	}
	if len(modelList) == 0 {
		modelList = append(modelList, p.defaultModel)
	}
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	entries := make([]modelEntry, 0, len(modelList))
	for _, model := range modelList {
		entries = append(entries, modelEntry{
			ModelName: model,
			Params:    modelParams{Model: p.modelPrefix + model, APIKey: "os.environ/" + p.keyEnv},
		})
	}
	config := configFile{ModelList: entries, General: general{MasterKey: token}}
	b, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("encode LiteLLM config: %w", err)
	}
	path := filepath.Join(dir, "litellm.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, fmt.Errorf("write LiteLLM config: %w", err)
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, "--config", path, "--host", "127.0.0.1", "--port", port)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start LiteLLM: %w", err)
	}
	result := &Process{URL: "http://127.0.0.1:" + port, Token: token, UpstreamKey: p.keyEnv, cmd: cmd}
	if err := result.waitReady(ctx); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	return result, nil
}

func (p *Process) Close() {
	if p != nil && p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		_ = p.cmd.Wait()
	}
}

func (p *Process) waitReady(ctx context.Context) error {
	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	client := &http.Client{Timeout: time.Second}
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.URL+"/health/liveliness", nil)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("LiteLLM did not become ready within 12 seconds")
		case <-tick.C:
		}
	}
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate gateway token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("reserve LiteLLM port: %w", err)
	}
	defer func() { _ = l.Close() }()
	return strings.TrimPrefix(l.Addr().String(), "127.0.0.1:"), nil
}
