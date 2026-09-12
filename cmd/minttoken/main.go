// Command minttoken prints a LiveKit access token for a room-join grant.
//
// Dev-only helper for Phase 1: lets a human tester join the same room as the
// agent worker (e.g. via https://meet.livekit.io) without the agent's own
// key/secret ever leaving this box. See docs/SETUP.md.
package main

import (
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/go2market/go-agent-worker/config"
	"github.com/livekit/protocol/auth"
)

func main() {
	identity := flag.String("identity", "human-caller", "participant identity")
	room := flag.String("room", "", "room to join (defaults to LIVEKIT_ROOM from .env)")
	validFor := flag.Duration("valid-for", time.Hour, "token validity duration")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	roomName := *room
	if roomName == "" {
		roomName = cfg.Room
	}

	at := auth.NewAccessToken(cfg.LiveKitAPIKey, cfg.LiveKitAPISecret).
		SetIdentity(*identity).
		SetName(*identity).
		SetValidFor(*validFor).
		SetVideoGrant(&auth.VideoGrant{RoomJoin: true, Room: roomName})

	token, err := at.ToJWT()
	if err != nil {
		log.Fatalf("mint token: %v", err)
	}

	fmt.Println(token)
	fmt.Printf("\nServer URL: %s\nRoom:       %s\nIdentity:   %s\nValid for:  %s\n",
		cfg.LiveKitURL, roomName, *identity, *validFor)
}
