package main

import (
	"context"
	"encoding/base64"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"
)

const maxInputImageBytes = 8 << 20
const maxInputTextFileBytes = 1 << 20
const maxInputMediaBytes = 16 << 20
const maxRemoteImages = 8

type imageResolver func(string) ([]byte, string, error)

func (s *requestScope) prepareConversation(chat chatRequest, tools []chatTool, route *nativeTransportPool) (chatConversation, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return chatConversation{}, context.Canceled
	}
	s.pending.Add(1)
	s.mu.Unlock()
	defer s.pending.Done()
	ctx, cancel := context.WithTimeout(s.ctx, imagePreparationTimeout)
	defer cancel()
	var fetch imageResolver
	if route != nil {
		images := newPublicImageFetcher(ctx, route)
		defer images.client.CloseIdleConnections()
		fetch = images.fetch
	}
	return conversation(chat, tools, fetch)
}

func imageMIME(value string) bool {
	switch value {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

func decodeBase64Media(value string, limit int) ([]byte, error) {
	if base64.StdEncoding.DecodedLen(len(value)) > limit+2 {
		return nil, failure(413, "Attachment exceeds its decoded byte limit")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || len(data) == 0 {
		return nil, failure(400, "Attachment must contain valid nonempty base64 data")
	}
	if len(data) > limit {
		return nil, failure(413, "Attachment exceeds its decoded byte limit")
	}
	return data, nil
}

func decodeInputImage(value string, fetch imageResolver) ([]byte, string, error) {
	if !strings.HasPrefix(value, "data:") {
		if fetch == nil {
			return nil, "", failure(400, "Remote image URLs require an explicit native transport route; supply an image data URL")
		}
		return fetch(value)
	}
	at := strings.Index(value, ";base64,")
	if at < 5 || !imageMIME(value[5:at]) {
		return nil, "", failure(400, "Image data URL must use PNG, JPEG, GIF, or WebP base64 data")
	}
	data, err := decodeBase64Media(value[at+8:], maxInputImageBytes)
	return data, value[5:at], err
}

func decodeTextFile(file map[string]any) (string, error) {
	if str(file["file_id"]) != "" {
		return "", failure(400, "Cursor cannot resolve file_id; supply base64 UTF-8 file_data")
	}
	encoded, ok := file["file_data"].(string)
	if !ok || encoded == "" {
		return "", failure(400, "File attachments require base64 UTF-8 file_data")
	}
	if strings.HasPrefix(encoded, "data:") {
		at := strings.Index(encoded, ";base64,")
		if at < 5 {
			return "", failure(400, "Invalid textual file data URL")
		}
		mediaType, _, err := mime.ParseMediaType(encoded[5:at])
		if err != nil || !textMIME(mediaType) {
			return "", failure(400, "Cursor accepts textual UTF-8 file attachments only")
		}
		encoded = encoded[at+8:]
	}
	data, err := decodeBase64Media(encoded, maxInputTextFileBytes)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) || !textMIME(strings.Split(http.DetectContentType(data), ";")[0]) {
		return "", failure(400, "Cursor accepts textual UTF-8 file attachments only")
	}
	for _, r := range string(data) {
		if r == 0 || (r < 0x20 && r != '\n' && r != '\r' && r != '\t') {
			return "", failure(400, "File data contains binary control bytes")
		}
	}
	return string(data), nil
}

func textMIME(value string) bool {
	return strings.HasPrefix(value, "text/") || value == "application/json" || value == "application/xml" || value == "application/yaml" || value == "application/x-yaml" || value == "application/toml"
}

func validateTextParts(value any) error {
	if value == nil {
		return nil
	}
	if _, ok := value.(string); ok {
		return nil
	}
	parts, ok := value.([]any)
	if !ok {
		return failure(400, "Message content must be text or a supported content array")
	}
	for _, raw := range parts {
		part := object(raw)
		if str(part["type"]) != "text" {
			return failure(400, "This message role supports text content only during history import")
		}
		if _, ok := part["text"].(string); !ok {
			return failure(400, "Text content must be a string")
		}
	}
	return nil
}
