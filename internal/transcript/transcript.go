package transcript

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Image represents an extracted image from the transcript
type Image struct {
	Data      []byte // Raw image bytes
	MediaType string // e.g., "image/jpeg", "image/png"
	Filename  string // Generated filename
}

// ImageContent represents the structure of an image in the transcript
// Format: {"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"..."}}
type ImageContent struct {
	Type   string `json:"type"`
	Source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"`
}

// TranscriptMessage represents a message in the JSONL transcript
type TranscriptMessage struct {
	Type    string          `json:"type"`
	Role    string          `json:"role,omitempty"`
	Message json.RawMessage `json:"message,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
}

// MessageWithContent is for messages that have a content array
type MessageWithContent struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

// ContentTypeCheck is used to check the type field of content items
type ContentTypeCheck struct {
	Type string `json:"type"`
}

// isActualUserPrompt checks if the content contains actual user prompt items (text/image)
// rather than tool_result items. Tool results are sent by Claude Code with type="user"
// but they don't contain user prompts - they contain tool execution results.
func isActualUserPrompt(content []json.RawMessage) bool {
	for _, item := range content {
		var check ContentTypeCheck
		if err := json.Unmarshal(item, &check); err != nil {
			continue
		}
		// If we find a text or image type, this is an actual user prompt
		if check.Type == "text" || check.Type == "image" {
			return true
		}
	}
	return false
}

// ExtractLatestImages reads the transcript file and extracts images from the most recent user message
func ExtractLatestImages(transcriptPath string) ([]Image, error) {
	if transcriptPath == "" {
		return nil, nil
	}

	// Verify file exists
	if _, err := os.Stat(transcriptPath); os.IsNotExist(err) {
		return nil, nil
	}

	file, err := os.Open(transcriptPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open transcript: %w", err)
	}
	defer file.Close()

	// Read all lines to find the last user message with images
	var lastUserMessage *MessageWithContent
	scanner := bufio.NewScanner(file)
	// Increase buffer size for large messages with images
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 10*1024*1024) // 10MB max line size

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Parse the line as a transcript message
		var msg TranscriptMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}

		// Look for user messages (type can be "user", "human", or role can be "user")
		if msg.Type == "user" || msg.Type == "human" || msg.Role == "user" {
			// Try to parse as MessageWithContent from the message field
			var content MessageWithContent
			if msg.Message != nil {
				if err := json.Unmarshal(msg.Message, &content); err == nil && len(content.Content) > 0 {
					// Only consider this if it's an actual user prompt (not tool_result)
					if isActualUserPrompt(content.Content) {
						lastUserMessage = &content
					}
				}
			} else if msg.Content != nil {
				// Direct content array
				content.Role = "user"
				var contentArray []json.RawMessage
				if err := json.Unmarshal(msg.Content, &contentArray); err == nil {
					// Only consider this if it's an actual user prompt (not tool_result)
					if isActualUserPrompt(contentArray) {
						content.Content = contentArray
						lastUserMessage = &content
					}
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading transcript: %w", err)
	}

	if lastUserMessage == nil || len(lastUserMessage.Content) == 0 {
		return nil, nil
	}

	// Extract images from the content array
	var images []Image
	imageCount := 0

	for _, contentItem := range lastUserMessage.Content {
		var img ImageContent
		if err := json.Unmarshal(contentItem, &img); err != nil {
			continue
		}

		if img.Type != "image" || img.Source.Type != "base64" || img.Source.Data == "" {
			continue
		}

		// Decode base64 data
		imageData, err := base64.StdEncoding.DecodeString(img.Source.Data)
		if err != nil {
			continue
		}

		// Generate filename based on media type
		imageCount++
		ext := getExtensionForMediaType(img.Source.MediaType)
		filename := fmt.Sprintf("image_%d%s", imageCount, ext)

		images = append(images, Image{
			Data:      imageData,
			MediaType: img.Source.MediaType,
			Filename:  filename,
		})
	}

	return images, nil
}

// ExtractPromptText extracts the text content from the latest user message
func ExtractPromptText(transcriptPath string) (string, error) {
	if transcriptPath == "" {
		return "", nil
	}

	if _, err := os.Stat(transcriptPath); os.IsNotExist(err) {
		return "", nil
	}

	file, err := os.Open(transcriptPath)
	if err != nil {
		return "", fmt.Errorf("failed to open transcript: %w", err)
	}
	defer file.Close()

	var lastText string
	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg TranscriptMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}

		if msg.Type == "user" || msg.Type == "human" || msg.Role == "user" {
			var content MessageWithContent
			var rawContent []json.RawMessage

			if msg.Message != nil {
				if err := json.Unmarshal(msg.Message, &content); err == nil && len(content.Content) > 0 {
					rawContent = content.Content
				}
			} else if msg.Content != nil {
				json.Unmarshal(msg.Content, &rawContent)
			}

			// Only process actual user prompts (skip tool_result messages)
			if !isActualUserPrompt(rawContent) {
				continue
			}

			// Extract text from content items
			for _, item := range rawContent {
				var textContent struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if err := json.Unmarshal(item, &textContent); err == nil && textContent.Type == "text" {
					lastText = textContent.Text
				}
			}
		}
	}

	return lastText, nil
}

// getExtensionForMediaType returns the file extension for a given MIME type
func getExtensionForMediaType(mediaType string) string {
	switch strings.ToLower(mediaType) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/svg+xml":
		return ".svg"
	default:
		return ".bin"
	}
}

// GetTranscriptDir returns the directory containing the transcript
func GetTranscriptDir(transcriptPath string) string {
	return filepath.Dir(transcriptPath)
}
