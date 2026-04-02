package tts

import (
	"regexp"
	"strings"
)

// Sanitise prepares text for TTS (Piper) by smoothing out prosody and removing non-speech elements.
func Sanitise(text string, userName string, personaName string, phoneticName string) string {
	// 1. URL Handling: Replace URLs with "[link]" to avoid reading out long strings
	urlRe := regexp.MustCompile(`https?://\S+`)
	text = urlRe.ReplaceAllString(text, "[link]")

	// 2. Vocative Comma Enhancement: Replace comma before the last word of a sentence with an ellipsis
	// Pattern: "," [optional space] [word] [terminal punctuation]
	// This helps Piper introduce a natural pause (e.g., "Hello, Jason!" -> "Hello... Jason!")
	
	// vocativeRe := regexp.MustCompile(`,\s*(\w+)([.!?;:])`)
	// text = vocativeRe.ReplaceAllString(text, "... $1$2")

	// 3. User Name specific cleanup (case-insensitive)
	if userName != "" {
		nameRe := regexp.MustCompile("(?i),\\s*" + regexp.QuoteMeta(userName) + "\\b")
		// If it wasn't already caught by the vocative ellipsis above (e.g. if it's not at the end of a sentence),
		// we still want to strip the comma for better prosody.
		text = nameRe.ReplaceAllString(text, " "+userName)
	}

	// 4. Persona Phonetic Replacement (case-insensitive)
	if phoneticName != "" && personaName != "" {
		// Replace the persona's name with its phonetic pronunciation if provided
		pNameRe := regexp.MustCompile("(?i)\\b" + regexp.QuoteMeta(personaName) + "\\b")
		text = pNameRe.ReplaceAllString(text, phoneticName)
	}

	// 5. Drop markdown formatting characters that Piper would read aloud verbatim
	mdRe := regexp.MustCompile("[*_`#~>]")
	text = mdRe.ReplaceAllString(text, "")

	// 6. Handle numbered lists: "1. " -> "1 " at start of line or after whitespace
	// This helps Piper prosody (treats it like a label rather than a sentence end)
	listRe := regexp.MustCompile(`(?m)(^\d+)\. `)
	text = listRe.ReplaceAllString(text, "$1 ")

	// 7. Emoji Handling: Strip common emoji ranges
	// This is a broad stroke to avoid Piper trying to describe or skip erratically
	emojiRe := regexp.MustCompile(`[\x{1F300}-\x{1F9FF}]|[\x{2600}-\x{26FF}]|[\x{2700}-\x{27BF}]`)
	text = emojiRe.ReplaceAllString(text, "")

	// 8. Collapse any runs of whitespace (including newlines) into single spaces
	wsRe := regexp.MustCompile(`\s+`)
	text = wsRe.ReplaceAllString(text, " ")

	return strings.TrimSpace(text)
}
