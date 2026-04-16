package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/SherClockHolmes/webpush-go"
	"golang.org/x/oauth2/google"
)

var fcmHttpClient *http.Client
var fcmProjectID string

type VapidKeySet struct {
	PublicKey  string `json:"public_key"`
	PrivateKey string `json:"private_key"`
}

var vapidKeys *VapidKeySet

func initPushManager() {
	// 1. Initialize FCM (REST v1 instead of SDK)
	ctx := context.Background()
	credsPath := "firebase-adminsdk.json"
	data, err := os.ReadFile(credsPath)
	if err != nil {
		log.Printf("[Push] Failed to read %s: %v", credsPath, err)
	} else {
		var sa struct {
			ProjectID string `json:"project_id"`
		}
		if err := json.Unmarshal(data, &sa); err == nil {
			fcmProjectID = sa.ProjectID
			conf, err := google.JWTConfigFromJSON(data, "https://www.googleapis.com/auth/firebase.messaging")
			if err != nil {
				log.Printf("[Push] Failed to create JWT config: %v", err)
			} else {
				fcmHttpClient = conf.Client(ctx)
				log.Printf("[Push] FCM initialized successfully for project: %s", fcmProjectID)
			}
		} else {
			log.Printf("[Push] Failed to parse %s: %v", credsPath, err)
		}
	}

	// 2. Initialize VAPID
	keysPath := filepath.Join(".", "context", "vapid_keys.json")
	if data, err := os.ReadFile(keysPath); err == nil {
		if json.Unmarshal(data, &vapidKeys) != nil {
			log.Printf("[Push] Failed to parse VAPID keys: %v", err)
		}
	}

	if vapidKeys == nil {
		log.Println("[Push] No VAPID keys found, generating new ones...")
		priv, pub, err := webpush.GenerateVAPIDKeys()
		if err != nil {
			log.Printf("[Push] Failed to generate VAPID keys: %v", err)
		} else {
			vapidKeys = &VapidKeySet{
				PrivateKey: priv,
				PublicKey:  pub,
			}
			data, _ := json.MarshalIndent(vapidKeys, "", "  ")
			os.MkdirAll(filepath.Dir(keysPath), 0755)
			os.WriteFile(keysPath, data, 0644)
			log.Println("[Push] VAPID keys generated and saved")
		}
	}
}

func sendFCMPush(token, title, body, threadID, personaName string) error {
	if fcmHttpClient == nil || fcmProjectID == "" {
		return fmt.Errorf("FCM client not initialized")
	}

	type fcmMessage struct {
		Message struct {
			Token        string `json:"token"`
			Notification struct {
				Title string `json:"title"`
				Body  string `json:"body"`
			} `json:"notification"`
			Data map[string]string `json:"data"`
			Android struct {
				Priority string `json:"priority"`
			} `json:"android"`
		} `json:"message"`
	}

	msg := fcmMessage{}
	msg.Message.Token = token
	msg.Message.Notification.Title = title
	msg.Message.Notification.Body = body
	msg.Message.Data = map[string]string{
		"threadId":    threadID,
		"personaName": personaName,
	}
	msg.Message.Android.Priority = "high"

	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("https://fcm.googleapis.com/v1/projects/%s/messages:send", fcmProjectID)
	resp, err := fcmHttpClient.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errData interface{}
		json.NewDecoder(resp.Body).Decode(&errData)
		return fmt.Errorf("FCM request failed with status %d: %v", resp.StatusCode, errData)
	}

	return nil
}

func sendVapidPush(subJSON, title, body, threadID, personaName string) error {
	if vapidKeys == nil {
		return fmt.Errorf("VAPID keys not initialized")
	}

	var s webpush.Subscription
	if err := json.Unmarshal([]byte(subJSON), &s); err != nil {
		return err
	}

	payload := map[string]string{
		"title":       title,
		"body":        body,
		"url":         fmt.Sprintf("/?threadId=%s", threadID),
		"personaName": personaName,
	}
	payloadBytes, _ := json.Marshal(payload)

	resp, err := webpush.SendNotification(payloadBytes, &s, &webpush.Options{
		Subscriber:      "abc@example.com", // Should be configured
		VAPIDPublicKey:  vapidKeys.PublicKey,
		VAPIDPrivateKey: vapidKeys.PrivateKey,
		TTL:             30,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func handleGetVapidPublicKey(w http.ResponseWriter, r *http.Request) {
	if vapidKeys == nil {
		http.Error(w, "VAPID not initialized", http.StatusInternalServerError)
		return
	}
	w.Write([]byte(vapidKeys.PublicKey))
}
