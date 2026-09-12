package agent

import (
	"sync"

	"github.com/go2market/go-agent-worker/internal/llm"
)

// History is the running conversation, safe for concurrent access.
//
// Gemini has no dedicated system role in `contents`, so the behavioral
// instruction is seeded as an initial user/model exchange.
type History struct {
	mu   sync.RWMutex
	msgs []llm.Message
}

// NewHistory seeds the conversation with a behavioral instruction.
func NewHistory(systemPrompt string) *History {
	h := &History{}
	if systemPrompt != "" {
		h.msgs = []llm.Message{
			{Role: "user", Text: systemPrompt},
			{Role: "model", Text: "Understood."},
		}
	}
	return h
}

// AddUser appends a user turn.
func (h *History) AddUser(text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, llm.Message{Role: "user", Text: text})
}

// AddModel appends an assistant turn.
func (h *History) AddModel(text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, llm.Message{Role: "model", Text: text})
}

// Snapshot returns a copy of the current messages for an LLM call.
func (h *History) Snapshot() []llm.Message {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]llm.Message, len(h.msgs))
	copy(out, h.msgs)
	return out
}
