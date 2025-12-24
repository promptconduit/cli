package transcript

import "fmt"

// ImageExtractor defines the interface for tool-specific image extraction
type ImageExtractor interface {
	// ExtractImages extracts images from the tool's storage format
	// Returns extracted images and the text prompt (if available)
	ExtractImages(nativeEvent map[string]interface{}) ([]Image, string, error)

	// SupportsImages returns whether this tool supports image extraction
	SupportsImages() bool
}

// GetExtractor returns the appropriate image extractor for the given tool
func GetExtractor(tool string) ImageExtractor {
	switch tool {
	case "claude-code":
		return &ClaudeCodeExtractor{}
	case "cursor":
		return &CursorExtractor{}
	case "gemini-cli":
		return &GeminiExtractor{}
	default:
		return &NoOpExtractor{}
	}
}

// ClaudeCodeExtractor extracts images from Claude Code transcripts
type ClaudeCodeExtractor struct{}

func (e *ClaudeCodeExtractor) SupportsImages() bool {
	return true
}

func (e *ClaudeCodeExtractor) ExtractImages(nativeEvent map[string]interface{}) ([]Image, string, error) {
	transcriptPath, ok := nativeEvent["transcript_path"].(string)
	if !ok || transcriptPath == "" {
		return nil, "", nil
	}

	images, err := ExtractLatestImages(transcriptPath)
	if err != nil {
		return nil, "", err
	}

	// Also extract the text prompt
	promptText, _ := ExtractPromptText(transcriptPath)

	return images, promptText, nil
}

// CursorExtractor handles image extraction for Cursor
// Currently Cursor may pass images differently or not at all via hooks
type CursorExtractor struct{}

func (e *CursorExtractor) SupportsImages() bool {
	// TODO: Update when we understand Cursor's image handling
	return false
}

func (e *CursorExtractor) ExtractImages(nativeEvent map[string]interface{}) ([]Image, string, error) {
	// Cursor hook format may include images differently
	// Check if there's an images field in the native event
	if images, ok := nativeEvent["images"].([]interface{}); ok && len(images) > 0 {
		return extractInlineImages(images)
	}

	// Check for attachments field
	if attachments, ok := nativeEvent["attachments"].([]interface{}); ok && len(attachments) > 0 {
		return extractAttachments(attachments)
	}

	return nil, "", nil
}

// GeminiExtractor handles image extraction for Gemini CLI
type GeminiExtractor struct{}

func (e *GeminiExtractor) SupportsImages() bool {
	// TODO: Update when we understand Gemini CLI's image handling
	return false
}

func (e *GeminiExtractor) ExtractImages(nativeEvent map[string]interface{}) ([]Image, string, error) {
	// Gemini CLI may pass images in the model_request field
	if modelRequest, ok := nativeEvent["model_request"].(map[string]interface{}); ok {
		if contents, ok := modelRequest["contents"].([]interface{}); ok {
			return extractGeminiContents(contents)
		}
	}

	return nil, "", nil
}

// NoOpExtractor for tools that don't support images
type NoOpExtractor struct{}

func (e *NoOpExtractor) SupportsImages() bool {
	return false
}

func (e *NoOpExtractor) ExtractImages(nativeEvent map[string]interface{}) ([]Image, string, error) {
	return nil, "", nil
}

// extractInlineImages handles inline base64 images from event payloads
func extractInlineImages(images []interface{}) ([]Image, string, error) {
	var result []Image
	for i, img := range images {
		if imgMap, ok := img.(map[string]interface{}); ok {
			data, _ := imgMap["data"].(string)
			mediaType, _ := imgMap["media_type"].(string)
			if mediaType == "" {
				mediaType, _ = imgMap["type"].(string)
			}
			if data != "" {
				result = append(result, Image{
					Data:      []byte(data), // Would need base64 decode
					MediaType: mediaType,
					Filename:  generateFilename(i, mediaType),
				})
			}
		}
	}
	return result, "", nil
}

// extractAttachments handles attachment references
func extractAttachments(attachments []interface{}) ([]Image, string, error) {
	// Attachments might be file paths or URLs
	// For now, just return empty - we'd need to fetch/read them
	return nil, "", nil
}

// extractGeminiContents extracts images from Gemini's content format
func extractGeminiContents(contents []interface{}) ([]Image, string, error) {
	var images []Image
	var textParts []string

	for _, content := range contents {
		if contentMap, ok := content.(map[string]interface{}); ok {
			if parts, ok := contentMap["parts"].([]interface{}); ok {
				for i, part := range parts {
					if partMap, ok := part.(map[string]interface{}); ok {
						// Check for inline_data (image)
						if inlineData, ok := partMap["inline_data"].(map[string]interface{}); ok {
							mimeType, _ := inlineData["mime_type"].(string)
							data, _ := inlineData["data"].(string)
							if data != "" {
								images = append(images, Image{
									Data:      []byte(data), // Would need base64 decode
									MediaType: mimeType,
									Filename:  generateFilename(i, mimeType),
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

	return images, promptText, nil
}

// generateFilename creates a filename for an extracted image
func generateFilename(index int, mediaType string) string {
	ext := getExtensionForMediaType(mediaType)
	return fmt.Sprintf("image_%d%s", index+1, ext)
}
