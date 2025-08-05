package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"time"

	"github.com/livekit/protocol/auth"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
)

var (
    roomName = getEnvOrDefault("ROOM_NAME", "sync-room")

    // Server A settings
    deploymentA = getEnvOrDefault("DEPLOYMENT_A", "wss://api.livekit-a.io")
    apiKeyA     = getEnvOrDefault("API_KEY_A", "AserverKey")
    apiSecretA  = getEnvOrDefault("API_SECRET_A", "AserverSecret")

    // Server B settings
    deploymentB = getEnvOrDefault("DEPLOYMENT_B", "wss://api.livekit-b.io")
    apiKeyB     = getEnvOrDefault("API_KEY_B", "BserverKey")
    apiSecretB  = getEnvOrDefault("API_SECRET_B", "BserverSecret")

    bridgeName = getEnvOrDefault("BRIDGE_NAME", "bridge")
)

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// bridgeCallback implements lksdk.RoomCallback.
type bridgeCallback struct {
	sourceName             string
	targetName             string
	callback               *lksdk.RoomCallback
	targetConnections      map[string]*lksdk.Room
	bridgedTracks          map[string]map[string]bool
	connectingParticipants map[string]chan struct{}
	mu                     sync.Mutex
}

// Create a new bridgeCallback
func newBridgeCallback(sourceName, targetName string) *bridgeCallback {
	bridge := &bridgeCallback{
		sourceName:             sourceName,
		targetName:             targetName,
		targetConnections:      make(map[string]*lksdk.Room),
		bridgedTracks:          make(map[string]map[string]bool),
		connectingParticipants: make(map[string]chan struct{}),
	}

	bridge.callback = &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed:   bridge.OnTrackSubscribed,
			OnTrackUnsubscribed: bridge.OnTrackUnsubscribed,
		},
		OnParticipantDisconnected: bridge.OnParticipantDisconnected,
	}

	return bridge
}

// getOrCreateTargetConnection ensures only one connection is made per participant to the target room.
func (b *bridgeCallback) getOrCreateTargetConnection(identity string, url, key, secret string) *lksdk.Room {
	b.mu.Lock()
	if room, ok := b.targetConnections[identity]; ok {
		b.mu.Unlock()
		return room
	}
	if doneChan, isConnecting := b.connectingParticipants[identity]; isConnecting {
		b.mu.Unlock()
		<-doneChan
		b.mu.Lock()
		room := b.targetConnections[identity]
		b.mu.Unlock()
		return room
	}

	doneChan := make(chan struct{})
	b.connectingParticipants[identity] = doneChan
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.connectingParticipants, identity)
		close(doneChan)
		b.mu.Unlock()
	}()

	token, err := auth.NewAccessToken(key, secret).
		AddGrant(&auth.VideoGrant{
			RoomJoin: true,
			Room:     roomName,
		}).
		SetIdentity(identity + "-bridge").
		ToJWT()
	if err != nil {
		fmt.Printf("[%s] Failed to create token for %s: %v\n", b.sourceName, identity, err)
		return nil
	}

	room, err := lksdk.ConnectToRoomWithToken(url, token, &lksdk.RoomCallback{})
	if err != nil {
		fmt.Printf("[%s] Failed to connect to target room for %s: %v\n", b.sourceName, identity, err)
		return nil
	}

	b.mu.Lock()
	b.targetConnections[identity] = room
	b.mu.Unlock()

	fmt.Printf("[%s] Created new connection for participant %s in target room\n", b.sourceName, identity)
	return room
}

// OnTrackUnsubscribed handles cleanup when a track is unsubscribed.
func (b *bridgeCallback) OnTrackUnsubscribed(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if tracks, exists := b.bridgedTracks[rp.Identity()]; exists {
		delete(tracks, pub.SID())
		fmt.Printf("[%s] Track %s unsubscribed from participant %s\n",
			b.sourceName, pub.SID(), rp.Identity())
	}
}

// OnTrackSubscribed is invoked when a remote track is available.
func (b *bridgeCallback) OnTrackSubscribed(
	track *webrtc.TrackRemote,
	pub *lksdk.RemoteTrackPublication,
	rp *lksdk.RemoteParticipant,
) {
	fmt.Printf("[%s] Track subscribed: participant %s, track %s (sid: %s, kind: %s)\n",
		b.sourceName, rp.Identity(), pub.Name(), pub.SID(), pub.Kind())

	if strings.HasSuffix(rp.Identity(), "-bridge") {
		fmt.Printf("[%s] Ignoring track from bridge participant %s\n", b.sourceName, rp.Identity())
		return
	}

	b.mu.Lock()
	if tracks, exists := b.bridgedTracks[rp.Identity()]; exists {
		if _, bridged := tracks[pub.SID()]; bridged {
			b.mu.Unlock()
			fmt.Printf("[%s] Track %s from participant %s is already bridged\n", b.sourceName, pub.SID(), rp.Identity())
			return
		}
	} else {
		b.bridgedTracks[rp.Identity()] = make(map[string]bool)
	}
	b.mu.Unlock()

	// **FIX: Explicitly subscribe to a single, low-quality simulcast layer.**
	// This requires an up-to-date SDK. Run `go get -u github.com/livekit/server-sdk-go/v2`.
	if pub.Kind() == lksdk.TrackKindVideo {
		// SDK versions prior to v0.20 do not expose per-track subscribed quality controls.
		// To minimize bandwidth, rely on server defaults or set participant-level settings.
	}

	var targetRoom *lksdk.Room
	if b.sourceName == "deploy-a" {
		targetRoom = b.getOrCreateTargetConnection(rp.Identity(), deploymentB, apiKeyB, apiSecretB)
	} else {
		targetRoom = b.getOrCreateTargetConnection(rp.Identity(), deploymentA, apiKeyA, apiSecretA)
	}

	if targetRoom == nil {
		fmt.Printf("[%s] Failed to get target room connection for %s\n", b.sourceName, rp.Identity())
		return
	}

	localTrack, err := webrtc.NewTrackLocalStaticRTP(track.Codec().RTPCodecCapability, track.ID(), track.StreamID())
	if err != nil {
		fmt.Printf("[%s] Failed to create local track for %s: %v\n", b.sourceName, pub.SID(), err)
		return
	}

	targetPub, err := targetRoom.LocalParticipant.PublishTrack(localTrack, &lksdk.TrackPublicationOptions{
		Name:   pub.Name(),
		Source: pub.Source(),
	})
	if err != nil {
		fmt.Printf("[%s] Failed to publish track to target room %s: %v\n", b.sourceName, b.targetName, err)
		return
	}

	// Proactively request keyframes from the original publisher to keep the video stream
	// decodable for downstream subscribers. Without periodic PLIs, only the initial frame
	// may make it through, leading to the "one-frame" symptom.
	if pub.Kind() == lksdk.TrackKindVideo {
		go func() {
			ticker := time.NewTicker(3 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				rp.WritePLI(track.SSRC())
			}
		}()
	}

	b.mu.Lock()
	b.bridgedTracks[rp.Identity()][pub.SID()] = true
	b.mu.Unlock()

	fmt.Printf("[%s] Successfully published track %s to %s. Starting RTP forwarder...\n", b.sourceName, pub.SID(), b.targetName)

	defer func() {
		fmt.Printf("[%s] Stopping forwarding for track %s\n", b.sourceName, pub.SID())
		targetRoom.LocalParticipant.UnpublishTrack(targetPub.SID())
		b.mu.Lock()
		if tracks, exists := b.bridgedTracks[rp.Identity()]; exists {
			delete(tracks, pub.SID())
		}
		b.mu.Unlock()
	}()

	// Simplified RTP forwarding loop.
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			if err == io.EOF {
				fmt.Printf("[%s] Track %s ended (EOF)\n", b.sourceName, pub.SID())
				return
			}
			fmt.Printf("[%s] Error reading RTP from track %s: %v\n", b.sourceName, pub.SID(), err)
			return
		}

		if err = localTrack.WriteRTP(pkt); err != nil {
			if err == io.ErrClosedPipe {
				return
			}
			fmt.Printf("[%s] Error writing RTP to local track %s: %v\n", b.sourceName, pub.SID(), err)
			return
		}
	}
}

// OnParticipantDisconnected is called when a participant leaves.
func (b *bridgeCallback) OnParticipantDisconnected(rp *lksdk.RemoteParticipant) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room, ok := b.targetConnections[rp.Identity()]; ok {
		fmt.Printf("[%s] Participant %s disconnected, cleaning up target connection\n",
			b.sourceName, rp.Identity())
		room.Disconnect()
		delete(b.targetConnections, rp.Identity())
	}
}

func connectToRoom(url, key, secret, identity string, callback *bridgeCallback) *lksdk.Room {
	fmt.Printf("Connecting to room %s at %s with identity %s\n", roomName, url, identity)

	token, err := auth.NewAccessToken(key, secret).
		AddGrant(&auth.VideoGrant{
			RoomJoin: true,
			Room:     roomName,
			Hidden:   true,
		}).
		SetIdentity(identity).
		ToJWT()
	if err != nil {
		panic(fmt.Sprintf("failed to create token: %v", err))
	}

	room, err := lksdk.ConnectToRoomWithToken(url, token, callback.callback)
	if err != nil {
		panic(fmt.Sprintf("failed to connect to room: %v", err))
	}

	fmt.Printf("Successfully connected to room %s as %s\n", roomName, identity)
	return room
}

func main() {
	callbackA := newBridgeCallback("deploy-a", "deploy-b")
	callbackB := newBridgeCallback("deploy-b", "deploy-a")

	roomA := connectToRoom(deploymentA, apiKeyA, apiSecretA, "monitor-a", callbackA)
	roomB := connectToRoom(deploymentB, apiKeyB, apiSecretB, "monitor-b", callbackB)

	fmt.Println("Bridge running. Press Ctrl+C to exit.")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	roomA.Disconnect()
	roomB.Disconnect()

	for _, room := range callbackA.targetConnections {
		room.Disconnect()
	}
	for _, room := range callbackB.targetConnections {
		room.Disconnect()
	}
}
