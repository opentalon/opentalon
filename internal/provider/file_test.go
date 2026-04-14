package provider

import "testing"

func TestClassifyFile(t *testing.T) {
	tests := []struct {
		name     string
		mimeType string
		data     []byte
		want     FileClass
	}{
		// image/*
		{"png", "image/png", []byte{0x89, 0x50, 0x4e, 0x47}, FileClassImage},
		{"jpeg", "image/jpeg", []byte{0xff, 0xd8}, FileClassImage},
		{"gif", "image/gif", []byte("GIF89a"), FileClassImage},

		// application/pdf
		{"pdf", "application/pdf", []byte("%PDF-1.4"), FileClassPDF},

		// explicit text-like MIME types
		{"text/plain", "text/plain", []byte("hello"), FileClassText},
		{"text/csv", "text/csv", []byte("a,b\n1,2"), FileClassText},
		{"text/html", "text/html", []byte("<p>hi</p>"), FileClassText},
		{"application/json", "application/json", []byte(`{"k":"v"}`), FileClassText},
		{"application/xml", "application/xml", []byte("<r/>"), FileClassText},

		// unrecognised MIME types → binary regardless of content
		{"zip", "application/zip", []byte{0x50, 0x4b, 0x03, 0x04}, FileClassBinary},
		{"octet-stream", "application/octet-stream", []byte("plain text content"), FileClassBinary},
		{"no mime", "", []byte("just text"), FileClassBinary},
		{"audio", "audio/mpeg", nil, FileClassBinary},
		{"video", "video/mp4", nil, FileClassBinary},
		{"docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", nil, FileClassBinary},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyFile(tc.mimeType, tc.data)
			if got != tc.want {
				t.Errorf("ClassifyFile(%q, ...) = %v, want %v", tc.mimeType, got, tc.want)
			}
		})
	}
}
