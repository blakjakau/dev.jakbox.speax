package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/jakbox/speax/tts/piper-service/api"
	"github.com/jakbox/speax/tts/piper-service/piper"
)

func main() {
	port := flag.String("port", "4410", "Port to run the service on")
	modelsDir := flag.String("models", "./models", "Directory containing .onnx voice models")
	espeakData := flag.String("espeak", "./piper/espeak-ng-data", "Path to eSpeak NG data directory")
	flag.Parse()

	log.Printf("Starting Piper TTS Service on port %s", *port)
	log.Printf("Models directory: %s", *modelsDir)

	// Ensure models directory exists
	if _, err := os.Stat(*modelsDir); os.IsNotExist(err) {
		log.Fatalf("Models directory does not exist: %v", err)
	}

	// Initialize Piper Engine
	piper.Initialize(*espeakData)
	defer piper.Terminate()

	// Initialize Piper Manager
	manager := piper.NewManager(*modelsDir, *espeakData)
	defer manager.Close()

	// Find the first available alphabetical .onnx model to preload
	entries, err := os.ReadDir(*modelsDir)
	if err == nil {
		var firstModel string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".onnx") {
				firstModel = e.Name()
				break
			}
		}
		
		if firstModel != "" {
			log.Printf("Auto-loading initial model: %s", firstModel)
			if err := manager.LoadModel(firstModel); err != nil {
				log.Printf("Warning: failed to preload model %s: %v", firstModel, err)
			}
		} else {
			log.Println("No .onnx models found in models directory")
		}
	} else {
		log.Printf("Failed to read models directory: %v", err)
	}

	// Initialize API router
	router := api.NewRouter(manager, *modelsDir)

	log.Printf("Service is ready. Listening on :%s", *port)
	if err := http.ListenAndServe(fmt.Sprintf(":%s", *port), router); err != nil {
		log.Fatalf("HTTP server error: %v", err)
	}
}
