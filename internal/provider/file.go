package provider

import "strings"

// FileClass is the broad category of a file attachment, used by providers to
// decide how to encode it in their API request.
type FileClass int

const (
	// FileClassText covers plain-text formats (text/*, application/json,
	// application/xml). Data is valid UTF-8 and can be sent as a text source.
	FileClassText FileClass = iota
	// FileClassImage covers image/* types sent as base64.
	FileClassImage
	// FileClassPDF covers application/pdf sent as a base64 document source.
	FileClassPDF
	// FileClassBinary covers everything else. Providers that cannot handle
	// arbitrary binary data should return an error for this class.
	FileClassBinary
)

// ClassifyFile determines the FileClass for a file attachment from its MIME type.
// Any type not explicitly recognised is treated as FileClassBinary; the caller
// is responsible for deciding whether to surface an error to the user.
func ClassifyFile(mimeType string, _ []byte) FileClass {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return FileClassImage
	case mimeType == "application/pdf":
		return FileClassPDF
	case strings.HasPrefix(mimeType, "text/"),
		mimeType == "application/json",
		mimeType == "application/xml":
		return FileClassText
	default:
		return FileClassBinary
	}
}
