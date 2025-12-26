package transcript

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Attachment represents an extracted file from the transcript (image, PDF, document, etc.)
type Attachment struct {
	Data      []byte // Raw file bytes
	MediaType string // e.g., "image/jpeg", "application/pdf"
	Filename  string // Generated filename
	Type      string // Content type: "image", "document", "file"
}

// AttachmentContent represents the structure of an attachment in the transcript
// Images: {"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"..."}}
// Documents: {"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"..."}}
type AttachmentContent struct {
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

// isActualUserPrompt checks if the content contains actual user prompt items
// rather than tool-related items. Tool results are sent by Claude Code with type="user"
// but they don't contain user prompts - they contain tool execution results.
// Uses blocklist approach: skip tool_result/tool_use, accept everything else.
func isActualUserPrompt(content []json.RawMessage) bool {
	for _, item := range content {
		var check ContentTypeCheck
		if err := json.Unmarshal(item, &check); err != nil {
			continue
		}
		// Skip tool-related content types
		if check.Type == "tool_result" || check.Type == "tool_use" {
			continue
		}
		// Any other type (text, image, document, file, etc.) is actual user content
		if check.Type != "" {
			return true
		}
	}
	return false
}

// isAttachmentType returns true if the content type represents a file attachment
func isAttachmentType(contentType string) bool {
	switch contentType {
	case "image", "document", "file", "pdf":
		return true
	default:
		return false
	}
}

// hasAttachmentContent checks if the content array contains any attachment types
func hasAttachmentContent(content []json.RawMessage) bool {
	for _, item := range content {
		var check ContentTypeCheck
		if err := json.Unmarshal(item, &check); err != nil {
			continue
		}
		if isAttachmentType(check.Type) {
			return true
		}
	}
	return false
}

// ExtractLatestAttachments reads the transcript file and extracts all attachments
// (images, documents, PDFs, files) from the most recent user message that contains attachments
func ExtractLatestAttachments(transcriptPath string) ([]Attachment, error) {
	if transcriptPath == "" {
		return nil, nil
	}

	// Verify file exists
	if _, err := os.Stat(transcriptPath); os.IsNotExist(err) {
		return nil, nil
	}

	// Try extraction with retries - the hook may fire before the transcript is fully written
	var attachments []Attachment
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		attachments, err = extractAttachmentsFromFile(transcriptPath)
		if err != nil || len(attachments) > 0 {
			return attachments, err
		}
		// Wait a bit for transcript to be written (50ms, 100ms)
		if attempt < 2 {
			sleepMs := (attempt + 1) * 50
			time.Sleep(time.Duration(sleepMs) * time.Millisecond)
		}
	}
	return attachments, err
}

// extractAttachmentsFromFile does the actual extraction work
func extractAttachmentsFromFile(transcriptPath string) ([]Attachment, error) {
	file, err := os.Open(transcriptPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open transcript: %w", err)
	}
	defer file.Close()

	// Read all lines to find the last user message WITH ATTACHMENTS
	// Claude Code writes image metadata as a separate text-only message after the actual image,
	// so we need to specifically look for messages that contain attachment content types
	var lastUserMessageWithAttachments *MessageWithContent
	scanner := bufio.NewScanner(file)
	// Increase buffer size for large messages with attachments
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
					// AND it contains attachment content (image, document, file)
					if isActualUserPrompt(content.Content) && hasAttachmentContent(content.Content) {
						lastUserMessageWithAttachments = &content
					}
				}
			} else if msg.Content != nil {
				// Direct content array
				content.Role = "user"
				var contentArray []json.RawMessage
				if err := json.Unmarshal(msg.Content, &contentArray); err == nil {
					// Only consider this if it's an actual user prompt (not tool_result)
					// AND it contains attachment content
					if isActualUserPrompt(contentArray) && hasAttachmentContent(contentArray) {
						content.Content = contentArray
						lastUserMessageWithAttachments = &content
					}
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading transcript: %w", err)
	}

	if lastUserMessageWithAttachments == nil || len(lastUserMessageWithAttachments.Content) == 0 {
		return nil, nil
	}

	// Use the message with attachments
	lastUserMessage := lastUserMessageWithAttachments

	// Extract all attachments from the content array
	var attachments []Attachment
	counts := make(map[string]int) // Track count per type for filenames

	for _, contentItem := range lastUserMessage.Content {
		var att AttachmentContent
		if err := json.Unmarshal(contentItem, &att); err != nil {
			continue
		}

		// Check if this is an attachment type with base64 data
		if !isAttachmentType(att.Type) || att.Source.Type != "base64" || att.Source.Data == "" {
			continue
		}

		// Decode base64 data
		data, err := base64.StdEncoding.DecodeString(att.Source.Data)
		if err != nil {
			continue
		}

		// Generate filename based on type and media type
		counts[att.Type]++
		ext := getExtensionForMediaType(att.Source.MediaType)
		filename := fmt.Sprintf("%s_%d%s", att.Type, counts[att.Type], ext)

		attachments = append(attachments, Attachment{
			Data:      data,
			MediaType: att.Source.MediaType,
			Filename:  filename,
			Type:      att.Type,
		})
	}

	return attachments, nil
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
	// Images
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
	case "image/bmp":
		return ".bmp"
	case "image/tiff":
		return ".tiff"
	case "image/heic", "image/heif":
		return ".heic"

	// Documents
	case "application/pdf":
		return ".pdf"
	case "application/msword":
		return ".doc"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return ".docx"
	case "application/vnd.ms-excel":
		return ".xls"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return ".xlsx"
	case "application/vnd.ms-powerpoint":
		return ".ppt"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return ".pptx"

	// Text
	case "text/plain":
		return ".txt"
	case "text/csv":
		return ".csv"
	case "text/html":
		return ".html"
	case "text/markdown":
		return ".md"
	case "application/json":
		return ".json"
	case "application/xml", "text/xml":
		return ".xml"

	// Archives
	case "application/zip":
		return ".zip"
	case "application/gzip":
		return ".gz"

	default:
		return ".bin"
	}
}

// GetTranscriptDir returns the directory containing the transcript
func GetTranscriptDir(transcriptPath string) string {
	return filepath.Dir(transcriptPath)
}
