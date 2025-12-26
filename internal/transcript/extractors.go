package transcript

import (
	"encoding/base64"
	"fmt"
	"time"
)

// AttachmentExtractor defines the interface for tool-specific attachment extraction
type AttachmentExtractor interface {
	// ExtractAttachments extracts all attachments (images, documents, files) from the tool's storage format
	// Returns extracted attachments and the text prompt (if available)
	ExtractAttachments(nativeEvent map[string]interface{}) ([]Attachment, string, error)

	// SupportsAttachments returns whether this tool supports attachment extraction
	SupportsAttachments() bool
}

// GetExtractor returns the appropriate attachment extractor for the given tool
func GetExtractor(tool string) AttachmentExtractor {
	switch tool {
	case "claude-code", "cline": // Cline uses Claude Code compatible hooks
		return &ClaudeCodeExtractor{}
	case "cursor":
		return &CursorExtractor{}
	case "gemini-cli":
		return &GeminiExtractor{}
	default:
		return &NoOpExtractor{}
	}
}

// ClaudeCodeExtractor extracts attachments from Claude Code transcripts
type ClaudeCodeExtractor struct{}

func (e *ClaudeCodeExtractor) SupportsAttachments() bool {
	return true
}

func (e *ClaudeCodeExtractor) ExtractAttachments(nativeEvent map[string]interface{}) ([]Attachment, string, error) {
	transcriptPath, ok := nativeEvent["transcript_path"].(string)
	if !ok || transcriptPath == "" {
		return nil, "", nil
	}

	// Get the prompt text from the native event to match against transcript
	// This is needed because the transcript may not be written yet when UserPromptSubmit fires
	promptText, _ := nativeEvent["prompt"].(string)

	// Use polling to wait for the transcript to contain the current message
	// This handles the timing issue where the hook fires before transcript is updated
	attachments, _, err := ExtractLatestAttachmentsWithWait(transcriptPath, promptText, 500*time.Millisecond)
	if err != nil {
		return nil, "", err
	}

	return attachments, promptText, nil
}

// CursorExtractor handles attachment extraction for Cursor
type CursorExtractor struct{}

func (e *CursorExtractor) SupportsAttachments() bool {
	// TODO: Update when we understand Cursor's attachment handling
	return false
}

func (e *CursorExtractor) ExtractAttachments(nativeEvent map[string]interface{}) ([]Attachment, string, error) {
	// Cursor hook format may include attachments differently
	// Check if there's an images field in the native event
	if images, ok := nativeEvent["images"].([]interface{}); ok && len(images) > 0 {
		return extractInlineAttachments(images, "image")
	}

	// Check for attachments field
	if attachments, ok := nativeEvent["attachments"].([]interface{}); ok && len(attachments) > 0 {
		return extractInlineAttachments(attachments, "file")
	}

	return nil, "", nil
}

// GeminiExtractor handles attachment extraction for Gemini CLI
type GeminiExtractor struct{}

func (e *GeminiExtractor) SupportsAttachments() bool {
	// TODO: Update when we understand Gemini CLI's attachment handling
	return false
}

func (e *GeminiExtractor) ExtractAttachments(nativeEvent map[string]interface{}) ([]Attachment, string, error) {
	// Gemini CLI may pass attachments in the model_request field
	if modelRequest, ok := nativeEvent["model_request"].(map[string]interface{}); ok {
		if contents, ok := modelRequest["contents"].([]interface{}); ok {
			return extractGeminiContents(contents)
		}
	}

	return nil, "", nil
}

// NoOpExtractor for tools that don't support attachments
type NoOpExtractor struct{}

func (e *NoOpExtractor) SupportsAttachments() bool {
	return false
}

func (e *NoOpExtractor) ExtractAttachments(nativeEvent map[string]interface{}) ([]Attachment, string, error) {
	return nil, "", nil
}

// extractInlineAttachments handles inline base64 attachments from event payloads
func extractInlineAttachments(items []interface{}, defaultType string) ([]Attachment, string, error) {
	var result []Attachment
	counts := make(map[string]int)

	for _, item := range items {
		if itemMap, ok := item.(map[string]interface{}); ok {
			data, _ := itemMap["data"].(string)
			mediaType, _ := itemMap["media_type"].(string)
			if mediaType == "" {
				mediaType, _ = itemMap["mime_type"].(string)
			}
			contentType, _ := itemMap["type"].(string)
			if contentType == "" {
				contentType = defaultType
			}

			if data != "" {
				// Decode base64 data
				decoded, err := base64.StdEncoding.DecodeString(data)
				if err != nil {
					continue
				}

				counts[contentType]++
				ext := getExtensionForMediaType(mediaType)
				filename := fmt.Sprintf("%s_%d%s", contentType, counts[contentType], ext)

				result = append(result, Attachment{
					Data:      decoded,
					MediaType: mediaType,
					Filename:  filename,
					Type:      contentType,
				})
			}
		}
	}
	return result, "", nil
}

// extractGeminiContents extracts attachments from Gemini's content format
func extractGeminiContents(contents []interface{}) ([]Attachment, string, error) {
	var attachments []Attachment
	var textParts []string
	counts := make(map[string]int)

	for _, content := range contents {
		if contentMap, ok := content.(map[string]interface{}); ok {
			if parts, ok := contentMap["parts"].([]interface{}); ok {
				for _, part := range parts {
					if partMap, ok := part.(map[string]interface{}); ok {
						// Check for inline_data (image/document)
						if inlineData, ok := partMap["inline_data"].(map[string]interface{}); ok {
							mimeType, _ := inlineData["mime_type"].(string)
							data, _ := inlineData["data"].(string)
							if data != "" {
								decoded, err := base64.StdEncoding.DecodeString(data)
								if err != nil {
									continue
								}

								// Determine content type from mime type
								contentType := "file"
								if len(mimeType) > 6 && mimeType[:6] == "image/" {
									contentType = "image"
								} else if mimeType == "application/pdf" {
									contentType = "document"
								}

								counts[contentType]++
								ext := getExtensionForMediaType(mimeType)
								filename := fmt.Sprintf("%s_%d%s", contentType, counts[contentType], ext)

								attachments = append(attachments, Attachment{
									Data:      decoded,
									MediaType: mimeType,
									Filename:  filename,
									Type:      contentType,
								})
							}
						}
						// Check for text
						if text, ok := partMap["text"].(string); ok {
							textParts = append(textParts, text)
						}
					}
				}
			}
		}
	}

	promptText := ""
	if len(textParts) > 0 {
		promptText = textParts[len(textParts)-1] // Use last text part
	}

	return attachments, promptText, nil
}
