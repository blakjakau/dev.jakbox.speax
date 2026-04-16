package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var (
	timersMutex sync.Mutex
)

func getTimersPath(clientID string) string {
	return filepath.Join(getContextDir(clientID), "timers.json")
}

func loadTimersUnsafe(clientID string) ([]*Timer, error) {
	path := getTimersPath(clientID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []*Timer{}, nil
		}
		return nil, err
	}
	var timers []*Timer
	if err := json.Unmarshal(data, &timers); err != nil {
		return nil, err
	}
	return timers, nil
}

func saveTimersUnsafe(clientID string, timers []*Timer) error {
	path := getTimersPath(clientID)
	os.MkdirAll(filepath.Dir(path), 0755)
	data, err := json.MarshalIndent(timers, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func addTimer(clientID string, timer *Timer) error {
	timersMutex.Lock()
	defer timersMutex.Unlock()

	timers, err := loadTimersUnsafe(clientID)
	if err != nil {
		return err
	}
	timers = append(timers, timer)
	return saveTimersUnsafe(clientID, timers)
}

func startTimerManager() {
	ticker := time.NewTicker(10 * time.Second)
	go func() {
		for range ticker.C {
			checkTimers()
		}
	}()
	log.Println("Timer Manager started (polling every 10s)")
}

func checkTimers() {
	activeSessionsMutex.Lock()
	sessions := make([]*ClientSession, 0, len(activeSessions))
	for _, s := range activeSessions {
		sessions = append(sessions, s)
	}
	activeSessionsMutex.Unlock()

	for _, s := range sessions {
		processSessionTimers(s)
	}
}

func processSessionTimers(s *ClientSession) {
	timersMutex.Lock()
	timers, err := loadTimersUnsafe(s.ClientID)
	if err != nil {
		timersMutex.Unlock()
		return
	}

	now := time.Now()
	changed := false
	var expiredTimers []*Timer

	for _, t := range timers {
		if !t.Expired && now.After(t.TriggerTime) {
			t.Expired = true
			expiredTimers = append(expiredTimers, t)
			changed = true
		}
	}

	if changed {
		saveTimersUnsafe(s.ClientID, timers)
	}
	timersMutex.Unlock()

	if len(expiredTimers) == 0 {
		return
	}

	// Check if user is connected (human clients only)
	var ws *websocket.Conn
	s.ConnMutex.Lock()
	for conn, meta := range s.Conns {
		if meta.ClientType != "tool" {
			ws = conn
			break
		}
	}
	s.ConnMutex.Unlock()
	connected := ws != nil

	if connected {
		for _, t := range expiredTimers {
			injectTimerExpiry(s, t, ws)
		}
	} else {
		// Mark as missed if not connected
		timersMutex.Lock()
		timers, _ := loadTimersUnsafe(s.ClientID)
		for _, et := range expiredTimers {
			for _, t := range timers {
				if t.ID == et.ID {
					t.Missed = true
					break
				}
			}
		}
		saveTimersUnsafe(s.ClientID, timers)
		timersMutex.Unlock()
		log.Printf("[Timer] Timer(s) expired while user %s was offline. Marked as missed.", s.ClientID)

		if s.HasPush() {
			go func() {
				for _, t := range expiredTimers {
					prompt := fmt.Sprintf("A timer you set for the user with the label '%s' has just expired while they are offline. Instead of acknowledging this as a system event, frame it as a natural, conversational follow-up. Use a casual, helpful tone (e.g. 'Just a quick check-in regarding [Label]...') and ensure the message is extremely brief.", t.Label)
					body := s.generateShortAssistantMessage(prompt)
					if body == "" {
						body = fmt.Sprintf("Timer Expired: %s", t.Label)
					}

					title := "Speax Alert"
					threadID := t.ThreadID
					personaName := extractVoiceName(s.Voice)

					if s.FCMToken != "" {
						if err := sendFCMPush(s.FCMToken, title, body, threadID, personaName); err != nil {
							log.Printf("[Push] FCM error for %s: %v", s.ClientID, err)
						} else {
							log.Printf("[Push] Sent FCM alert to %s", s.ClientID)
						}
					}
					if s.VapidSub != "" {
						if err := sendVapidPush(s.VapidSub, title, body, threadID, personaName); err != nil {
							log.Printf("[Push] VAPID error for %s: %v", s.ClientID, err)
						} else {
							log.Printf("[Push] Sent VAPID alert to %s", s.ClientID)
						}
					}
				}
			}()
		}
	}
}

func injectTimerExpiry(s *ClientSession, t *Timer, ws *websocket.Conn) {
	s.Mutex.Lock()
	thread, ok := s.Threads[t.ThreadID]
	if !ok {
		thread = s.ActiveThread() // Fallback to active thread
	}

	content := fmt.Sprintf("[SYSTEM] Timer Expired: %s", t.Label)
	s.appendMessage("system", content, thread)
	s.Mutex.Unlock()

	saveSession(s)

	log.Printf("[Timer] Timer expired for %s: %s. Triggering response.", s.ClientID, t.Label)

	// Trigger LLM to respond to the timer
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		s.Mutex.Lock()
		s.ActiveCancel = cancel
		s.Mutex.Unlock()

		prompt := fmt.Sprintf("[SYSTEM: The timer for '%s' has expired. Instead of acknowledging this as a system event, frame it as a natural, conversational follow-up to our current topic. Use a casual, helpful tone (e.g. 'By the way, as a quick reminder regarding [Label]...' or 'Just to keep us on track, [Label].') and ensure the transition is seamless without mentioning system mechanics, triggers, or expiration.]", t.Label)
		if err := streamLLMAndTTS(ctx, prompt, ws, s); err != nil {
			log.Printf("[Timer] Error streaming LLM response for timer: %v", err)
		}
	}()
}

func checkMissedTimers(s *ClientSession, ws *websocket.Conn) {
	timersMutex.Lock()
	timers, err := loadTimersUnsafe(s.ClientID)
	if err != nil || len(timers) == 0 {
		timersMutex.Unlock()
		return
	}

	missed := []*Timer{}
	remaining := []*Timer{}
	for _, t := range timers {
		if t.Missed {
			missed = append(missed, t)
		} else if !t.Expired {
			remaining = append(remaining, t)
		}
		// We drop old expired non-missed timers from the persistent file to keep it clean
	}

	if len(missed) == 0 {
		timersMutex.Unlock()
		return
	}

	// Sort missed by trigger time
	sort.Slice(missed, func(i, j int) bool {
		return missed[i].TriggerTime.Before(missed[j].TriggerTime)
	})

	// Batch fire missed timers
	var sb strings.Builder
	sb.WriteString("[SYSTEM] The following timers expired while the user was offline:\n")
	for _, t := range missed {
		sb.WriteString(fmt.Sprintf("- [%s] %s\n", t.TriggerTime.Format("2006-01-02 15:04:05"), t.Label))
	}

	// Update storage: remove missed (now fired)
	saveTimersUnsafe(s.ClientID, remaining)
	timersMutex.Unlock()

	s.Mutex.Lock()
	thread := s.ActiveThread()
	content := sb.String()
	s.appendMessage("system", content, thread)
	s.Mutex.Unlock()

	saveSession(s)

	log.Printf("[Timer] Fired %d missed timers for %s", len(missed), s.ClientID)

	// Trigger response
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		s.Mutex.Lock()
		s.ActiveCancel = cancel
		s.Mutex.Unlock()

		prompt := "[SYSTEM: Multiple timers expired while the user was offline. Instead of acknowledging these as system events, frame the information as a natural, conversational follow-up. Use a casual, helpful tone to bring them back up to speed on these items seamlessly.]"
		if err := streamLLMAndTTS(ctx, prompt, ws, s); err != nil {
			log.Printf("[Timer] Error streaming LLM response for missed timers: %v", err)
		}
	}()
}
