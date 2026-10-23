package gateway

// Protocol bridges (M2 of the product plan). The gateway's upstream pool
// speaks OpenAI Chat Completions, so the bridges translate inbound Anthropic
// Messages (/v1/messages) and OpenAI Responses (/v1/responses) requests into
// Chat form, forward them through the normal acquire/retry pipeline, and
// translate the Chat response stream back to the client's protocol. Usage
// accounting, sessions and redaction all keep operating on the raw upstream
// bytes; the client only ever sees its own protocol.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// bridgeKind identifies the outbound translation the proxy should apply.
type bridgeKind int

const (
	bridgeNone bridgeKind = iota
	bridgeMessages           // Chat -> Anthropic Messages
	bridgeResponses          // Chat -> Responses
)

// bridgedRequest carries the converted body and the encoder decision into the
// proxy pipeline.
type bridgedRequest struct {
	body []byte
	kind bridgeKind
}

// chatRequestFrom converts an inbound request body + path into a Chat
// Completions body.
func (s *Server) chatRequestFrom(path string, body []byte) (bridgedRequest, error) {
	if path == "/v1/messages" && s.Config.Bridges.Messages {
		converted, err := messagesToChat(body)
		if err != nil {
			return bridgedRequest{}, err
		}
		return bridgedRequest{body: converted, kind: bridgeMessages}, nil
	}
	if path == "/v1/responses" && s.Config.Bridges.Responses {
		converted, err := responsesToChat(body)
		if err != nil {
			return bridgedRequest{}, err
		}
		return bridgedRequest{body: converted, kind: bridgeResponses}, nil
	}
	return bridgedRequest{body: body, kind: bridgeNone}, nil
}

// bridgeWriter translates the upstream Chat response into the target protocol
// while the proxy streams it.
type bridgeWriter struct {
	http.ResponseWriter
	enc      bridgeEncoder
	stream   bool
	forceRaw bool // upstream ignored stream:true and returned JSON; pass through
	buffered *bytes.Buffer // non-streaming: hold the JSON until EOF
	h        http.Header   // captured headers for non-streaming finalize
	status   int
	wrote    bool
}

func newBridgeWriter(w http.ResponseWriter, kind bridgeKind, stream bool) *bridgeWriter {
	bw := &bridgeWriter{ResponseWriter: w, stream: stream, h: w.Header().Clone()}
	if kind == bridgeMessages {
		bw.enc = newAnthropicEncoder()
	} else {
		bw.enc = newResponsesEncoder()
	}
	if stream {
		bw.Header().Set("Content-Type", "text/event-stream")
	} else {
		bw.buffered = &bytes.Buffer{}
	}
	return bw
}

func (b *bridgeWriter) WriteHeader(status int) {
	b.status = status
	// Error responses must reach the client immediately in every mode: for
	// non-streaming they must not be delayed into the buffer, otherwise the
	// underlying writer would auto-send 200 and finalize would duplicate the
	// body. Streaming already forwards the header.
	if status >= 400 {
		b.ResponseWriter.WriteHeader(status)
	}
	if b.stream && status < 400 {
		b.ResponseWriter.WriteHeader(status)
	}
}

// setRaw implements rawSwitcher for the proxy's non-SSE detection. It
// cascades to the inner writer so a chain (chatRewrite -> bridge) all switch
// to passthrough together — otherwise the outer layer would pass the JSON
// through into the inner SSE encoder, which would swallow it.
// Unwrap exposes the wrapped writer so layered writers can be searched for the
// gateway's trace holder.
func (b *bridgeWriter) Unwrap() http.ResponseWriter { return b.ResponseWriter }

func (b *bridgeWriter) setRaw() {
	b.forceRaw = true
	if inner, ok := b.ResponseWriter.(rawSwitcher); ok {
		inner.setRaw()
	}
}

// wantsStream reports streaming mode.
func (b *bridgeWriter) wantsStream() bool { return b.stream }

func (b *bridgeWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	// Error responses (4xx/5xx) pass through unmodified: the client protocol
	// error shape differs from Chat and translation would corrupt the message.
	if b.status >= 400 || b.forceRaw {
		return b.ResponseWriter.Write(p)
	}
	if b.stream {
		if err := b.enc.Write(b.ResponseWriter, p); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	// Non-streaming: the whole payload must be translated as one JSON object.
	b.buffered.Write(p)
	return len(p), nil
}

// Flush implements http.Flusher for the streaming path.
func (b *bridgeWriter) Flush() {
	if f, ok := b.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// end finalizes a streaming bridge after the proxy returned: it emits the
// protocol's terminal event when the upstream never sent usage.
func (b *bridgeWriter) end() {
	if !b.stream || b.status >= 400 {
		return
	}
	_ = b.enc.End(b.ResponseWriter)
	if f, ok := b.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// finalize writes the translated non-streaming JSON once the proxy finished.
func (b *bridgeWriter) finalize() error {
	if b.stream || b.buffered == nil {
		return nil
	}
	body := b.buffered.Bytes()
	h := b.ResponseWriter.Header()
	for k, vs := range b.h {
		if k == "Content-Length" || k == "Content-MD5" || k == "Digest" || k == "ETag" {
			continue
		}
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	if b.status == 0 {
		b.status = http.StatusOK
	}
	// Error responses pass through raw (the client protocol error shape may
	// still differ, but HTTP status and message remain readable).
	if b.status >= 400 || len(bytes.TrimSpace(body)) == 0 {
		b.ResponseWriter.WriteHeader(b.status)
		_, err := b.ResponseWriter.Write(body)
		return err
	}
	translated, err := b.enc.TranslateNonStreaming(body)
	if err != nil {
		// Translation failed (e.g. the upstream ignored stream:false and
		// returned SSE, or a non-Chat error shape): pass the raw body through
		// so the client sees a concrete failure instead of an empty reply.
		b.ResponseWriter.WriteHeader(b.status)
		_, writeErr := b.ResponseWriter.Write(body)
		return writeErr
	}
	h.Set("Content-Type", "application/json")
	b.ResponseWriter.WriteHeader(b.status)
	_, err = b.ResponseWriter.Write(translated)
	return err
}

// maybeBridge rewrites the request when a bridge owns the path. It returns the
// converted body and whether the request went through a bridge (in which case
// a gateway response may already have been written on conversion error).
func (s *Server) maybeBridge(w http.ResponseWriter, path string, body []byte) (bridgedRequest, bool) {
	if !s.Config.Bridges.Messages && !s.Config.Bridges.Responses {
		return bridgedRequest{body: body}, false
	}
	if path != "/v1/messages" && path != "/v1/responses" {
		return bridgedRequest{body: body}, false
	}
	converted, err := s.chatRequestFrom(path, body)
	if err != nil {
		apiError(w, 400, "bridge_invalid_request", err.Error())
		return bridgedRequest{}, true
	}
	if converted.kind == bridgeNone {
		// This path's bridge is disabled (the other bridge is on): keep the
		// historical passthrough behavior instead of returning an empty reply.
		return bridgedRequest{body: body}, false
	}
	return converted, true
}

// Forwarding through the bridge reserves the retry semantics of
// /v1/chat/completions (POST 401/429 failover) while the trace keeps the
// client path for sessions and logs.
func bridgeClientPath(path string) string {
	if path == "/v1/messages" || path == "/v1/responses" {
		return "/v1/chat/completions"
	}
	return path
}

// rawSwitcher is implemented by streaming response writers that must fall back
// to raw passthrough when the upstream ignores stream:true and returns JSON.
// wantsStream reports whether the writer is in streaming mode: only streaming
// writers may switch to raw (a buffering writer still needs its finalize).
type rawSwitcher interface {
	setRaw()
	wantsStream() bool
}

// bridgeEncoder translates Chat response bytes into the target protocol.
type bridgeEncoder interface {
	// Write translates one chunk of SSE bytes, writing the translated SSE to
	// dst.
	Write(dst io.Writer, chunk []byte) error
	// TranslateNonStreaming converts one complete non-streaming Chat JSON
	// response into the target JSON response.
	TranslateNonStreaming(body []byte) ([]byte, error)
	// End emits the protocol's terminal event when the upstream stream ended
	// without one (e.g. no usage chunk arrived).
	End(dst io.Writer) error
}

// ---- shared Chat SSE parsing ----

type ssePump struct {
	line []byte
}

// feed processes a chunk of SSE bytes, invoking event for each complete
// blank-line-delimited event with the accumulated data lines joined by "\n".
// The "[DONE]" sentinel is consumed and not reported.
func (p *ssePump) feed(chunk []byte, event func(data string)) {
	p.feedRaw(chunk, func(data string) {
		if data != "[DONE]" {
			event(data)
		}
	})
}

// feedRaw is feed without the [DONE] filter; the Chat response rewriter needs
// the sentinel to forward it.
func (p *ssePump) feedRaw(chunk []byte, event func(data string)) {
	for len(chunk) > 0 {
		i := bytes.IndexByte(chunk, '\n')
		var part []byte
		if i < 0 {
			p.line = append(p.line, chunk...)
			return
		}
		part = chunk[:i+1]
		chunk = chunk[i+1:]
		line := strings.TrimRight(string(part), "\r\n")
		if line == "" {
			if len(p.line) > 0 {
				data := strings.TrimPrefix(string(p.line), "data:")
				data = strings.TrimPrefix(data, " ")
				event(data)
				p.line = nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			part := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			if len(p.line) > 0 {
				p.line = append(p.line, '\n')
			}
			p.line = append(p.line, part...)
		}
	}
}

// finish processes any trailing partial event (non-SSE path safety).
func (p *ssePump) finish(event func(data string)) {
	if len(p.line) > 0 {
		data := strings.TrimPrefix(string(p.line), "data:")
		if data != "[DONE]" {
			event(strings.TrimPrefix(data, " "))
		}
		p.line = nil
	}
}

type chatChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int `json:"index"`
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Role            string `json:"role"`
			Content         string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls       []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		Message struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage *json.RawMessage `json:"usage"`
}

func decodeChatChunk(data string) (*chatChunk, error) {
	var c chatChunk
	if err := json.Unmarshal([]byte(data), &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// jsonString marshals v as a JSON string literal.
func jsonString(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// randSuffix returns a short random token used to synthesize ids for
// translated responses that carry none.
func randSuffix() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "gateway"
	}
	return hex.EncodeToString(b)
}

func nowUnix() int64 { return time.Now().Unix() }