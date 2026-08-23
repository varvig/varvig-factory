package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/varvig/varvig-factory/cell"
)

// HTTPRuntime drives a model over HTTP using the chat-completions request
// shape that ollama, vLLM, llama.cpp's server and hosted APIs all implement.
// One adapter therefore covers three of the four rows in the §4 table, and the
// difference between a Micro cell and a Mini cell is an endpoint and a model
// name in a config file.
//
// The request envelope is written by hand with encoding/json rather than a
// vendor SDK: this module has no third-party dependencies, and more to the
// point, a vendor SDK in the adapter would be the vendor leaking through the
// seam it exists to hide.
type HTTPRuntime struct {
	// Endpoint is the chat-completions URL, e.g.
	// http://127.0.0.1:11434/v1/chat/completions.
	Endpoint string
	// VersionURL is probed once to measure the serving runtime's version, e.g.
	// http://127.0.0.1:11434/api/version. It is REQUIRED: without it the
	// adapter cannot describe itself and returns ErrIndescribable, because a
	// version read from configuration is a claim about the server rather than a
	// measurement of it.
	VersionURL string
	// Model is the model name to request.
	Model string
	// ModelVersion is the model's build/quantization label, e.g. "32b-q4". It is
	// configuration because most servers do not report it, and it is recorded
	// verbatim so two cells running different quantizations of the same weights
	// are visibly not the same ground.
	ModelVersion string
	// Params are the sampling parameters, recorded in the environment.
	Params Params
	// AuthHeader and AuthValue carry credentials for a hosted endpoint, e.g.
	// "Authorization" / "Bearer sk-…". They are never recorded in any
	// environment, evidence, or log.
	AuthHeader, AuthValue string
	// System is an optional system prompt.
	System string
	// Client defaults to a client with a generous timeout: a large local model
	// on CPU is slow, and a timeout tuned for a hosted API would make Micro
	// look broken.
	Client *http.Client

	once     sync.Once
	fragment cell.Fragment
	fragErr  error
}

// Name implements Runtime.
func (h *HTTPRuntime) Name() string { return "http" }

func (h *HTTPRuntime) client() *http.Client {
	if h.Client != nil {
		return h.Client
	}
	return &http.Client{Timeout: 30 * time.Minute}
}

// Fragment implements Runtime by probing VersionURL once and caching the
// result. Caching is what makes it deterministic within a run; probing is what
// makes it a measurement rather than a claim.
//
// The version token is extracted from the probe response's "version" field when
// there is one, and otherwise is the canonical hash of whatever the server did
// report. The fallback matters: it keeps the adapter vendor-neutral — an
// endpoint that reports its identity in some other shape still yields a stable,
// comparable token instead of an adapter that refuses to work with it.
func (h *HTTPRuntime) Fragment(ctx context.Context) (cell.Fragment, error) {
	h.once.Do(func() { h.fragment, h.fragErr = h.probe(ctx) })
	return h.fragment, h.fragErr
}

func (h *HTTPRuntime) probe(ctx context.Context) (cell.Fragment, error) {
	if h.Model == "" {
		return cell.Fragment{}, fmt.Errorf("inference: http runtime has no model configured")
	}
	if h.VersionURL == "" {
		return cell.Fragment{}, fmt.Errorf("%w: no version_url configured for endpoint %s", ErrIndescribable, h.Endpoint)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.VersionURL, nil)
	if err != nil {
		return cell.Fragment{}, err
	}
	h.auth(req)
	resp, err := h.client().Do(req)
	if err != nil {
		return cell.Fragment{}, fmt.Errorf("%w: probing %s: %v", ErrIndescribable, h.VersionURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return cell.Fragment{}, fmt.Errorf("%w: reading %s: %v", ErrIndescribable, h.VersionURL, err)
	}
	if resp.StatusCode/100 != 2 {
		return cell.Fragment{}, fmt.Errorf("%w: %s returned %s", ErrIndescribable, h.VersionURL, resp.Status)
	}

	version := extractVersion(body)
	if version == "" {
		// Nothing recognisable: fall back to a digest of the exact bytes the
		// server reported. Opaque to a human, but stable and comparable, which
		// is all the environment hash needs.
		hash, herr := cell.CanonicalHash(string(body))
		if herr != nil {
			return cell.Fragment{}, herr
		}
		version = hash
	}
	return cell.Fragment{
		Toolchains: map[string]string{"inference-runtime": version},
		Model: &cell.EnvModel{
			ID:      h.Model,
			Version: h.ModelVersion,
			Params:  h.Params.String(),
		},
	}, nil
}

// extractVersion pulls a version string out of a probe response. It accepts a
// top-level "version" field, which is what ollama and vLLM both report, and
// gives up quietly otherwise so the caller can fall back to a digest.
func extractVersion(body []byte) string {
	var probe struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && probe.Version != "" {
		return probe.Version
	}
	return ""
}

func (h *HTTPRuntime) auth(req *http.Request) {
	if h.AuthHeader != "" && h.AuthValue != "" {
		req.Header.Set(h.AuthHeader, h.AuthValue)
	}
}

// chatRequest is the request envelope. Written out explicitly so the wire shape
// is reviewable in one place.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature *float64      `json:"temperature,omitempty"`
	TopP        *float64      `json:"top_p,omitempty"`
	Seed        *int64        `json:"seed,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Stream      bool          `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Generate implements Runtime.
func (h *HTTPRuntime) Generate(ctx context.Context, r Request) (Response, error) {
	if h.Endpoint == "" {
		return Response{}, fmt.Errorf("inference: http runtime has no endpoint configured")
	}
	msgs := make([]chatMessage, 0, 2)
	if h.System != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: h.System})
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: Prompt(r)})

	body := chatRequest{Model: h.Model, Messages: msgs, MaxTokens: r.MaxTokens}
	// Sampling parameters are sent only when set, so that "unset" means the
	// server's default rather than a zero this adapter invented — and so the
	// environment's params string and the request agree.
	if h.Params.Temperature != 0 {
		t := h.Params.Temperature
		body.Temperature = &t
	}
	if h.Params.TopP != 0 {
		p := h.Params.TopP
		body.TopP = &p
	}
	if h.Params.Seed != 0 {
		s := h.Params.Seed
		body.Seed = &s
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.Endpoint, bytes.NewReader(buf))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	h.auth(req)

	resp, err := h.client().Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("inference: %s: %w", h.Endpoint, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return Response{}, err
	}
	if resp.StatusCode/100 != 2 {
		return Response{}, fmt.Errorf("inference: %s returned %s: %s", h.Endpoint, resp.Status, snippet(raw))
	}
	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Response{}, fmt.Errorf("inference: malformed response from %s: %w", h.Endpoint, err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return Response{}, fmt.Errorf("inference: %s: %s", h.Endpoint, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return Response{}, fmt.Errorf("inference: %s returned no choices", h.Endpoint)
	}
	return Response{
		Text:      parsed.Choices[0].Message.Content,
		TokensIn:  parsed.Usage.PromptTokens,
		TokensOut: parsed.Usage.CompletionTokens,
	}, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// Prompt renders a Request as the text handed to a model. It is exported and
// shared by every adapter so that two runtimes given the same request see the
// same prompt — otherwise a cross-cell comparison of two attempts would be
// comparing prompts as much as models.
func Prompt(r Request) string {
	var b strings.Builder
	b.WriteString("You are implementing one ticket in a source repository.\n\n")
	b.WriteString("## Intent\n\n")
	b.WriteString(strings.TrimSpace(r.Intent))
	b.WriteString("\n")
	if len(r.Context) > 0 {
		b.WriteString("\n## Files in scope\n")
		for _, f := range r.Context {
			b.WriteString("\n### " + f.Path + "\n\n```\n")
			b.WriteString(f.Content)
			if !strings.HasSuffix(f.Content, "\n") {
				b.WriteString("\n")
			}
			b.WriteString("```\n")
		}
	}
	b.WriteString("\n## Output\n\n")
	b.WriteString("Return the complete new content of each file you change, each preceded by a line\n")
	b.WriteString("of the form `--- path/to/file` and nothing else. Change no file outside the scope\n")
	b.WriteString("above. If no change is needed, return nothing.\n")
	return b.String()
}
