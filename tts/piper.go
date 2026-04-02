package tts

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// SynthesizeLocal executes the local piper binary to generate a WAV file.
func SynthesizeLocal(piperBin, modelPath string, text string, noiseScale, lengthScale, noiseW float64) ([]byte, error) {
	randomVar := func(base float64) float64 {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return base
		}
		v := float64(binary.LittleEndian.Uint64(b)) / float64(1<<64)
		newVal := base + (v*0.16) - 0.08
		if newVal < 0.01 {
			newVal = 0.01
		}
		return newVal
	}

	ns := randomVar(noiseScale)
	nw := randomVar(noiseW)

	cmd := exec.Command(piperBin,
		"--model", modelPath,
		"--noise_scale", fmt.Sprintf("%f", ns),
		"--length_scale", fmt.Sprintf("%f", lengthScale),
		"--noise_w", fmt.Sprintf("%f", nw),
		"-f", "-")

	cmd.Stdin = strings.NewReader(text)
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("piper execution failed: %v, stderr: %s", err, stderr.String())
	}

	return out.Bytes(), nil
}

// RemotePiper handles a persistent connection to a piper-service.
type RemotePiper struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

// NewRemotePiper creates a new connection to a piper-service.
func NewRemotePiper(serviceURL string) (*RemotePiper, error) {
	u, err := url.Parse(serviceURL)
	if err != nil {
		return nil, err
	}

	// websocket.Dialer requires ws/wss scheme
	if u.Scheme == "http" {
		u.Scheme = "ws"
	} else if u.Scheme == "https" {
		u.Scheme = "wss"
	}

	// piper-service WebSocket endpoint is at /stream
	if u.Path == "" || u.Path == "/" {
		u.Path = "/stream"
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
	}

	conn, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to dial piper-service at %s: %v", u.String(), err)
	}

	return &RemotePiper{conn: conn}, nil
}

// Close closes the connection.
func (rp *RemotePiper) Close() error {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if rp.conn != nil {
		return rp.conn.Close()
	}
	return nil
}

// Stream sends text to the service and relays its output via callbacks.
func (rp *RemotePiper) Stream(ctx context.Context, model string, text string, annotated bool, lengthScale, noiseScale, noiseW, variance float64, onMetadata func(jsonStr string), onAudio func(pcm []byte)) error {
	rp.mu.Lock()
	defer rp.mu.Unlock()

	// Send request
	req := map[string]interface{}{
		"text":         text,
		"annotated":    annotated,
		"model":        model,
		"length_scale": lengthScale,
		"noise_scale":  noiseScale,
		"noise_w":      noiseW,
		"variance":     variance,
	}
	if err := rp.conn.WriteJSON(req); err != nil {
		return fmt.Errorf("failed to send TTS request: %v", err)
	}

	// Read loop for this specific request
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			messageType, p, err := rp.conn.ReadMessage()
			if err != nil {
				return err
			}

			switch messageType {
			case websocket.TextMessage:
				var meta map[string]interface{}
				if err := json.Unmarshal(p, &meta); err == nil {
					if meta["type"] == "end" {
						return nil
					}
					onMetadata(string(p))
				}
			case websocket.BinaryMessage:
				onAudio(p)
			}
		}
	}
}
