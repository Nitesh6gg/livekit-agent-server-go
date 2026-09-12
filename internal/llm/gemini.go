// Package llm streams tokens from Gemini given the running conversation.
//
// Uses google.golang.org/genai. Token streaming is cancellable mid-generation
// (pass a cancellable context) so a barge-in can abort the reply immediately.
package llm

import (
	"context"

	"google.golang.org/genai"
)

// Message is one conversation turn. Role is "user" or "model".
type Message struct {
	Role string
	Text string
}

// Config configures the Gemini client.
type Config struct {
	APIKey string
	Model  string // e.g. "gemini-2.5-flash"
}

// Client wraps a genai client bound to one model.
type Client struct {
	model  string
	client *genai.Client
}

// New creates a Gemini client.
func New(ctx context.Context, cfg Config) (*Client, error) {
	gc, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  cfg.APIKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, err
	}
	return &Client{model: cfg.Model, client: gc}, nil
}

// Stream generates a reply for the given history and returns a channel of text
// tokens. The channel closes when generation finishes, errors, or ctx is
// cancelled. Streaming errors are reported on the returned error channel (at
// most one value) and also close the token channel.
func (c *Client) Stream(ctx context.Context, history []Message) (<-chan string, <-chan error) {
	tokens := make(chan string, 16)
	errc := make(chan error, 1)

	contents := make([]*genai.Content, 0, len(history))
	for _, m := range history {
		role := genai.Role(genai.RoleUser)
		if m.Role == "model" {
			role = genai.RoleModel
		}
		contents = append(contents, genai.NewContentFromText(m.Text, role))
	}

	go func() {
		defer close(tokens)
		defer close(errc)
		for resp, err := range c.client.Models.GenerateContentStream(ctx, c.model, contents, nil) {
			if err != nil {
				errc <- err
				return
			}
			if ctx.Err() != nil {
				return
			}
			if t := resp.Text(); t != "" {
				select {
				case tokens <- t:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return tokens, errc
}
