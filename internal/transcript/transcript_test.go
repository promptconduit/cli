package transcript

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractLatestImages(t *testing.T) {
	// Create a temporary transcript file with test data
	tmpDir := t.TempDir()
	transcriptPath := filepath.Join(tmpDir, "test_transcript.jsonl")

	// Create a test image (1x1 red PNG)
	testImageBase64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFBQIAX8jx0gAAAABJRU5ErkJggg=="

	// Create test transcript content
	transcriptContent := `{"type":"summary","summary":"Test session","leafUuid":"test-uuid"}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Here is an image"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + testImageBase64 + `"}}]},"uuid":"msg-1","timestamp":"2025-01-01T00:00:00Z"}
`

	if err := os.WriteFile(transcriptPath, []byte(transcriptContent), 0644); err != nil {
		t.Fatalf("Failed to write test transcript: %v", err)
	}

	// Test extraction
	images, err := ExtractLatestImages(transcriptPath)
	if err != nil {
		t.Fatalf("ExtractLatestImages failed: %v", err)
	}

	if len(images) != 1 {
		t.Fatalf("Expected 1 image, got %d", len(images))
	}

	img := images[0]
	if img.MediaType != "image/png" {
		t.Errorf("Expected media_type 'image/png', got '%s'", img.MediaType)
	}

	if img.Filename != "image_1.png" {
		t.Errorf("Expected filename 'image_1.png', got '%s'", img.Filename)
	}

	// Verify the image data matches
	expectedData, _ := base64.StdEncoding.DecodeString(testImageBase64)
	if len(img.Data) != len(expectedData) {
		t.Errorf("Image data length mismatch: expected %d, got %d", len(expectedData), len(img.Data))
	}
}

func TestExtractLatestImages_NoImages(t *testing.T) {
	tmpDir := t.TempDir()
	transcriptPath := filepath.Join(tmpDir, "test_transcript.jsonl")

	// Create transcript with only text
	transcriptContent := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Just text, no images"}]},"uuid":"msg-1","timestamp":"2025-01-01T00:00:00Z"}
`

	if err := os.WriteFile(transcriptPath, []byte(transcriptContent), 0644); err != nil {
		t.Fatalf("Failed to write test transcript: %v", err)
	}

	images, err := ExtractLatestImages(transcriptPath)
	if err != nil {
		t.Fatalf("ExtractLatestImages failed: %v", err)
	}

	if len(images) != 0 {
		t.Errorf("Expected 0 images, got %d", len(images))
	}
}

func TestExtractLatestImages_NonexistentFile(t *testing.T) {
	images, err := ExtractLatestImages("/nonexistent/file.jsonl")
	if err != nil {
		t.Errorf("Expected nil error for nonexistent file, got: %v", err)
	}
	if images != nil {
		t.Errorf("Expected nil images for nonexistent file")
	}
}

func TestExtractLatestImages_EmptyPath(t *testing.T) {
	images, err := ExtractLatestImages("")
	if err != nil {
		t.Errorf("Expected nil error for empty path, got: %v", err)
	}
	if images != nil {
		t.Errorf("Expected nil images for empty path")
	}
}

func TestClaudeCodeExtractor(t *testing.T) {
	tmpDir := t.TempDir()
	transcriptPath := filepath.Join(tmpDir, "test_transcript.jsonl")

	testImageBase64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFBQIAX8jx0gAAAABJRU5ErkJggg=="

	transcriptContent := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Test prompt"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"` + testImageBase64 + `"}}]},"uuid":"msg-1","timestamp":"2025-01-01T00:00:00Z"}
`

	if err := os.WriteFile(transcriptPath, []byte(transcriptContent), 0644); err != nil {
		t.Fatalf("Failed to write test transcript: %v", err)
	}

	extractor := GetExtractor("claude-code")
	if !extractor.SupportsImages() {
		t.Error("ClaudeCodeExtractor should support images")
	}

	nativeEvent := map[string]interface{}{
		"transcript_path": transcriptPath,
		"prompt":          "Test prompt",
	}

	images, promptText, err := extractor.ExtractImages(nativeEvent)
	if err != nil {
		t.Fatalf("ExtractImages failed: %v", err)
	}

	if len(images) != 1 {
		t.Fatalf("Expected 1 image, got %d", len(images))
	}

	if promptText != "Test prompt" {
		t.Errorf("Expected prompt 'Test prompt', got '%s'", promptText)
	}
}

func TestNoOpExtractor(t *testing.T) {
	extractor := GetExtractor("unknown-tool")
	if extractor.SupportsImages() {
		t.Error("NoOpExtractor should not support images")
	}

	images, text, err := extractor.ExtractImages(map[string]interface{}{})
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if images != nil {
		t.Error("Expected nil images from NoOpExtractor")
	}
	if text != "" {
		t.Error("Expected empty text from NoOpExtractor")
	}
}

func TestExtractLatestImages_SkipsToolResults(t *testing.T) {
	// This test verifies that tool_result messages (which also have type="user")
	// are correctly skipped when looking for the last user message with images
	tmpDir := t.TempDir()
	transcriptPath := filepath.Join(tmpDir, "test_transcript.jsonl")

	testImageBase64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFBQIAX8jx0gAAAABJRU5ErkJggg=="

	// Create transcript with:
	// 1. A user message with an image
	// 2. An assistant message with tool_use
	// 3. A tool_result message (which has type="user" but should be skipped)
	transcriptContent := `{"type":"summary","summary":"Test session","leafUuid":"test-uuid"}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Here is an image"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + testImageBase64 + `"}}]},"uuid":"msg-1","timestamp":"2025-01-01T00:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_123","name":"Read","input":{"file_path":"/test.txt"}}]},"uuid":"msg-2","timestamp":"2025-01-01T00:00:01Z"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":"file contents here"}]},"uuid":"msg-3","timestamp":"2025-01-01T00:00:02Z"}
`

	if err := os.WriteFile(transcriptPath, []byte(transcriptContent), 0644); err != nil {
		t.Fatalf("Failed to write test transcript: %v", err)
	}

	// Should still find the image from the first user message
	images, err := ExtractLatestImages(transcriptPath)
	if err != nil {
		t.Fatalf("ExtractLatestImages failed: %v", err)
	}

	if len(images) != 1 {
		t.Fatalf("Expected 1 image (tool_result should be skipped), got %d", len(images))
	}

	img := images[0]
	if img.MediaType != "image/png" {
		t.Errorf("Expected media_type 'image/png', got '%s'", img.MediaType)
	}
}

func TestExtractPromptText_SkipsToolResults(t *testing.T) {
	tmpDir := t.TempDir()
	transcriptPath := filepath.Join(tmpDir, "test_transcript.jsonl")

	// Create transcript with user message followed by tool_result
	transcriptContent := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"My actual prompt"}]},"uuid":"msg-1","timestamp":"2025-01-01T00:00:00Z"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":"tool output"}]},"uuid":"msg-2","timestamp":"2025-01-01T00:00:01Z"}
`

	if err := os.WriteFile(transcriptPath, []byte(transcriptContent), 0644); err != nil {
		t.Fatalf("Failed to write test transcript: %v", err)
	}

	text, err := ExtractPromptText(transcriptPath)
	if err != nil {
		t.Fatalf("ExtractPromptText failed: %v", err)
	}

	if text != "My actual prompt" {
		t.Errorf("Expected 'My actual prompt', got '%s'", text)
	}
}

func TestExtractLatestAttachments_Document(t *testing.T) {
	tmpDir := t.TempDir()
	transcriptPath := filepath.Join(tmpDir, "test_transcript.jsonl")

	// Create a fake PDF content (just for testing, not a real PDF)
	testPDFBase64 := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 fake pdf content"))

	// Create transcript with a document attachment
	transcriptContent := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Here is a PDF"},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + testPDFBase64 + `"}}]},"uuid":"msg-1","timestamp":"2025-01-01T00:00:00Z"}
`

	if err := os.WriteFile(transcriptPath, []byte(transcriptContent), 0644); err != nil {
		t.Fatalf("Failed to write test transcript: %v", err)
	}

	attachments, err := ExtractLatestAttachments(transcriptPath)
	if err != nil {
		t.Fatalf("ExtractLatestAttachments failed: %v", err)
	}

	if len(attachments) != 1 {
		t.Fatalf("Expected 1 attachment, got %d", len(attachments))
	}

	att := attachments[0]
	if att.MediaType != "application/pdf" {
		t.Errorf("Expected media_type 'application/pdf', got '%s'", att.MediaType)
	}

	if att.Type != "document" {
		t.Errorf("Expected type 'document', got '%s'", att.Type)
	}

	if att.Filename != "document_1.pdf" {
		t.Errorf("Expected filename 'document_1.pdf', got '%s'", att.Filename)
	}
}

func TestExtractLatestAttachments_MultipleTypes(t *testing.T) {
	tmpDir := t.TempDir()
	transcriptPath := filepath.Join(tmpDir, "test_transcript.jsonl")

	testImageBase64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFBQIAX8jx0gAAAABJRU5ErkJggg=="
	testPDFBase64 := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 fake pdf"))

	// Create transcript with both image and document
	transcriptContent := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Here are files"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + testImageBase64 + `"}},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + testPDFBase64 + `"}}]},"uuid":"msg-1","timestamp":"2025-01-01T00:00:00Z"}
`

	if err := os.WriteFile(transcriptPath, []byte(transcriptContent), 0644); err != nil {
		t.Fatalf("Failed to write test transcript: %v", err)
	}

	attachments, err := ExtractLatestAttachments(transcriptPath)
	if err != nil {
		t.Fatalf("ExtractLatestAttachments failed: %v", err)
	}

	if len(attachments) != 2 {
		t.Fatalf("Expected 2 attachments, got %d", len(attachments))
	}

	// Check image
	if attachments[0].Type != "image" {
		t.Errorf("Expected first attachment to be image, got '%s'", attachments[0].Type)
	}
	if attachments[0].MediaType != "image/png" {
		t.Errorf("Expected image/png, got '%s'", attachments[0].MediaType)
	}

	// Check document
	if attachments[1].Type != "document" {
		t.Errorf("Expected second attachment to be document, got '%s'", attachments[1].Type)
	}
	if attachments[1].MediaType != "application/pdf" {
		t.Errorf("Expected application/pdf, got '%s'", attachments[1].MediaType)
	}
}
