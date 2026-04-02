package api

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type sessionState struct {
	firstChunkSent bool
	isAnnotated    bool
	ls, ns, nw, varp float32
}

func (api *API) handleStream(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Websocket upgrade failed: %v", err)
		return
	}
	defer func() {
		log.Printf("Websocket client disconnected: %v", conn.RemoteAddr())
		conn.Close()
	}()
	log.Printf("Websocket client connected: %v", conn.RemoteAddr())

	// Per-connection state
	textBuffer := ""
	state := &sessionState{
		firstChunkSent: false,
		isAnnotated:    false,
	}
	textInput := make(chan struct {
		text      string
		annotated bool
		ls, ns, nw, varp float32
	}, 100)
	done := make(chan struct{})

	// Timer for 1500ms inactivity flush
	flushTimer := time.NewTimer(1500 * time.Millisecond)
	flushTimer.Stop()

	// Synthesis & Processing Processor
	go func() {
		defer close(done)
		for {
			select {
			case req, ok := <-textInput:
				if !ok {
					// Final flush before closing
					log.Printf("[WS] Channel closed, performing final flush of %d chars", len(textBuffer))
					api.flushRemaining(conn, &textBuffer, state, state.ls, state.ns, state.nw, state.varp)
					return
				}

				if req.annotated {
					state.isAnnotated = true
				}
				// Update current synthesis state from request
				state.ls, state.ns, state.nw, state.varp = req.ls, req.ns, req.nw, req.varp

				log.Printf("[WS] Received text chunk: '%s'", req.text)

				// Input received, stop the timer and ensure it's drained
				if !flushTimer.Stop() {
					select {
					case <-flushTimer.C:
					default:
					}
				}

				textBuffer += req.text

				// Iteratively process buffers as boundaries are identified (the "gates")
				for {
					minLength := 10
					hardLimit := 350

					splitIdx := findSentenceBoundary(textBuffer, minLength, hardLimit)
					if splitIdx == -1 {
						break // Buffer incomplete, wait for more text or inactivity
					}

					segment := textBuffer[:splitIdx+1]
					textBuffer = textBuffer[splitIdx+1:]

					log.Printf("[WS] Gated segment identified: '%s' (Stage 1: %v)", segment, !state.firstChunkSent)
					api.synthesizeAndSend(conn, segment, state, state.ls, state.ns, state.nw, state.varp)
				}

				// Always start/restart the inactivity timer unconditionally
				// to ensure we can officially close the stream.
				flushTimer.Reset(1500 * time.Millisecond)

			case <-flushTimer.C:
				if len(textBuffer) > 0 {
					log.Printf("[WS] Inactivity flush trigger: '%s'", textBuffer)
					api.flushRemaining(conn, &textBuffer, state, state.ls, state.ns, state.nw, state.varp)
				} else if state.firstChunkSent {
					log.Printf("[WS] Inactivity end trigger: stream completed")
					api.finalizeStream(conn, state)
				}
			}
		}
	}()

	// Reader Loop
	for {
		messageType, p, err := conn.ReadMessage()
		if err != nil {
			close(textInput)
			break
		}

		if messageType != websocket.TextMessage {
			continue
		}

		var req TTSRequest
		if err := json.Unmarshal(p, &req); err != nil {
			log.Printf("WebSocket invalid json: %v", err)
			continue
		}

		// Handle explicit model switching if provided
		if req.Model != "" {
			log.Printf("[WS] Switching model to: %s", req.Model)
			if err := api.Manager.LoadModel(req.Model); err != nil {
				log.Printf("[WS] Failed to switch model: %v", err)
				// Don't abort, just log and continue as it might just be the current model already
			}
		}

		// Handle diagnostic commands
		if req.Cmd != "" {
			switch req.Cmd {
			case "health":
				log.Printf("[WS] Command received: health")
				health := api.GetHealthMetrics()
				if p, err := json.Marshal(health); err == nil {
					api.Manager.RecordBytes(0, uint64(len(p)))
					conn.WriteMessage(websocket.TextMessage, p)
				}
			case "status":
				log.Printf("[WS] Command received: status")
				active, cache, totalSize, metrics := api.Manager.GetDetailedStatus()
				status := map[string]interface{}{
					"active_model":              active,
					"cached_models":             cache,
					"total_cache_size_estimate": totalSize,
					"metrics":                   metrics,
				}
				if p, err := json.Marshal(status); err == nil {
					api.Manager.RecordBytes(0, uint64(len(p)))
					conn.WriteMessage(websocket.TextMessage, p)
				}
			}
		}

		if req.Text != "" {
			ls, ns, nw := api.getSynthesisParams(req)
			textInput <- struct {
				text             string
				annotated        bool
				ls, ns, nw, varp float32
			}{req.Text, req.Annotated, ls, ns, nw, req.Variance}
		}

		api.Manager.RecordBytes(uint64(len(p)), 0)
	}
	<-done
}

// synthesizeAndSend performs core synthesis and writes binary data to WS immediately.
func (api *API) synthesizeAndSend(conn *websocket.Conn, text string, state *sessionState, baseLS, baseNS, baseNW, variance float32) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	log.Printf("[WS] Engine Synthesis Start: '%s'", text)

	if state.isAnnotated {
		textMap := map[string]interface{}{
			"type": "text",
			"text": text,
		}
		if p, err := json.Marshal(textMap); err == nil {
			api.Manager.RecordBytes(0, uint64(len(p)))
			conn.WriteMessage(websocket.TextMessage, p)
		}
	}
	
	// Apply per-sentence variance (Length-weighted for LS)
	ls := applyLengthVariance(baseLS, variance, text)
	ns := applyParameterVariance(baseNS, variance)
	nw := applyParameterVariance(baseNW, variance)

	err := api.Manager.SynthesizeStream(text, ls, ns, nw, func(audioData []int16) {
		if len(audioData) == 0 {
			return
		}

		if !state.firstChunkSent {
			srMap := map[string]interface{}{
				"type":       "start",
				"sampleRate": api.Manager.GetSampleRate(),
			}
			if p, err := json.Marshal(srMap); err == nil {
				api.Manager.RecordBytes(0, uint64(len(p)))
				conn.WriteMessage(websocket.TextMessage, p)
			}
			state.firstChunkSent = true
		}

		log.Printf("[WS] Binary Chunk: sending %d samples", len(audioData))

		// Convert to raw bytes to stream as binary message
		buf := new(bytes.Buffer)
		for _, v := range audioData {
			binary.Write(buf, binary.LittleEndian, v)
		}

		resBytes := buf.Bytes()
		api.Manager.RecordBytes(0, uint64(len(resBytes)))
		if err := conn.WriteMessage(websocket.BinaryMessage, resBytes); err != nil {
			log.Printf("Failed to write to WS: %v", err)
		}
	})

	if err != nil {
		log.Printf("Streaming synthesis failed on WS: %v", err)
		errResp := map[string]string{"error": err.Error()}
		if p, err := json.Marshal(errResp); err == nil {
			api.Manager.RecordBytes(0, uint64(len(p)))
			conn.WriteMessage(websocket.TextMessage, p)
		}
		return
	}
	log.Printf("[WS] Engine Synthesis Complete: '%s'", text)
}

// flushRemaining consumes the rest of the buffer after a timeout or connection end.
func (api *API) flushRemaining(conn *websocket.Conn, buffer *string, state *sessionState, ls, ns, nw, variance float32) {
	text := *buffer
	if text == "" {
		return
	}

	log.Printf("[WS] Flushing remaining text (%d chars). Splitting into sentences.", len(text))

	// Even during a flush, we split by sentence to avoid Piper truncation issues
Loop:
	for {
		if text == "" {
			break
		}

		// Use Stage 1 rules (aggressive splitting) for the flush to ensure nothing is missed
		splitIdx := findSentenceBoundary(text, 0, 350)
		if splitIdx == -1 {
			// No more boundaries, synthesize the rest as one last chunk
			api.synthesizeAndSend(conn, text, state, ls, ns, nw, variance)
			break Loop
		}

		segment := text[:splitIdx+1]
		text = text[splitIdx+1:]

		api.synthesizeAndSend(conn, segment, state, ls, ns, nw, variance)
	}

	*buffer = ""
	api.finalizeStream(conn, state)
}

// finalizeStream formally ends the stream block by sending a trailing JSON message.
func (api *API) finalizeStream(conn *websocket.Conn, state *sessionState) {
	if !state.firstChunkSent {
		return // Nothing was ever sent, so don't emit end
	}
	endMap := map[string]interface{}{"type": "end"}
	if p, err := json.Marshal(endMap); err == nil {
		api.Manager.RecordBytes(0, uint64(len(p)))
		conn.WriteMessage(websocket.TextMessage, p)
	}
	state.firstChunkSent = false // Reset gate for an entirely new utterance
	state.isAnnotated = false    // Reset annotated mode for the next block
}
