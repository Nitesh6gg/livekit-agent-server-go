// Package tts streams text to Sarvam's text-to-speech WebSocket and emits
// linear16 PCM audio chunks.
//
// Protocol reference: the production pipecat Sarvam TTS client
// (api/patches/pipecat_sarvam_tts.py in the voice-ai-agent repo). Message flow:
//
//	-> {"type":"config","data":{...}}
//	-> {"type":"text","data":{"text":"..."}}
//	-> {"type":"flush"}          // synthesize buffered text
//	-> {"type":"ping"}           // keepalive
//	<- {"type":"audio","data":{"audio":"<base64 linear16>","request_id":"..."}}
//	<- {"type":"event","data":{"event_type":"final"}}  // this flush's audio is complete
//	<- {"type":"error","data":{"message":"..."}}
//
// The connection is call-scoped, not turn-scoped: it is dialed once and reused
// across every Speak call for the life of the call, the same way the STT
// connection is. Cancelling one turn does not stop Sarvam from continuing to
// synthesize and deliver audio for it — Sarvam has no "stop this request"
// message, and request_id is a session identifier, not a per-request one (it
// was verified constant across sequential Speak calls on the same connection
// via a direct probe). So audio for an already-cancelled turn can still arrive
// after a newer turn has started.
//
// Each chunk is therefore tagged with the generation number the caller passed
// to Speak, using the "final" event as the boundary between one Speak call's
// audio and the next: LiveKit Agents' own SpeechHandle takes the same
// approach — filtering stale audio at the point of playback rather than trying
// to cancel synthesis server-side — because closing and reconnecting the
// socket on every interruption would add reconnect latency to the very next
// reply, which is the reply the caller is now waiting on.
package tts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const sarvamTTSBaseURL = "wss://api.sarvam.ai/text-to-speech/ws"

// audioBufferChunks is deliberately large. Synthesis outruns real-time playback,
// so a whole reply can land well before it is played out. The read loop must
// never block on a full buffer: that stalls the socket reader and Sarvam drops
// the connection.
const audioBufferChunks = 256

// Config configures the Sarvam TTS client.
type Config struct {
	APIKey     string
	Model      string // e.g. "bulbul:v3"
	Voice      string // e.g. "shubh"
	Language   string // e.g. "hi-IN"
	SampleRate int    // e.g. 24000
}

// pendingGen is one outstanding Speak call awaiting its "final" event.
type pendingGen struct {
	gen      uint64
	pushedAt time.Time
}

// Chunk is one piece of decoded PCM, tagged with the generation number that was
// passed to the Speak call which produced it. A caller that tracks "the current
// generation" can drop any Chunk whose Gen no longer matches, discarding audio
// left over from an interrupted turn no matter when it arrives.
type Chunk struct {
	PCM []int16
	Gen uint64
}

// Client is a streaming Sarvam TTS connection. The socket is redialed lazily on
// the next Speak after a drop rather than from a standing retry loop, so a
// permanently rejected config costs nothing while idle.
type Client struct {
	cfg Config
	ctx context.Context

	connMu sync.RWMutex
	conn   *websocket.Conn

	writeMu sync.Mutex
	dropped atomic.Uint64

	// pendingGens is the FIFO queue of generations with outstanding Speak
	// calls: pushed in Speak, popped on each "final" event. Chunks arriving
	// before the next "final" belong to the generation at the front. A fresh
	// connection (initial or reconnect) starts with an empty queue: whatever
	// "final" events were still outstanding on the old connection will never
	// arrive on it.
	//
	// pushedAt is logged on pop so a slow "final" — which mistags newer
	// generations' audio as the stuck old one, per docs/DECISIONS.md ADR-016 —
	// is directly visible instead of inferred from symptoms.
	genMu       sync.Mutex
	pendingGens []pendingGen
	lastGen     uint64 // most recently pushed generation, for the empty-queue fallback in peekGen

	// audio is never closed by a per-connection read loop; consumers exit via
	// their own context so a dropped socket cannot mute the agent for good.
	audio chan Chunk
}

// New creates an unconnected client.
func New(cfg Config) *Client {
	return &Client{
		cfg:   cfg,
		audio: make(chan Chunk, audioBufferChunks),
	}
}

// Audio yields decoded linear16 PCM chunks at cfg.SampleRate, each tagged with
// its originating generation.
func (c *Client) Audio() <-chan Chunk { return c.audio }

// Connect dials the Sarvam TTS WebSocket, sends the initial config, and starts
// the receive + keepalive loops.
func (c *Client) Connect(ctx context.Context) error {
	c.ctx = ctx
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	if err := c.sendConfig(conn); err != nil {
		conn.Close()
		return err
	}
	c.setConn(conn)
	c.resetGens()

	go c.receiveLoop(ctx, conn)
	go c.keepaliveLoop(ctx)
	return nil
}

func (c *Client) dial(ctx context.Context) (*websocket.Conn, error) {
	// send_completion_event=true is required: without it Sarvam accepts the
	// connection and every message but never emits audio (verified against
	// bulbul:v3 — silent 20s timeout without the flag, audio immediately with
	// it). pipecat's client sets it too.
	endpoint := fmt.Sprintf("%s?model=%s&send_completion_event=true", sarvamTTSBaseURL, c.cfg.Model)
	header := http.Header{}
	header.Set("api-subscription-key", c.cfg.APIKey)

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, endpoint, header)
	if err != nil {
		return nil, fmt.Errorf("sarvam tts dial: %w", err)
	}
	return conn, nil
}

func (c *Client) sendConfig(conn *websocket.Conn) error {
	cfg := map[string]any{
		"target_language_code": c.cfg.Language,
		"speaker":              c.cfg.Voice,
		"speech_sample_rate":   fmt.Sprintf("%d", c.cfg.SampleRate),
		"enable_preprocessing": false,
		"min_buffer_size":      50,
		"max_chunk_length":     150,
		"output_audio_codec":   "linear16",
		"output_audio_bitrate": "128k",
		"pace":                 1.0,
		"model":                c.cfg.Model,
	}
	return c.writeJSON(conn, map[string]any{"type": "config", "data": cfg})
}

func (c *Client) setConn(conn *websocket.Conn) {
	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()
}

func (c *Client) getConn() *websocket.Conn {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	return c.conn
}

// invalidate clears conn if it is still the active one, so the next Speak
// redials.
func (c *Client) invalidate(conn *websocket.Conn) {
	c.connMu.Lock()
	if c.conn == conn {
		c.conn = nil
	}
	c.connMu.Unlock()
	conn.Close()
}

// Disconnect closes the current connection so the next Speak call redials on
// a clean session, with an empty generation queue (see resetGens).
//
// Sarvam has no message to cancel an in-flight synthesis request — once text
// is sent, audio for it keeps arriving until Sarvam is done, regardless of
// what the caller wants by then (see ADR-015). Generation tagging discards
// that stray audio at playback time, but depends on Sarvam's "final" event to
// know when a generation is over, and that event was observed taking up to
// 9.5s in a live call — long enough to misattribute a fresh reply's real audio
// as stale leftovers and drop it. Disconnecting removes the ambiguity at the
// source: nothing more can arrive for a request whose connection no longer
// exists.
//
// Callers should invoke this only when synthesis was genuinely in progress at
// interruption time (matching pipecat's `should_reconnect = bot_speaking or
// tts_started`) — reconnecting unconditionally on every barge-in would pay a
// redial on the next reply even when there was nothing to clean up.
func (c *Client) Disconnect() {
	if conn := c.getConn(); conn != nil {
		c.invalidate(conn)
	}
}

// ensureConn returns a live connection, redialing if the previous one died.
// Called from the turn goroutine, so a blocking dial here is acceptable.
func (c *Client) ensureConn() (*websocket.Conn, error) {
	if conn := c.getConn(); conn != nil {
		return conn, nil
	}
	if c.ctx == nil || c.ctx.Err() != nil {
		return nil, fmt.Errorf("tts not connected")
	}
	conn, err := c.dial(c.ctx)
	if err != nil {
		return nil, err
	}
	if err := c.sendConfig(conn); err != nil {
		conn.Close()
		return nil, err
	}
	c.setConn(conn)
	c.resetGens()
	log.Print("sarvam tts reconnected")
	go c.receiveLoop(c.ctx, conn)
	return conn, nil
}

// Speak sends text for synthesis followed by a flush so Sarvam emits audio.
// gen is the caller's generation number for the turn this text belongs to; it
// is attached to every Chunk this call produces.
func (c *Client) Speak(text string, gen uint64) error {
	if text == "" {
		return nil
	}
	conn, err := c.ensureConn()
	if err != nil {
		return err
	}
	// Pushed before the write completes so a chunk that arrives immediately
	// after cannot race ahead of its own generation being queued.
	c.pushGen(gen)
	if err := c.writeJSON(conn, map[string]any{
		"type": "text",
		"data": map[string]any{"text": text},
	}); err != nil {
		c.invalidate(conn)
		return err
	}
	if err := c.writeJSON(conn, map[string]any{"type": "flush"}); err != nil {
		c.invalidate(conn)
		return err
	}
	return nil
}

func (c *Client) pushGen(gen uint64) {
	c.genMu.Lock()
	depthBefore := len(c.pendingGens)
	c.pendingGens = append(c.pendingGens, pendingGen{gen: gen, pushedAt: time.Now()})
	c.lastGen = gen
	c.genMu.Unlock()

	if depthBefore > 0 {
		// A generation older than this one is still awaiting its "final". Any
		// audio arriving before that arrives will be tagged with the OLD
		// generation, not this one — the exact condition that can make a fresh
		// reply's audio look like stale leftovers and get silently dropped.
		log.Printf("sarvam tts: gen=%d queued behind %d still-open generation(s)", gen, depthBefore)
	}
}

// popGen removes and returns the oldest pending generation on a "final" event,
// logging how long it took — a slow "final" is what lets a newer generation's
// audio get mistagged as the old one.
func (c *Client) popGen() (gen uint64, ok bool) {
	c.genMu.Lock()
	if len(c.pendingGens) == 0 {
		c.genMu.Unlock()
		return 0, false
	}
	head := c.pendingGens[0]
	c.pendingGens = c.pendingGens[1:]
	depthAfter := len(c.pendingGens)
	c.genMu.Unlock()

	log.Printf("sarvam tts: gen=%d final received after %s (%d generation(s) still queued)",
		head.gen, time.Since(head.pushedAt), depthAfter)
	return head.gen, true
}

// peekGen returns the generation new audio should be tagged with: the oldest
// still-pending one, or the last known generation if the queue is briefly
// empty between a "final" event and the next Speak call.
func (c *Client) peekGen() uint64 {
	c.genMu.Lock()
	defer c.genMu.Unlock()
	if len(c.pendingGens) > 0 {
		return c.pendingGens[0].gen
	}
	return c.lastGen
}

// resetGens clears pending state on a fresh connection. Any "final" events the
// previous connection owed will never arrive on it.
func (c *Client) resetGens() {
	c.genMu.Lock()
	c.pendingGens = nil
	c.genMu.Unlock()
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	conn := c.getConn()
	if conn == nil {
		return nil
	}
	return conn.Close()
}

func (c *Client) writeJSON(conn *websocket.Conn, v any) error {
	if conn == nil {
		return fmt.Errorf("tts not connected")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return conn.WriteJSON(v)
}

// receiveLoop owns one connection and exits when that connection dies; the next
// Speak redials.
func (c *Client) receiveLoop(ctx context.Context, conn *websocket.Conn) {
	for {
		if ctx.Err() != nil {
			return
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			c.invalidate(conn)
			return
		}
		var m inboundMessage
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		switch {
		case m.Type == "error":
			// Sarvam reports rejected config (bad model/speaker combination,
			// bad parameters, rate limits) here and then closes the socket.
			log.Printf("sarvam tts error: %s", m.Data.Message)
		case m.Type == "event" && m.Data.EventType == "final":
			// This Speak call's audio is fully delivered; whatever is next in
			// the queue (if anything) now owns subsequent chunks. Verified
			// against the live API: exactly one "final" per flush, in order.
			c.popGen()
		case m.Type == "audio" && m.Data.Audio != "":
			raw, err := base64.StdEncoding.DecodeString(m.Data.Audio)
			if err != nil {
				continue
			}
			chunk := Chunk{PCM: bytesToPCM16(raw), Gen: c.peekGen()}
			// Non-blocking: never stall the socket reader on a full buffer.
			select {
			case c.audio <- chunk:
			case <-ctx.Done():
				return
			default:
				if n := c.dropped.Add(1); n%50 == 1 {
					log.Printf("tts audio buffer full, dropped %d chunk(s)", n)
				}
			}
		}
	}
}

func (c *Client) keepaliveLoop(ctx context.Context) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if conn := c.getConn(); conn != nil {
				_ = c.writeJSON(conn, map[string]any{"type": "ping"})
			}
		}
	}
}

// bytesToPCM16 parses little-endian 16-bit PCM bytes into int16 samples.
func bytesToPCM16(b []byte) []int16 {
	n := len(b) / 2
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		out[i] = int16(b[i*2]) | int16(b[i*2+1])<<8
	}
	return out
}

type inboundMessage struct {
	Type string `json:"type"`
	Data struct {
		Audio     string `json:"audio"`
		Message   string `json:"message"`
		EventType string `json:"event_type"`
	} `json:"data"`
}
