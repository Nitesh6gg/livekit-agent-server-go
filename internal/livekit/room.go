// Package livekit wraps the official server-sdk-go client: joining a room,
// subscribing to the caller's audio (Opus -> PCM16, resampled to the STT rate),
// and publishing the agent's audio (PCM16 -> Opus). It does no AI work.
//
// The SDK's PCMRemoteTrack / PCMLocalTrack handle Opus decode/encode and
// resampling internally, so this package deals only in int16 PCM.
package livekit

import (
	"context"
	"fmt"

	media "github.com/livekit/media-sdk"
	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	lkmedia "github.com/livekit/server-sdk-go/v2/pkg/media"
	"github.com/pion/webrtc/v4"
)

// Params configures a room session.
type Params struct {
	URL       string
	APIKey    string
	APISecret string
	Room      string
	Identity  string

	// STTSampleRate is the target rate for caller audio delivered to OnCallerAudio.
	STTSampleRate int
	// TTSSampleRate is the source rate of PCM passed to WritePlayback.
	TTSSampleRate int

	// OnCallerAudio receives mono int16 PCM at STTSampleRate. It is called from
	// the SDK's decode goroutine and must not block.
	OnCallerAudio func([]int16)
}

// Session is a joined room with a published agent audio track.
type Session struct {
	room   *lksdk.Room
	pub    *lkmedia.PCMLocalTrack
	remote *lkmedia.PCMRemoteTrack
}

// Connect joins the room and publishes the agent's audio track.
func Connect(_ context.Context, p Params) (*Session, error) {
	s := &Session{}

	cb := &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, _ *lksdk.RemoteTrackPublication, _ *lksdk.RemoteParticipant) {
				if track.Codec().MimeType != webrtc.MimeTypeOpus {
					return
				}
				if s.remote != nil {
					return // handle a single caller track for Phase 1
				}
				w := &pcmWriter{onAudio: p.OnCallerAudio}
				rt, err := lkmedia.NewPCMRemoteTrack(
					track, w,
					lkmedia.WithTargetSampleRate(p.STTSampleRate),
					lkmedia.WithTargetChannels(1),
				)
				if err != nil {
					logger.GetLogger().Errorw("failed to create PCM remote track", err)
					return
				}
				s.remote = rt
			},
		},
		OnDisconnected: func() {
			if s.remote != nil {
				s.remote.Close()
				s.remote = nil
			}
		},
	}

	room, err := lksdk.ConnectToRoom(p.URL, lksdk.ConnectInfo{
		APIKey:              p.APIKey,
		APISecret:           p.APISecret,
		RoomName:            p.Room,
		ParticipantIdentity: p.Identity,
		ParticipantName:     p.Identity,
	}, cb)
	if err != nil {
		return nil, fmt.Errorf("connect to room: %w", err)
	}
	s.room = room

	pub, err := lkmedia.NewPCMLocalTrack(p.TTSSampleRate, 1, logger.GetLogger())
	if err != nil {
		room.Disconnect()
		return nil, fmt.Errorf("create PCM local track: %w", err)
	}
	if _, err := room.LocalParticipant.PublishTrack(pub, &lksdk.TrackPublicationOptions{Name: "agent"}); err != nil {
		room.Disconnect()
		return nil, fmt.Errorf("publish agent track: %w", err)
	}
	s.pub = pub

	return s, nil
}

// WritePlayback queues agent PCM (mono int16 at TTSSampleRate) for playout.
func (s *Session) WritePlayback(pcm []int16) error {
	return s.pub.WriteSample(media.PCM16Sample(pcm))
}

// FlushPlayback drops all queued-but-unplayed agent audio. Used for barge-in.
func (s *Session) FlushPlayback() {
	s.pub.ClearQueue()
}

// Disconnect tears down the published track and leaves the room.
func (s *Session) Disconnect() {
	if s.pub != nil {
		s.pub.ClearQueue()
		s.pub.Close()
	}
	if s.remote != nil {
		s.remote.Close()
	}
	if s.room != nil {
		s.room.Disconnect()
	}
}

// pcmWriter adapts the caller-audio callback to the SDK's writer interface.
type pcmWriter struct {
	onAudio func([]int16)
}

func (w *pcmWriter) WriteSample(sample media.PCM16Sample) error {
	if w.onAudio != nil && len(sample) > 0 {
		cp := make([]int16, len(sample))
		copy(cp, sample)
		w.onAudio(cp)
	}
	return nil
}

func (w *pcmWriter) Close() error { return nil }
