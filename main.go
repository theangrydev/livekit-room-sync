package main

import (
    "fmt"
    "os"
    "os/signal"
    "syscall"
    "time"
    "sync"
    "sort"

    "github.com/livekit/protocol/auth"
    lksdk "github.com/livekit/server-sdk-go/v2"
    "github.com/pion/webrtc/v4"
    "github.com/pion/rtp"
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

// Helper function to check if a VP9 packet is a key frame
func isVP9KeyFrame(payload []byte) bool {
    if len(payload) < 1 {
        return false
    }
    // Check VP9 payload descriptor
    // I bit (inverse key frame flag) is the first bit
    // If I bit is 0, this is a key frame
    return (payload[0] & 0x80) == 0
}

// bridgeCallback implements lksdk.RoomCallback.
type bridgeCallback struct {
    target     *lksdk.Room
    sourceName string
    targetName string
    callback   *lksdk.RoomCallback
    targetConnections map[string]*lksdk.Room  // map of participant identity to their room connection
    bridgedTracks    map[string]map[string]bool // map of participant identity to track IDs that are already bridged
    connectingParticipants map[string]chan struct{} // tracks participants that are in the process of connecting
    mu sync.Mutex // protects the maps above
    videoLayers map[string]map[uint8]bool // track SID to layer mapping
}

// Create a new bridgeCallback
func newBridgeCallback(sourceName, targetName string) *bridgeCallback {
    bridge := &bridgeCallback{
        sourceName: sourceName,
        targetName: targetName,
        targetConnections: make(map[string]*lksdk.Room),
        bridgedTracks: make(map[string]map[string]bool),
        connectingParticipants: make(map[string]chan struct{}),
    }

    bridge.callback = &lksdk.RoomCallback{
        ParticipantCallback: lksdk.ParticipantCallback{
            OnTrackSubscribed: bridge.OnTrackSubscribed,
            OnTrackUnsubscribed: bridge.OnTrackUnsubscribed,
        },
        OnParticipantDisconnected: bridge.OnParticipantDisconnected,
    }

    return bridge
}

func (b *bridgeCallback) getOrCreateTargetConnection(identity string, url, key, secret string) *lksdk.Room {
    b.mu.Lock()
    
    // First check if we already have an established connection
    if room, ok := b.targetConnections[identity]; ok {
        b.mu.Unlock()
        return room
    }

    // Check if a connection is in progress
    if doneChan, isConnecting := b.connectingParticipants[identity]; isConnecting {
        b.mu.Unlock()
        // Wait for the existing connection attempt to complete
        <-doneChan
        // Check the result
        b.mu.Lock()
        room := b.targetConnections[identity]
        b.mu.Unlock()
        return room
    }

    // Create a channel to signal when this connection attempt is complete
    doneChan := make(chan struct{})
    b.connectingParticipants[identity] = doneChan
    b.mu.Unlock()

    // Ensure we clean up the connecting state when we're done
    defer func() {
        b.mu.Lock()
        delete(b.connectingParticipants, identity)
        close(doneChan)
        b.mu.Unlock()
    }()

    // Create a new connection with the participant's identity
    token, err := auth.NewAccessToken(key, secret).
        AddGrant(&auth.VideoGrant{
            RoomJoin: true,
            Room: roomName,
            Hidden: false,
        }).
        SetIdentity(identity + "-bridge").
        ToJWT()
    if err != nil {
        fmt.Printf("[%s] Failed to create token for %s: %v\n", b.sourceName, identity, err)
        return nil
    }

    // Create an empty callback since we don't need to handle events for the target connection
    emptyCallback := &lksdk.RoomCallback{}
    
    room, err := lksdk.ConnectToRoomWithToken(url, token, emptyCallback)
    if err != nil {
        fmt.Printf("[%s] Failed to connect to target room for %s: %v\n", b.sourceName, identity, err)
        return nil
    }

    // Store the successful connection
    b.mu.Lock()
    b.targetConnections[identity] = room
    b.mu.Unlock()

    fmt.Printf("[%s] Created new connection for participant %s in target room\n", b.sourceName, identity)
    return room
}

// OnTrackUnsubscribed handles cleanup when a track is unsubscribed
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
    fmt.Printf("[%s] Track subscribed from participant %s: %s (kind: %s, sid: %s, source: %s)\n", 
        b.sourceName, rp.Identity(), pub.Name(), pub.Kind(), pub.SID(), pub.Source())

    // Print all tracks of the participant for debugging
    fmt.Printf("[%s] Current tracks for participant %s:\n", b.sourceName, rp.Identity())
    for _, track := range rp.TrackPublications() {
        fmt.Printf("  - Track: %s (kind: %s, source: %s)\n", track.SID(), track.Kind(), track.Source())
    }

    // Ignore tracks from bridge participants (they will have "-bridge" suffix)
    if len(rp.Identity()) > 7 && rp.Identity()[len(rp.Identity())-7:] == "-bridge" {
        fmt.Printf("[%s] Ignoring bridged track from %s\n", b.sourceName, rp.Identity())
        return
    }

    // Check if this specific track is already bridged
    if tracks, exists := b.bridgedTracks[rp.Identity()]; exists {
        if tracks[pub.SID()] {
            fmt.Printf("[%s] Track %s from participant %s is already bridged\n", 
                b.sourceName, pub.SID(), rp.Identity())
            return
        }
    } else {
        // Initialize track map for this participant if it doesn't exist
        b.bridgedTracks[rp.Identity()] = make(map[string]bool)
    }

    // For video tracks, try to get the lowest quality layer
    if pub.Kind() == lksdk.TrackKindVideo {
        fmt.Printf("[%s] Setting up VP9 video track %s\n", b.sourceName, pub.SID())
        
        // Create a buffered channel for packets with larger size
        const bufferSize = 4000 // Increased for better buffering
        pktChan := make(chan *rtp.Packet, bufferSize)

        // Jitter buffer configuration
        const (
            maxJitterBufferSize = 100    // Maximum number of packets to buffer
            maxWaitTime        = 100 * time.Millisecond // Maximum time to wait for missing packets
            pliInterval        = 2 * time.Second        // Minimum interval between PLI requests
            maxPacketGap       = 10                     // Maximum sequence number gap before requesting PLI
        )

        type JitterBuffer struct {
            packets    map[uint16]*rtp.Packet
            timestamps map[uint16]time.Time
            mu        sync.Mutex
        }

        jitterBuffer := &JitterBuffer{
            packets:    make(map[uint16]*rtp.Packet),
            timestamps: make(map[uint16]time.Time),
        }

        // Stats tracking
        type PacketStats struct {
            totalPackets uint64
            lostPackets uint64
            recoveredPackets uint64
            outOfOrder uint64
            duplicates uint64
            pliCount uint64
            lastSequence uint16
            lastTimestamp uint32
            lastPLI time.Time
            mu sync.Mutex
        }
        stats := &PacketStats{
            lastPLI: time.Now(),
        }

        // Log stats periodically
        go func() {
            ticker := time.NewTicker(5 * time.Second)
            defer ticker.Stop()

            for range ticker.C {
                stats.mu.Lock()
                if stats.totalPackets > 0 {
                    lossRate := float64(stats.lostPackets-stats.recoveredPackets) / float64(stats.totalPackets) * 100
                    fmt.Printf("[%s] Track %s detailed stats:\n"+
                        "  Total packets: %d\n"+
                        "  Lost packets: %d\n"+
                        "  Recovered packets: %d\n"+
                        "  Net loss rate: %.2f%%\n"+
                        "  Out of order: %d\n"+
                        "  Duplicates: %d\n"+
                        "  PLI requests: %d\n",
                        b.sourceName, pub.SID(),
                        stats.totalPackets,
                        stats.lostPackets,
                        stats.recoveredPackets,
                        lossRate,
                        stats.outOfOrder,
                        stats.duplicates,
                        stats.pliCount)
                }
                stats.mu.Unlock()
            }
        }()

        // Function to request PLI
        requestPLI := func() {
            stats.mu.Lock()
            if time.Since(stats.lastPLI) > pliInterval {
                stats.lastPLI = time.Now()
                stats.pliCount++
                stats.mu.Unlock()
                
                fmt.Printf("[%s] Requesting PLI for track %s\n", b.sourceName, pub.SID())
            } else {
                stats.mu.Unlock()
            }
        }

        // Function to process packets from jitter buffer
        processJitterBuffer := func(currentSeq uint16) []*rtp.Packet {
            jitterBuffer.mu.Lock()
            defer jitterBuffer.mu.Unlock()

            var packets []*rtp.Packet
            now := time.Now()

            // Process all packets that are ready
            for seq, timestamp := range jitterBuffer.timestamps {
                if now.Sub(timestamp) > maxWaitTime {
                    if pkt, ok := jitterBuffer.packets[seq]; ok {
                        packets = append(packets, pkt)
                        delete(jitterBuffer.packets, seq)
                        delete(jitterBuffer.timestamps, seq)
                    }
                }
            }

            // Sort packets by sequence number
            sort.Slice(packets, func(i, j int) bool {
                return packets[i].SequenceNumber < packets[j].SequenceNumber
            })

            return packets
        }

        // Modify the packet processing loop:
        go func() {
            defer close(pktChan)
            var lastSequence uint16
            var firstPacket bool = true
            
            for {
                pkt, _, err := track.ReadRTP()
                if err != nil {
                    fmt.Printf("[%s] Error reading RTP: %v\n", b.sourceName, err)
                    time.Sleep(20 * time.Millisecond)
                    continue
                }

                if pkt == nil || len(pkt.Payload) == 0 {
                    continue
                }

                // Update packet stats
                stats.mu.Lock()
                stats.totalPackets++
                
                if !firstPacket {
                    diff := int32(pkt.SequenceNumber) - int32(lastSequence)
                    if diff < 0 && diff > -0x8000 {
                        // Out of order packet
                        stats.outOfOrder++
                        
                        // Add to jitter buffer
                        jitterBuffer.mu.Lock()
                        if len(jitterBuffer.packets) < maxJitterBufferSize {
                            jitterBuffer.packets[pkt.SequenceNumber] = pkt.Clone()
                            jitterBuffer.timestamps[pkt.SequenceNumber] = time.Now()
                        }
                        jitterBuffer.mu.Unlock()
                        
                    } else if diff == 0 {
                        stats.duplicates++
                    } else if diff > 1 && diff < 0x8000 {
                        missed := diff - 1
                        stats.lostPackets += uint64(missed)
                        
                        // Request PLI if gap is too large
                        if missed >= maxPacketGap {
                            requestPLI()
                        }
                        
                        fmt.Printf("[%s] Packet loss detected: missing %d packets (seq: %d -> %d)\n",
                            b.sourceName, missed, lastSequence, pkt.SequenceNumber)
                    }
                }
                firstPacket = false
                lastSequence = pkt.SequenceNumber
                stats.mu.Unlock()

                // Process packets from jitter buffer
                if bufferedPackets := processJitterBuffer(pkt.SequenceNumber); len(bufferedPackets) > 0 {
                    stats.mu.Lock()
                    stats.recoveredPackets += uint64(len(bufferedPackets))
                    stats.mu.Unlock()

                    for _, bufferedPkt := range bufferedPackets {
                        select {
                        case pktChan <- bufferedPkt:
                        default:
                            fmt.Printf("[%s] Warning: Buffer full, dropped recovered packet\n", b.sourceName)
                        }
                    }
                }

                // Forward current packet
                select {
                case pktChan <- pkt.Clone():
                default:
                    fmt.Printf("[%s] Warning: Buffer full, dropped packet\n", b.sourceName)
                }

                // Check if this is a key frame
                if isVP9KeyFrame(pkt.Payload) {
                    fmt.Printf("[%s] Received VP9 key frame\n", b.sourceName)
                }
            }
        }()
    }

    // Get or create target room connection for this participant
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

    // Mark this track as bridged
    b.bridgedTracks[rp.Identity()][pub.SID()] = true

    fmt.Printf("[%s] Forwarding track from %s to %s (participant: %s, track: %s, kind: %s, sid: %s)\n", 
        b.sourceName, b.sourceName, b.targetName, rp.Identity(), pub.Name(), pub.Kind(), pub.SID())

    // Create a local track using the codec from the remote track.
    codec := track.Codec()
    fmt.Printf("[%s] Creating local track with codec: %s for kind: %s\n", b.sourceName, codec.MimeType, pub.Kind())

    var localTrack *webrtc.TrackLocalStaticRTP
    var err error

    streamID := fmt.Sprintf("%s_%s_%s", rp.Identity(), pub.Kind(), pub.SID())
    trackID := fmt.Sprintf("%s_%s_%s", rp.Identity(), pub.Kind(), pub.SID())

    // For VP9, ensure we handle spatial layers properly
    if codec.MimeType == "video/VP9" {
        // Add VP9 specific parameters
        codec.RTPCodecCapability.SDPFmtpLine = "profile-id=0"
        
        // Add RTP header extensions for VP9 SVC
        codec.RTPCodecCapability.RTCPFeedback = []webrtc.RTCPFeedback{
            {Type: "goog-remb", Parameter: ""},
            {Type: "transport-cc", Parameter: ""},
            {Type: "ccm", Parameter: "fir"},
            {Type: "nack", Parameter: ""},
            {Type: "nack", Parameter: "pli"},
        }
    }

    localTrack, err = webrtc.NewTrackLocalStaticRTP(codec.RTPCodecCapability, trackID, streamID)
    if err != nil {
        fmt.Printf("[%s] Failed to create local track: %v\n", b.sourceName, err)
        return
    }

    // Set up publication options for the track
    pubOptions := &lksdk.TrackPublicationOptions{
        Name: pub.Name(),
        Source: pub.Source(),
    }
    
    if pub.Kind() == lksdk.TrackKindVideo {
        // For VP9, specify the video layers explicitly
        if codec.MimeType == "video/VP9" {
            // Use the correct field names and constants
            pubOptions.VideoWidth = 1280    // These are hints to the SFU
            pubOptions.VideoHeight = 720
            pubOptions.Source = lksdk.TrackSourceCamera  // Correct constant name
        }
    }

    targetPub, err := targetRoom.LocalParticipant.PublishTrack(localTrack, pubOptions)
    if err != nil {
        fmt.Printf("[%s] Failed to publish local track on %s: %v\n", b.sourceName, b.targetName, err)
        return
    }
    fmt.Printf("[%s] Successfully published track to %s with SID: %s\n", 
        b.sourceName, b.targetName, targetPub.SID())

    // Continuously forward RTP packets.
    go func() {
        fmt.Printf("[%s] Starting RTP forwarding for track %s (kind: %s)\n", 
            b.sourceName, pub.Name(), pub.Kind())
        packetCount := 0
        incomingCount := 0
        droppedCount := 0
        lastLogTime := time.Now()
        isVideo := pub.Kind() == lksdk.TrackKindVideo

        defer func() {
            fmt.Printf("[%s] Stopping RTP forwarding for track %s (kind: %s). Total received: %d, forwarded: %d, dropped: %d\n",
                b.sourceName, pub.Name(), pub.Kind(), incomingCount, packetCount, droppedCount)
            targetRoom.LocalParticipant.UnpublishTrack(targetPub.SID())
            if tracks, exists := b.bridgedTracks[rp.Identity()]; exists {
                delete(tracks, pub.SID())
            }
        }()

        // Create a buffered channel for packets
        const bufferSize = 3000 // Increased buffer size for VP9 SVC
        pktChan := make(chan *rtp.Packet, bufferSize)

        // Start a goroutine to read packets
        go func() {
            defer close(pktChan)
            var lastSequence uint16
            var frameStarted bool
            var currentFrame []*rtp.Packet
            var lastTimestamp uint32
            var frameCount uint32
            var consecutiveErrors int
            
            for {
                pkt, _, err := track.ReadRTP()
                if err != nil {
                    consecutiveErrors++
                    if consecutiveErrors > 5 {
                        fmt.Printf("[%s] Too many consecutive errors reading RTP: %v\n", b.sourceName, err)
                        return
                    }
                    // Sleep briefly to avoid tight loop on errors
                    time.Sleep(20 * time.Millisecond)
                    continue
                }
                consecutiveErrors = 0
                
                // Validate packet
                if pkt == nil || len(pkt.Payload) == 0 {
                    continue
                }

                incomingCount++

                if isVideo {
                    // For VP9, check for key frames but don't wait for them
                    if isVP9KeyFrame(pkt.Payload) {
                        fmt.Printf("[%s] Found VP9 key frame at frame %d\n", b.sourceName, frameCount)
                        frameStarted = true
                        lastTimestamp = pkt.Timestamp
                        
                        // Clear current frame buffer on key frame
                        if len(currentFrame) > 0 {
                            currentFrame = nil
                        }
                    }

                    // Handle frame boundaries
                    if pkt.Timestamp != lastTimestamp {
                        // New frame based on timestamp change
                        if len(currentFrame) > 0 {
                            // Forward all packets in the current frame together
                            frameCount++
                            for _, framePkt := range currentFrame {
                                select {
                                case pktChan <- framePkt:
                                default:
                                    droppedCount++
                                }
                            }
                            currentFrame = nil
                        }
                        frameStarted = true
                        lastTimestamp = pkt.Timestamp
                    }

                    // Handle sequence number wrapping
                    if lastSequence > 0 {
                        seqDiff := pkt.SequenceNumber - lastSequence
                        if seqDiff > 0x8000 {
                            // Sequence number wrapped around
                            fmt.Printf("[%s] Sequence number wrapped: %d -> %d\n", 
                                b.sourceName, lastSequence, pkt.SequenceNumber)
                        } else if seqDiff > 1 && seqDiff < 0x8000 {
                            // Packet loss detected
                            fmt.Printf("[%s] Packet loss detected: missing %d packets\n", 
                                b.sourceName, seqDiff-1)
                        }
                    }

                    // Add packet to current frame
                    pktCopy := pkt.Clone()
                    if frameStarted || len(currentFrame) > 0 {
                        currentFrame = append(currentFrame, pktCopy)
                        
                        // If this is a marker packet, it's the end of the frame
                        if pkt.Marker {
                            frameCount++
                            // Forward all packets in the frame together
                            for _, framePkt := range currentFrame {
                                select {
                                case pktChan <- framePkt:
                                default:
                                    droppedCount++
                                }
                            }
                            currentFrame = nil
                            frameStarted = false
                            
                            // Log frame stats periodically
                            if frameCount%300 == 0 { // Every ~10 seconds at 30fps
                                fmt.Printf("[%s] Processed %d frames for track %s\n", 
                                    b.sourceName, frameCount, pub.SID())
                            }
                        }
                    } else {
                        // Forward packets even if we haven't seen a frame start
                        select {
                        case pktChan <- pktCopy:
                        default:
                            droppedCount++
                        }
                    }

                    lastSequence = pkt.SequenceNumber
                } else {
                    // For audio, just forward directly with minimal delay
                    select {
                    case pktChan <- pkt.Clone():
                    default:
                        droppedCount++
                        if droppedCount%100 == 1 {
                            fmt.Printf("[%s] WARNING: Dropped packet for track %s (type: %s) - buffer full. Total dropped: %d\n",
                                b.sourceName, pub.Name(), pub.Kind(), droppedCount)
                        }
                    }
                }
            }
        }()

        // Process and forward packets with minimal buffering for video
        var lastPacketTime time.Time

        for pkt := range pktChan {
            now := time.Now()

            // Initialize on first packet
            if lastPacketTime.IsZero() {
                lastPacketTime = now
            }

            // Minimal spacing for video packets to prevent overwhelming the receiver
            if isVideo {
                // Ensure minimum spacing between packets (0.5ms)
                minSpacing := 500 * time.Microsecond
                if now.Sub(lastPacketTime) < minSpacing {
                    time.Sleep(minSpacing - now.Sub(lastPacketTime))
                }
            }

            if err := localTrack.WriteRTP(pkt); err != nil {
                fmt.Printf("[%s] Error writing RTP: %v\n", b.targetName, err)
                return
            }

            lastPacketTime = time.Now()
            packetCount++

            if time.Since(lastLogTime) >= time.Second {
                bufferUsage := len(pktChan)
                fmt.Printf("[%s] Track stats for %s (type: %s, sid: %s) - Received: %d/s, Forwarded: %d/s, Dropped: %d, Buffer: %d/%d\n",
                    b.sourceName, pub.Name(), pub.Kind(), pub.SID(), 
                    incomingCount, packetCount, droppedCount, bufferUsage, bufferSize)
                incomingCount = 0
                packetCount = 0
                lastLogTime = time.Now()
            }
        }
    }()

    // Track layer availability
    b.mu.Lock()
    if b.videoLayers == nil {
        b.videoLayers = make(map[string]map[uint8]bool)
    }
    b.videoLayers[pub.SID()] = make(map[uint8]bool)
    b.videoLayers[pub.SID()][0] = true // Base layer
    b.mu.Unlock()
}

// OnParticipantDisconnected is called when a participant leaves.
func (b *bridgeCallback) OnParticipantDisconnected(rp *lksdk.RemoteParticipant) {
    b.mu.Lock()
    defer b.mu.Unlock()

    // Clean up the participant's connection when they disconnect
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
            Room: roomName,
            Hidden: true,  // Make the monitor participants hidden
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
    // Create our callback instances.
    callbackA := newBridgeCallback("deploy-a", "deploy-b")
    callbackB := newBridgeCallback("deploy-b", "deploy-a")

    // Connect to both LiveKit rooms with monitor identities
    roomA := connectToRoom(deploymentA, apiKeyA, apiSecretA, "monitor-a", callbackA)
    roomB := connectToRoom(deploymentB, apiKeyB, apiSecretB, "monitor-b", callbackB)

    fmt.Println("Bridge running. Press Ctrl+C to exit.")

    // Wait for termination signal.
    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
    <-sigChan

    // Disconnect all rooms
    roomA.Disconnect()
    roomB.Disconnect()
    
    // Disconnect all participant connections
    for _, room := range callbackA.targetConnections {
        room.Disconnect()
    }
    for _, room := range callbackB.targetConnections {
        room.Disconnect()
    }
}
